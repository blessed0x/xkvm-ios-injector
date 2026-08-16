package decrypt

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authPlist returns a successful authenticate response body.
func authPlist(dsid string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>dsPersonId</key><string>p1</string>
<key>passwordToken</key><string>tok-abc</string>
<key>download-queue-info</key><dict><key>dsid</key><integer>` + dsid + `</integer></dict>
<key>accountInfo</key><dict><key>address</key><dict>
<key>firstName</key><string>Jane</string><key>lastName</key><string>Doe</string>
</dict></dict>
</dict></plist>`)
}

func twoFactorPlist() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>customerMessage</key><string>MZFinance.Configurator_message.signIn</string>
</dict></plist>`)
}

func TestGenerateGUID(t *testing.T) {
	// Golden pins, computed from the Swift algorithm.
	if got := GenerateGUID("test@example.com"); got != "0097999D59A9" {
		t.Fatalf("GUID(test@example.com) = %q, want 0097999D59A9", got)
	}
	if got := GenerateGUID("other@example.com"); got != "00EFBC588203" {
		t.Fatalf("GUID(other@example.com) = %q, want 00EFBC588203", got)
	}
	// Deterministic, and distinct ids give distinct GUIDs.
	twice, once := GenerateGUID("a@b.c"), GenerateGUID("a@b.c")
	if twice != once {
		t.Fatal("GUID not deterministic")
	}
	if GenerateGUID("a@b.c") == GenerateGUID("d@e.f") {
		t.Fatal("distinct apple ids produced the same GUID")
	}
}

// authStack wires the bag + authenticate endpoints. The returned handlers let
// the test assert what was received (pod header, trailing slash, redirects).
func authStack(t *testing.T, authHandler http.HandlerFunc) (*Client, *httptest.Server, *httptest.Server) {
	t.Helper()
	authSrv := httptest.NewServer(authHandler)
	t.Cleanup(authSrv.Close)
	bagSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?><plist version="1.0"><dict><key>urlBag</key><dict>`+
			`<key>authenticateAccount</key><string>%s</string>`+
			`</dict></dict></plist>`, authSrv.URL)
	}))
	t.Cleanup(bagSrv.Close)
	c := New("test@example.com", "hunter2")
	c.bagBase = bagSrv.URL
	return c, bagSrv, authSrv
}

func TestAuthenticateSuccess(t *testing.T) {
	var gotSlash bool
	c, _, _ := authStack(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/") {
			t.Errorf("auth path %q does not end with / (brazil fix)", r.URL.Path)
			gotSlash = false
		} else {
			gotSlash = true
		}
		w.Header().Set("pod", "42")
		w.Header().Set("x-set-apple-store-front", "143441")
		w.Header().Set("Content-Type", "text/xml")
		w.Write(authPlist("98765"))
	})
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !gotSlash {
		t.Fatal("brazil fix failed: request path did not get a trailing slash")
	}
	if c.Pod != "42" {
		t.Errorf("Pod = %q, want 42", c.Pod)
	}
	if c.AccountName != "Jane Doe" {
		t.Errorf("AccountName = %q, want Jane Doe", c.AccountName)
	}
	if c.headers["X-Dsid"] != "98765" || c.headers["iCloud-Dsid"] != "98765" {
		t.Errorf("dsid headers = %v, want 98765", c.headers)
	}
	if c.headers["X-Token"] != "tok-abc" {
		t.Errorf("X-Token = %q, want tok-abc", c.headers["X-Token"])
	}
	if c.headers["X-Apple-Store-Front"] != "143441" {
		t.Errorf("store front = %q, want 143441", c.headers["X-Apple-Store-Front"])
	}
	if !c.HasAuth() {
		t.Fatal("HasAuth() false after successful authenticate")
	}
}

func TestAuthenticateTwoFactor(t *testing.T) {
	c, _, _ := authStack(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("pod", "42")
		w.Write(twoFactorPlist())
	})
	err := c.Authenticate(context.Background())
	if !errors.Is(err, ErrTwoFactor) {
		t.Fatalf("Authenticate with 2FA response: err = %v, want ErrTwoFactor", err)
	}
	if c.HasAuth() {
		t.Fatal("HasAuth() true after a failed 2FA authenticate")
	}
}

func TestAuthenticatePodRedirect(t *testing.T) {
	// The "russia fix": the auth endpoint 302s to another pod without a pod
	// header; the client must follow and read the pod from the final landing.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("pod", "59")
		w.Header().Set("x-set-apple-store-front", "143441")
		w.Write(authPlist("1"))
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	c := New("test@example.com", "hunter2")
	c.bagBase = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<plist version="1.0"><dict><key>urlBag</key><dict><key>authenticateAccount</key><string>%s</string></dict></dict></plist>`, redirector.URL)
	})).URL
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if c.Pod != "59" {
		t.Errorf("Pod = %q, want 59 (redirect followed)", c.Pod)
	}
}

