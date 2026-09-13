package decrypt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// itunesLookupBase is the iTunes Search API host; a var so tests can point
// it at a fake.
var itunesLookupBase = "https://itunes.apple.com"

// ResolveAppID turns any of the accepted inputs into a numeric App Store
// adam id:
//
//   - a bare numeric id:            "310633997" -> 310633997
//   - an App Store URL:             https://apps.apple.com/us/app/x/id310633997 -> 310633997
//   - a bundle id (com.x.y):        looked up via the iTunes Search API
//
// Bundle-id lookup requires network; the other two do not.
func ResolveAppID(ctx context.Context, input string) (int64, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, fmt.Errorf("empty app id")
	}
	if n, err := strconv.ParseInt(input, 10, 64); err == nil && n > 0 {
		return n, nil
	}
	if u, err := url.Parse(input); err == nil && strings.Contains(u.Host, "apps.apple.com") {
		// The id is the trailing path segment: /us/app/name/id123456.
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		last := segments[len(segments)-1]
		if strings.HasPrefix(last, "id") {
			if n, err := strconv.ParseInt(strings.TrimPrefix(last, "id"), 10, 64); err == nil {
				return n, nil
			}
		}
		return 0, fmt.Errorf("couldn't find an app id in %q", input)
	}
	// Fall back to a bundle-id lookup.
	lookup := itunesLookupBase + "/lookup?bundleId=" + url.QueryEscape(input)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookup, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("lookup %q: %w", input, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("lookup %q: HTTP %d", input, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	var out struct {
		Results []struct {
			TrackID int64 `json:"trackId"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("lookup %q: bad response: %w", input, err)
	}
	if len(out.Results) == 0 {
		return 0, fmt.Errorf("no app found for bundle id %q", input)
	}
	return out.Results[0].TrackID, nil
}
