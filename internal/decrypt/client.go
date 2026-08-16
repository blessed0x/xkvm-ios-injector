// Package decrypt downloads App Store apps by Apple ID (the ipatool /
// PancakeStore flow) so they can be injected and sideloaded. "Decrypt" here
// means the full iTunes Store finance-API dance: GUID generation, the
// configurator bag, Apple ID authentication (including pod resolution), and
// the volumeStoreDownloadProduct purchase endpoint that returns the .ipa URL
// plus the per-version .sinf DRM files. The downloaded IPA is written back
// with iTunesMetadata.plist and SC_Info/ populated, matching PancakeStore's
// output.
//
// The port is faithful to MuffinStoreJailed/Functions/IPATool.swift
// (jailbreakdotparty/PancakeStore): the same endpoints, headers, the
// JSON-body-with-form-header quirk, the pod-follow ("russia fix") and
// trailing-slash ("brazil fix") behaviors. Apple changes this API
// server-side without notice; the bag endpoint exists precisely so the auth
// URL does not need to be hardcoded forever.
//
// Boundary: the binary inside the downloaded IPA stays FairPlay-encrypted.
// That is how the App Store delivers apps; real Mach-O decryption requires a
// jailbroken device. The output here is installable on a device signed in
// with the same Apple ID.
package decrypt

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xscope0/xkvm-ios-injector/internal/plist"
)

// userAgent matches PancakeStore's configurator UA. Apple keys server
// behavior off of it; do not "clean up" this value.
const userAgent = "Configurator/2.17 (Macintosh; OS X 15.2; 24C5089c) AppleWebKit/0620.1.16.11.6"

// bagFallback is used when the bag request fails; the same fallback
// PancakeStore uses.
const bagFallback = "https://auth.itunes.apple.com/auth/v1/native/"

// ErrTwoFactor is returned by Authenticate when Apple reports that the
// account needs two-factor approval (the "Configurator_message" response).
// Approval must happen on a trusted device; retry after it completes.
var ErrTwoFactor = fmt.Errorf("two-factor authentication required: approve the sign-in on a trusted device, then run again")

// jar is a minimal in-memory cookie jar. The real implementation would be
// net/http/cookiejar, but auth persistence needs to serialize the cookies,
// so a plain map of http.Cookie is simpler and sufficient.
type jar struct{ cookies []http.Cookie }

func (j *jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		if c == nil {
			continue
		}
		j.cookies = append(j.cookies, *c)
	}
}

func (j *jar) Cookies(u *url.URL) []*http.Cookie {
	out := make([]*http.Cookie, 0, len(j.cookies))
	for i := range j.cookies {
		c := j.cookies[i]
		out = append(out, &c)
	}
	return out
} // Client talks to the iTunes Store finance API for one Apple ID.
type Client struct {
	AppleID     string
	Password    string
	GUID        string
	Pod         string
	AccountName string

	headers map[string]string
	jar     *jar
	hc      *http.Client

	// bagBase and buyHost are the endpoint seams. Production uses Apple's
	// hosts; tests point them at httptest servers.
	bagBase string
	buyHost string
}

// New returns a Client for the given Apple ID. No network happens until
// Authenticate is called.
func New(appleID, password string) *Client {
	return &Client{
		AppleID:  appleID,
		Password: password,
		GUID:     GenerateGUID(appleID),
		jar:      &jar{},
		hc:       &http.Client{Timeout: 60 * time.Second},
		bagBase:  "https://init.itunes.apple.com",
	}
}

// GenerateGUID derives the stable configurator GUID for an Apple ID. The
// algorithm is PancakeStore's: SHA1 of "CAFEBABE"<appleId>"CAFEBABE", then
// "00" + hex[10:20], upper-cased.
func GenerateGUID(appleID string) string {
	const (
		defaultGUID = "000C2941396B"
		prefixLen   = 2
		seed        = "CAFEBABE"
		hexStart    = 10
	)
	sum := sha1.Sum([]byte(seed + appleID + seed))
	h := hex.EncodeToString(sum[:])
	// The Swift code uppercases; the GUID must match byte-for-byte.
	return strings.ToUpper(defaultGUID[:prefixLen] + h[hexStart:hexStart+len(defaultGUID)-prefixLen])
}

// HasAuth reports whether an authenticated session is loaded.
func (c *Client) HasAuth() bool { return len(c.headers) > 0 }

