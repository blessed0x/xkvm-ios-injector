package fetch

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blessed0x/xkvm-ios-injector/internal/log"
)

// defaultClient is used when Resolve is called without a client.
var defaultClient = &http.Client{Timeout: 60 * time.Second}

// Resolve fetches the given package ids — plus their Depends: closures
// unless noRecurse — as .debs into cacheDir, and returns the local paths in
// resolution order, deduplicated by package id. sources (from --apt-source)
// are searched before the Canister index; when an id isn't in any listed
// source, Canister resolves which repo hosts it.
func Resolve(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, client *http.Client) ([]string, error) {
	return ResolveVersion(ctx, ids, sources, noRecurse, cacheDir, client, nil)
}

// ResolveVersion is Resolve with optional Canister version pins: when
// pinned[id] is non-empty that exact version is downloaded for id instead of
// the default latest. Dependencies are never pinned — only the ids named in
// the map. Callers without pins can just use Resolve.
func ResolveVersion(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, client *http.Client, pinned map[string]string) ([]string, error) {
	if client == nil {
		client = defaultClient
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	// Lazy 7-day TTL: drop stale entries before anything is resolved, so the
	// cache never grows without bound and old versions retire on their own.
	// Only the standard persistent cache is pruned — a caller-supplied
	// folder (the TUI's download destination) is the user's own files, and
	// silently deleting them on a later run would be a surprise.
	if isDefaultCacheDir(cacheDir) {
		pruneCache(cacheDir, time.Now())
	}
	r := &resolver{
		client:    client,
		cache:     cacheDir,
		sources:   sources,
		noRecurse: noRecurse,
		pinned:    pinned,
		indexes:   map[string][]Entry{},
	}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := r.walk(ctx, id, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type resolver struct {
	client    *http.Client
	cache     string
	sources   []string
	noRecurse bool
	pinned    map[string]string // package id -> must-have Canister version

	visited map[string]bool    // package ids already resolved (cycle guard)
	indexes map[string][]Entry // repo base URI -> parsed Packages index
}

// walk resolves one package id (its deb plus, unless disabled, its
// dependencies) and appends the local deb paths to out.
func (r *resolver) walk(ctx context.Context, id string, out *[]string) error {
	if r.visited == nil {
		r.visited = map[string]bool{}
	}
	if r.visited[id] {
		return nil
	}
	r.visited[id] = true

	repo, file, depends, sha, version, err := r.locate(ctx, id)
	if err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("no download found for %s", id)
	}
	path, err := r.download(ctx, id, repo, file, sha, version)
	if err != nil {
		return err
	}
	*out = append(*out, path)

	if r.noRecurse {
		return nil
	}
	for _, group := range parseDepends(depends) {
		// If any non-core alternative of this group was already resolved,
		// the dependency is satisfied — don't fetch a redundant member.
		var pick string
		for _, alt := range group {
			if !isCoreDep(alt) && r.visited[alt] {
				pick = ""
				break
			}
			if pick == "" && !isCoreDep(alt) {
				pick = alt
			}
		}
		if pick == "" {
			continue // group satisfied or all alternatives are core
		}
		if err := r.walk(ctx, pick, out); err != nil {
			return err
		}
	}
	return nil
}

// locate finds where id lives. Explicit sources are checked first (a direct
// Packages-index match); otherwise Canister resolves the hosting repo and the
// repo's index is fetched; a package neither knows about triggers the smart
// fallback sweep over the built-in default repo list (the active-repos set,
// repos.go). It returns the repo base URI, the relative .deb path, the
// Depends: closure (used for recursion), an optional sha256, and the
// package version (used for cache naming).
func (r *resolver) locate(ctx context.Context, id string) (repo, file, depends, sha, version string, err error) {
	for _, src := range r.sources {
		entries, ierr := r.indexFor(ctx, src)
		if ierr != nil {
			continue // a listed repo that's down doesn't block the others
		}
		if e := findEntry(entries, id); e != nil {
			return strings.TrimSuffix(src, "/"), e.Filename, e.Depends, e.SHA256, e.Version, nil
		}
	}

	repoID, pkgFile, pkgSHA, pkgVer, cerr := canisterLookup(ctx, r.client, id)
	if cerr == nil && r.pinned[id] != "" && r.pinned[id] != pkgVer {
		// A pinned version was requested and the default (latest) isn't it:
		// pick the matching entry. Unknown pins fail with the list of what
		// actually exists so the caller can show it.
		vs, verr := canisterVersions(ctx, r.client, id)
		if verr != nil {
			return "", "", "", "", "", verr
		}
		repoID, pkgFile, pkgSHA, pkgVer = "", "", "", ""
		for i := range vs {
			if vs[i].Version == r.pinned[id] {
				repoID, pkgFile, pkgSHA, pkgVer = vs[i].RepositoryID, vs[i].PackageFile, vs[i].SHA256, vs[i].Version
				break
			}
		}
		if repoID == "" {
			have := make([]string, 0, len(vs))
			for i := range vs {
				have = append(have, vs[i].Version)
			}
			return "", "", "", "", "", fmt.Errorf("version %q of %s isn't available (have: %s)", r.pinned[id], id, strings.Join(have, ", "))
		}
	}
	if cerr != nil {
		// Smart dependency fallback: sweep the default repo list for a
		// Packages-index hit. Repos already consulted this run are skipped
		// via the shared index cache; a down repo just moves the sweep on.
		// The whole sweep shares one deadline (fallbackSweepTimeout) so a
		// dead id can't stall the resolve on a wall of hanging indexes.
		sweepCtx, cancel := context.WithTimeout(ctx, fallbackSweepTimeout)
		defer cancel()
		for _, repo := range defaultRepos {
			if _, done := r.indexes[repo]; done {
				continue
			}
			entries, ierr := r.indexFor(sweepCtx, repo)
			if ierr != nil {
				continue
			}
			if e := findEntry(entries, id); e != nil {
				return strings.TrimSuffix(repo, "/"), e.Filename, e.Depends, e.SHA256, e.Version, nil
			}
		}
		return "", "", "", "", "", fmt.Errorf("couldn't locate %s: %w", id, cerr)
	}
	uri, uerr := canisterRepoURI(ctx, r.client, repoID)
	if uerr != nil {
		return "", "", "", "", "", uerr
	}
	// The repo index is authoritative for Depends and Filename when
	// present; fall back to the Canister-reported path otherwise.
	if entries, ierr := r.indexFor(ctx, uri); ierr == nil {
		if e := findEntry(entries, id); e != nil {
			if e.Filename != "" {
				return uri, e.Filename, e.Depends, e.SHA256, e.Version, nil
			}
			return uri, pkgFile, e.Depends, cmp.Or(e.SHA256, pkgSHA), cmp.Or(e.Version, pkgVer), nil
		}
	}
	return uri, pkgFile, "", pkgSHA, pkgVer, nil
}

func (r *resolver) indexFor(ctx context.Context, repo string) ([]Entry, error) {
	if entries, ok := r.indexes[repo]; ok {
		return entries, nil
	}
	entries, err := fetchIndex(ctx, r.client, repo)
	if err != nil {
		return nil, err
	}
	r.indexes[repo] = entries
	return entries, nil
}

// download fetches repo/file into the cache as a versioned <id>__<ver>.deb,
// verifying the sha256 when one is available. A cached copy of the exact
// version whose sha checks out is reused instead of downloading again — the
// smart-dependency cache: the same dependency needed twice is served from
// disk the second time (and on any later run inside the 7-day TTL).
func (r *resolver) download(ctx context.Context, id, repo, file, sha, version string) (string, error) {
	if !validPackageID(id) {
		return "", fmt.Errorf("refusing unsafe package id %q", id)
	}
	path := filepath.Join(r.cache, cacheFileName(id, version))
	if data, err := os.ReadFile(path); err == nil {
		if sha == "" || hex.EncodeToString(sha256sum(data)) == sha {
			log.Infof("using cached %s", path)
			return path, nil
		}
		log.Infof("cache miss (sha changed): re-downloading %s", id)
	}

	url := strings.TrimSuffix(repo, "/") + "/" + file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Havoc (and repos like it) block direct downloads of PAID packages
		// with 418 + a "must be downloaded through a package manager" body.
		// Say that instead of a bare status code: the fix is buying it once
		// in Sileo/Zebra and exporting the deb, not retrying.
		if resp.StatusCode == http.StatusTeapot || resp.StatusCode == http.StatusPaymentRequired {
			bodyHint, _ := readLimited(resp.Body, 4<<10)
			if strings.Contains(strings.ToLower(string(bodyHint)), "paid") {
				return "", fmt.Errorf("downloading %s: %s is a PAID package on %s — buy it once in Sileo/Zebra on a jailbroken device and export the .deb from there", url, id, repoHost(repo))
			}
		}
		return "", fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 1<<30) // 1 GiB cap; errors on overflow
	if err != nil {
		return "", err
	}
	if sha != "" {
		if hex.EncodeToString(sha256sum(body)) != sha {
			return "", fmt.Errorf("sha256 mismatch for %s", id)
		}
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// repoHost extracts the printable host from a repo base URL for error text.
func repoHost(repo string) string {
	if u, err := url.Parse(strings.TrimSuffix(repo, "/")); err == nil && u.Host != "" {
		return u.Host
	}
	return repo
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// isDefaultCacheDir reports whether dir is the standard persistent cache
// directory (CacheDir). Custom cache dirs are treated as user-owned folders
// and exempt from the automatic 7-day prune.
func isDefaultCacheDir(dir string) bool {
	def, err := CacheDir()
	if err != nil {
		return false
	}
	return filepath.Clean(dir) == filepath.Clean(def)
}

// validPackageID reports whether id is safe to use as a cache file name.
// Depends: values come from third-party repo indexes, so an id must never be
// able to escape the cache directory (path traversal) or collide with
// special names.
func validPackageID(id string) bool {
	// filepath.Base("..") is "..", so the bare dot-dot must be rejected
	// explicitly or the cache escape it guards against is back.
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return false
	}
	return id == filepath.Base(id)
}
