package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// canisterBase is the Canister jailbreak index API base. Verified live
// 2026-08-13: api.canister.me/v2 redirects (308) here, and the response
// shapes below were captured from a live lookup of ws.hbang.alderis.
// It is a var (not a const) so tests can point it at an httptest server.
var canisterBase = "https://api.tale.me/v4/canister-services/jailbreak"

type canisterPkgResp struct {
	Data []canisterPkg `json:"data"`
}

type canisterPkg struct {
	PackageID    string `json:"package_id"`
	RepositoryID string `json:"repository_id"`
	Version      string `json:"version"`
	PackageFile  string `json:"package_filename"`
	Price        string `json:"price"`
	Visible      bool   `json:"visible"`
	Latest       bool   `json:"latest_version"`
	Quality      int    `json:"quality"`
	SHA256       string `json:"sha256_hash"`
}

type canisterRepoResp struct {
	Data canisterRepo `json:"data"`
}

type canisterRepo struct {
	ID  string `json:"id"`
	URI string `json:"uri"`
}

// canisterLookup resolves a package id to its best free download, returning
// the hosting repository id, the .deb path relative to that repo's root, an
// optional sha256, and the package version (used for cache naming). Entries
// are filtered to visible + free (paid packages can't be sideloaded this
// way); among those, the latest-version entry with the highest quality wins.
func canisterLookup(ctx context.Context, client *http.Client, id string) (repoID, pkgFile, sha, version string, err error) {
	body, err := getJSON(ctx, client, canisterBase+"/package/"+id)
	if err != nil {
		return "", "", "", "", err
	}
	var resp canisterPkgResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", "", "", fmt.Errorf("canister: %w", err)
	}
	var best *canisterPkg
	for i := range resp.Data {
		p := &resp.Data[i]
		if !p.Visible || p.Price != "Free" {
			continue
		}
		if best == nil || better(p, best) {
			best = p
		}
	}
	if best == nil {
		return "", "", "", "", fmt.Errorf("no free download found for %s", id)
	}
	return best.RepositoryID, best.PackageFile, best.SHA256, best.Version, nil
}

// better orders two Canister entries: latest version wins, then higher
// quality, then first-seen.
func better(a, b *canisterPkg) bool {
	if a.Latest != b.Latest {
		return a.Latest
	}
	return a.Quality > b.Quality
}

// canisterVersions returns every visible free package entry for id —
// deduplicated by version, latest-flagged entries first, otherwise in API
// order (newest first). Used by the TUI's version picker and by pinned
// downloads (--version equivalent for the menu).
func canisterVersions(ctx context.Context, client *http.Client, id string) ([]canisterPkg, error) {
	body, err := getJSON(ctx, client, canisterBase+"/package/"+id)
	if err != nil {
		return nil, err
	}
	var resp canisterPkgResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("canister: %w", err)
	}
	var out, rest []canisterPkg
	seen := map[string]bool{}
	for i := range resp.Data {
		p := resp.Data[i]
		if !p.Visible || p.Price != "Free" || seen[p.Version] {
			continue
		}
		seen[p.Version] = true
		if p.Latest {
			out = append(out, p)
		} else {
			rest = append(rest, p)
		}
	}
	out = append(out, rest...)
	if len(out) == 0 {
		return nil, fmt.Errorf("no free download found for %s", id)
	}
	return out, nil
}

// canisterRepoURI resolves a repository id to its base URI.
func canisterRepoURI(ctx context.Context, client *http.Client, repoID string) (string, error) {
	body, err := getJSON(ctx, client, canisterBase+"/repository/"+repoID)
	if err != nil {
		return "", err
	}
	var resp canisterRepoResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("canister: %w", err)
	}
	if resp.Data.URI == "" {
		return "", fmt.Errorf("canister: no URI for repository %s", repoID)
	}
	return resp.Data.URI, nil
}

func getJSON(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("canister %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("canister %s: HTTP %d", url, resp.StatusCode)
	}
	return readLimited(resp.Body, 8<<20)
}