// bagEndpoint resolves the live authenticate URL from Apple's configurator
// bag. On any failure it returns the hardcoded fallback, exactly like
// PancakeStore.
func (c *Client) bagEndpoint(ctx context.Context) string {
	bagURL := c.bagBase + "/bag.xml?guid=" + url.QueryEscape(c.GUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bagURL, nil)
	if err != nil {
		return bagFallback
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return bagFallback
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || len(body) == 0 {
		return bagFallback
	}
	// The bag is XML wrapping a <plist> section; slice it out and parse it.
	xml := string(body)
	start := strings.Index(xml, "<plist")
	end := strings.Index(xml, "</plist>")
	if start < 0 || end < 0 || end <= start {
		return bagFallback
	}
	bagPlist, err := plist.Decode([]byte(xml[start : end+len("</plist>")]))
	if err != nil {
		return bagFallback
	}
	bag, _ := bagPlist["urlBag"].(map[string]any)
	if endpoint, ok := bag["authenticateAccount"].(string); ok && endpoint != "" {
		return endpoint
	}
	return bagFallback
}

// Authenticate signs into the iTunes Store. On success the session headers
// and cookies are stored on the client and Save() will persist them.
func (c *Client) Authenticate(ctx context.Context) error {
	endpoint := c.bagEndpoint(ctx)
	// "brazil fix": force a trailing slash on the auth URL.
	if !strings.HasSuffix(endpoint, "/") {
		endpoint += "/"
	}

	body, _ := json.Marshal(map[string]string{
		"appleId":  c.AppleID,
		"password": c.Password,
		"guid":     c.GUID,
		"rmp":      "0",
		"why":      "signIn",
	})

	// The response carries a "pod" header naming which buy pod serves this
	// account. If the response has none and the URL changed, follow it
	// ("russia fix") — some regions redirect the auth endpoint.
	var resp *http.Response
	reqURL := endpoint
	for i := 0; i < 5; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", userAgent)
		for _, ck := range c.jar.Cookies(nil) {
			req.AddCookie(ck)
		}
		resp, err = c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("authenticate: %w", err)
		}
		pod := resp.Header.Get("pod")
		landed := resp.Request.URL.String()
		if pod != "" {
			c.Pod = strings.TrimPrefix(pod, "p")
			break
		}
		if landed == reqURL {
			// Same URL, no pod: the request was absorbed by iTunes
			// purgatory. Read the body anyway — it carries the reason.
			defer resp.Body.Close()
			body2, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			msg, err := authMessage(body2)
			if err == nil && strings.Contains(msg, "Configurator_message") {
				return ErrTwoFactor
			}
			return fmt.Errorf("authenticate: no pod header; the sign-in was rejected (Apple may have changed the flow)")
		}
		// Redirected without a pod: follow the new URL.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		reqURL = landed
	}
	if resp == nil {
		return fmt.Errorf("authenticate: no response")
	}
	defer resp.Body.Close()
	authBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("authenticate: read response: %w", err)
	}

	auth, err := plist.Decode(authBody)
	if err != nil {
		return fmt.Errorf("authenticate: response was not a plist: %w", err)
	}

	dsPersonID, _ := auth["dsPersonId"].(string)
	passwordToken, _ := auth["passwordToken"].(string)
	if dsPersonID != "" && passwordToken != "" {
		storefront := resp.Header.Get("x-set-apple-store-front")
		if storefront == "" {
			return fmt.Errorf("authenticate: missing x-set-apple-store-front header")
		}
		var dsid int64
		if dq, ok := auth["download-queue-info"].(map[string]any); ok {
			dsid = asInt(dq["dsid"])
		}
		c.headers = map[string]string{
			"X-Dsid":              fmt.Sprint(dsid),
			"iCloud-Dsid":         fmt.Sprint(dsid),
			"X-Apple-Store-Front": storefront,
			"X-Token":             passwordToken,
		}
		if acct, ok := auth["accountInfo"].(map[string]any); ok {
			if addr, ok := acct["address"].(map[string]any); ok {
				c.AccountName = fmt.Sprint(addr["firstName"]) + " " + fmt.Sprint(addr["lastName"])
			}
		}
		c.jar.SetCookies(nil, resp.Cookies())
		return nil
	}

	msg, err := authMessage(authBody)
	if err == nil && strings.Contains(msg, "Configurator_message") {
		return ErrTwoFactor
	}
	if err == nil && msg != "" {
		return fmt.Errorf("authentication failed: %s", msg)
	}
	return fmt.Errorf("authentication failed: unexpected response")
}

