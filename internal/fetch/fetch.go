package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultClient is used when Resolve is called without a client.
var defaultClient = &http.Client{Timeout: 60 * time.Second}

// Resolve fetches the given package ids — plus their Depends: closures
// unless noRecurse — as .debs into cacheDir, and returns the local paths in
// resolution order, deduplicated by package id. sources (from --apt-source)
// are searched before the Canister index; when an id isn't in any listed
// source, Canister resolves which repo hosts it.
func Resolve(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, client *http.Client) ([]string, error) {
	if client == nil {
		client = defaultClient
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	r := &resolver{
		client:    client,
		cache:     cacheDir,
		sources:   sources,
		noRecurse: noRecurse,
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

	repo, file, depends, sha, err := r.locate(ctx, id)
	if err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("no download found for %s", id)
	}
	path, err := r.download(ctx, id, repo, file, sha)
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
// repo's index is fetched. It returns the repo base URI, the relative .deb
// path, the Depends: closure (used for recursion), and an optional sha256.
func (r *resolver) locate(ctx context.Context, id string) (repo, file, depends, sha string, err error) {
	for _, src := range r.sources {
		entries, ierr := r.indexFor(ctx, src)
		if ierr != nil {
			continue // a listed repo that's down doesn't block the others
		}
		if e := findEntry(entries, id); e != nil {
			return strings.TrimSuffix(src, "/"), e.Filename, e.Depends, e.SHA256, nil
		}
	}

	repoID, pkgFile, pkgSHA, cerr := canisterLookup(ctx, r.client, id)
	if cerr != nil {
		return "", "", "", "", fmt.Errorf("couldn't locate %s: %w", id, cerr)
	}
	uri, uerr := canisterRepoURI(ctx, r.client, repoID)
	if uerr != nil {
		return "", "", "", "", uerr
	}
	// The repo index is authoritative for Depends and Filename when
	// present; fall back to the Canister-reported path otherwise.
	if entries, ierr := r.indexFor(ctx, uri); ierr == nil {
		if e := findEntry(entries, id); e != nil {
			if e.Filename != "" {
				return uri, e.Filename, e.Depends, e.SHA256, nil
			}
			return uri, pkgFile, e.Depends, firstNonEmpty(e.SHA256, pkgSHA), nil
		}
	}
	return uri, pkgFile, "", pkgSHA, nil
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

// download fetches repo/file into the cache as <id>.deb, verifying the
// sha256 when one is available.
func (r *resolver) download(ctx context.Context, id, repo, file, sha string) (string, error) {
	if !validPackageID(id) {
		return "", fmt.Errorf("refusing unsafe package id %q", id)
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
		return "", fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 1<<30) // 1 GiB cap; errors on overflow
	if err != nil {
		return "", err
	}
	if sha != "" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sha {
			return "", fmt.Errorf("sha256 mismatch for %s", id)
		}
	}
	path := filepath.Join(r.cache, id+".deb")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