func TestAuthenticateRejected(t *testing.T) {
	c, _, _ := authStack(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("pod", "42")
		w.Write([]byte(`<plist version="1.0"><dict><key>customerMessage</key><string>bad credentials</string></dict></plist>`))
	})
	err := c.Authenticate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad credentials") {
		t.Fatalf("Authenticate rejected: err = %v, want customerMessage surfaced", err)
	}
}

func buyPlist(url string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>songList</key><array><dict>
<key>URL</key><string>` + url + `</string>
<key>metadata</key><dict>
<key>bundleVersion</key><string>9.3.4</string>
<key>softwareVersionExternalIdentifiers</key><array><integer>820000001</integer><integer>820000000</integer></array>
</dict>
<key>sinfs</key><array><dict><key>sinf</key><data>U0lORg==</data></dict></array>
</dict></array>
</dict></plist>`)
}

func TestPurchase(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("zipdata"))
	}))
	defer cdn.Close()
	buySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Dsid"); got != "98765" {
			t.Errorf("X-Dsid header = %q, want 98765", got)
		}
		if got := r.Header.Get("X-Token"); got != "tok-abc" {
			t.Errorf("X-Token header = %q, want tok-abc", got)
		}
		w.Write(buyPlist(cdn.URL + "/app.zip"))
	}))
	defer buySrv.Close()

	c := New("test@example.com", "hunter2")
	c.headers = map[string]string{
		"X-Dsid": "98765", "iCloud-Dsid": "98765",
		"X-Apple-Store-Front": "143441", "X-Token": "tok-abc",
	}
	c.Pod = "42"
	c.buyHost = buySrv.URL

	info, err := c.Purchase(context.Background(), 310633997, "")
	if err != nil {
		t.Fatalf("Purchase: %v", err)
	}
	if info.URL != cdn.URL+"/app.zip" {
		t.Errorf("URL = %q, want %s/app.zip", info.URL, cdn.URL)
	}
	if info.Version != "9.3.4" {
		t.Errorf("Version = %q, want 9.3.4", info.Version)
	}
	if len(info.Sinfs) != 1 || !bytes.Equal(info.Sinfs[0], []byte("SINF")) {
		t.Errorf("Sinfs = %d blobs, want one containing SINF", len(info.Sinfs))
	}

	ids, err := c.VersionIDs(context.Background(), 310633997)
	if err != nil {
		t.Fatalf("VersionIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != 820000001 || ids[1] != 820000000 {
		t.Errorf("VersionIDs = %v, want [820000001 820000000]", ids)
	}
}

// fakeIPA builds a minimal app zip with the SC_Info manifest shape Apple
// ships: an app with Info.plist, a dummy executable, a manifest listing one
// sinf path, and a (stale) iTunesMetadata.plist that must be replaced.
func fakeIPA(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	add("Payload/TestApp.app/Info.plist", `<?xml version="1.0"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.test</string>
<key>CFBundleExecutable</key><string>TestApp</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
</dict></plist>`)
	add("Payload/TestApp.app/TestApp", "\xcf\xfa\xed\xfe-dummy")
	add("Payload/TestApp.app/SC_Info/Manifest.plist", `<?xml version="1.0"?><plist version="1.0"><dict>
<key>SinfPaths</key><array><string>SC_Info/TestApp.sinF</string></array>
</dict></plist>`)
	add("Payload/TestApp.app/SC_Info/TestApp.sinF", "old-sinf")
	add("iTunesMetadata.plist", `<plist version="1.0"><dict><key>stale</key><true/></dict></plist>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadEndToEnd(t *testing.T) {
	zipData := fakeIPA(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipData)
	}))
	defer cdn.Close()
	buySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buyPlist(cdn.URL + "/app.zip"))
	}))
	defer buySrv.Close()
	c, _, _ := authStack(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("pod", "42")
		w.Header().Set("x-set-apple-store-front", "143441")
		w.Write(authPlist("98765"))
	})
	if err := c.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	c.buyHost = buySrv.URL

	outDir := t.TempDir()
	path, err := c.Download(context.Background(), 310633997, "", outDir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(path) != "com.example.test_1.0.ipa" {
		t.Errorf("output name = %q, want com.example.test_1.0.ipa", filepath.Base(path))
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer zr.Close()
	entries := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		entries[f.Name] = b.String()
	}
	if _, ok := entries["Payload/TestApp.app/TestApp"]; !ok {
		t.Error("original executable entry missing from output")
	}
	if got := entries["Payload/TestApp.app/SC_Info/TestApp.sinF"]; got != "SINF" {
		t.Errorf("sinf content = %q, want SINF (written from buy response)", got)
	}
	meta, ok := entries["iTunesMetadata.plist"]
	if !ok {
		t.Fatal("iTunesMetadata.plist missing from output")
	}
	if !strings.Contains(meta, "test@example.com") {
		t.Errorf("iTunesMetadata.plist does not carry the apple id:\n%s", meta)
	}
	if strings.Contains(meta, "stale") {
		t.Error("stale iTunesMetadata.plist was not replaced")
	}
}