// authMessage extracts customerMessage from an auth plist body.
func authMessage(body []byte) (string, error) {
	d, err := plist.Decode(body)
	if err != nil {
		return "", err
	}
	msg, _ := d["customerMessage"].(string)
	return msg, nil
}

// DownloadInfo is the buy endpoint's answer for one app version.
type DownloadInfo struct {
	URL      string
	Version  string
	Sinfs    [][]byte // raw .sinf blobs, in SinfPaths order
	Metadata map[string]any
}

// purchase requests volumeStoreDownloadProduct for the given adam id. When
// versionID is empty the response reports the app's available versions
// instead of returning a download URL.
func (c *Client) purchase(ctx context.Context, appID int64, versionID string) (map[string]any, error) {
	reqBody := map[string]any{
		"creditDisplay": "",
		"guid":          c.GUID,
		"salableAdamId": fmt.Sprint(appID),
	}
	if versionID != "" {
		reqBody["externalVersionId"] = versionID
	}
	encoded, _ := json.Marshal(reqBody)

	buyHost := c.buyHost
	if buyHost == "" {
		buyHost = "https://p" + c.Pod + "-buy.itunes.apple.com"
	}
	urlStr := fmt.Sprintf("%s/WebObjects/MZFinance.woa/wa/volumeStoreDownloadProduct?guid=%s",
		buyHost, url.QueryEscape(c.GUID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	for _, ck := range c.jar.Cookies(nil) {
		req.AddCookie(ck)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("volumeStoreDownloadProduct: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("volumeStoreDownloadProduct: read: %w", err)
	}
	out, err := plist.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("volumeStoreDownloadProduct: response was not a plist: %w", err)
	}
	if _, cancelled := out["cancel-purchase-batch"]; cancelled {
		msg, _ := out["customerMessage"].(string)
		return nil, fmt.Errorf("app download failed: %s", msg)
	}
	return out, nil
}

// Purchase gets the download URL and sinfs for one app version.
func (c *Client) Purchase(ctx context.Context, appID int64, versionID string) (DownloadInfo, error) {
	out, err := c.purchase(ctx, appID, versionID)
	if err != nil {
		return DownloadInfo{}, err
	}
	songList, _ := out["songList"].([]any)
	if len(songList) == 0 {
		return DownloadInfo{}, fmt.Errorf("no download info returned for app %d", appID)
	}
	first, _ := songList[0].(map[string]any)
	info := DownloadInfo{Metadata: first}
	info.URL, _ = first["URL"].(string)
	if info.URL == "" {
		return DownloadInfo{}, fmt.Errorf("no download URL returned for app %d", appID)
	}
	if meta, ok := first["metadata"].(map[string]any); ok {
		info.Metadata = meta
		info.Version, _ = meta["bundleVersion"].(string)
	}
	if sinfList, ok := first["sinfs"].([]any); ok {
		for _, s := range sinfList {
			if m, ok := s.(map[string]any); ok {
				if b, ok := m["sinf"].([]byte); ok {
					info.Sinfs = append(info.Sinfs, b)
				}
			}
		}
	}
	return info, nil
}

// VersionIDs lists the external version ids available for an app (the
// downgrade menu). Matches PancakeStore's getVersionIDList.
func (c *Client) VersionIDs(ctx context.Context, appID int64) ([]int64, error) {
	out, err := c.purchase(ctx, appID, "")
	if err != nil {
		return nil, err
	}
	songList, _ := out["songList"].([]any)
	if len(songList) == 0 {
		return nil, fmt.Errorf("no version list returned for app %d", appID)
	}
	first, _ := songList[0].(map[string]any)
	meta, _ := first["metadata"].(map[string]any)
	raw, _ := meta["softwareVersionExternalIdentifiers"].([]any)
	ids := make([]int64, 0, len(raw))
	for _, v := range raw {
		ids = append(ids, asInt(v))
	}
	return ids, nil
}

// DownloadToPath fetches the CDN .ipa to dest. The URL from the buy
// endpoint is a plain zip; redirects are followed automatically.
func (c *Client) DownloadToPath(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	dl := &http.Client{Timeout: 15 * time.Minute}
	resp, err := dl.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	return writeFileAtomic(dest, resp.Body)
}

// asInt coerces plist number values to int64 regardless of how howett
// decoded them.
func asInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