func TestDownloadRequiresAuth(t *testing.T) {
	c := New("", "")
	_, err := c.Download(context.Background(), 1, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("Download without auth: err = %v, want not-signed-in", err)
	}
}

func TestResolveAppID(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		in   string
		want int64
	}{
		{"310633997", 310633997},
		{"https://apps.apple.com/us/app/example/id310633997", 310633997},
		{"https://apps.apple.com/de/app/xyz/id123456789", 123456789},
	}
	for _, tc := range cases {
		got, err := ResolveAppID(ctx, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ResolveAppID(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}

	// Bundle-id lookup through the iTunes Search API.
	lookup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bundleId") != "com.example.test" {
			t.Errorf("lookup query = %v, want bundleId=com.example.test", r.URL.Query())
		}
		w.Write([]byte(`{"results":[{"trackId":555555}]}`))
	}))
	defer lookup.Close()
	old := itunesLookupBase
	itunesLookupBase = lookup.URL
	defer func() { itunesLookupBase = old }()

	got, err := ResolveAppID(ctx, "com.example.test")
	if err != nil || got != 555555 {
		t.Errorf("ResolveAppID(bundle id) = %d, %v; want 555555", got, err)
	}

	// Empty results are an error, not a zero id.
	lookup2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[]}`))
	}))
	defer lookup2.Close()
	itunesLookupBase = lookup2.URL
	if _, err := ResolveAppID(ctx, "com.example.none"); err == nil {
		t.Error("empty lookup results should error")
	}
}

func TestIpaNameSanitize(t *testing.T) {
	if got := ipaName(map[string]any{"CFBundleIdentifier": "com/x:y", "CFBundleShortVersionString": "1.0"}); got != "com_x_y_1.0.ipa" {
		t.Errorf("ipaName = %q, want com_x_y_1.0.ipa", got)
	}
	if got := ipaName(map[string]any{"bundleId": "com.fb", "bundleVersion": "2.0"}); got != "com.fb_2.0.ipa" {
		t.Errorf("ipaName (metadata fallback) = %q, want com.fb_2.0.ipa", got)
	}
	if got := ipaName(map[string]any{}); got != "app.ipa" {
		t.Errorf("ipaName (empty) = %q, want app.ipa", got)
	}
}

// A manifest that exists but carries no SinfPaths must fall through to the
// executable fallback instead of panicking on a nil assertion.
func TestInspectZipManifestWithoutSinfPaths(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Payload/TestApp.app/Info.plist")
	w.Write([]byte(`<plist version="1.0"><dict><key>CFBundleExecutable</key><string>TestApp</string></dict></plist>`))
	w, _ = zw.Create("Payload/TestApp.app/SC_Info/Manifest.plist")
	w.Write([]byte(`<plist version="1.0"><dict></dict></plist>`))
	zw.Close()

	f, err := os.CreateTemp(t.TempDir(), "nomanifest-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(buf.Bytes())
	f.Close()

	paths, _, err := inspectZip(f.Name())
	if err != nil {
		t.Fatalf("inspectZip: %v", err)
	}
	if len(paths) != 1 || paths[0] != "Payload/TestApp.app/SC_Info/TestApp.sinf" {
		t.Errorf("fallback sinf path = %v, want executable-named fallback", paths)
	}
}

// verify the download helper handles a manifest-less (old app) fallback path.
func TestInspectZipNoManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Payload/TestApp.app/Info.plist")
	w.Write([]byte(`<plist version="1.0"><dict><key>CFBundleExecutable</key><string>OldApp</string></dict></plist>`))
	zw.Close()

	f, err := os.CreateTemp(t.TempDir(), "old-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(buf.Bytes())
	f.Close()

	paths, meta, err := inspectZip(f.Name())
	if err != nil {
		t.Fatalf("inspectZip: %v", err)
	}
	if len(paths) != 1 || paths[0] != "Payload/TestApp.app/SC_Info/OldApp.sinf" {
		t.Errorf("fallback sinf path = %v, want Payload/TestApp.app/SC_Info/OldApp.sinf", paths)
	}
	if meta["CFBundleExecutable"] != "OldApp" {
		t.Errorf("meta = %v, want CFBundleExecutable OldApp", meta)
	}
}
