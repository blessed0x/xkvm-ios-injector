package fetch

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// PkgVersion is one available free version of a Canister-hosted package.
type PkgVersion struct {
	Version string // the exact version string (also the cache-key version)
	RepoID  string // hosting repository (shown in pickers, useful for logs)
	Latest  bool   // is this the version Resolve would download by default
}

// Versions lists the free, visible versions of a Canister package — latest
// first, deduplicated. It powers the TUI version picker; the CLI equivalent
// is the researcher-facing pin passed to ResolveVersion.
func Versions(ctx context.Context, id string, client *http.Client) ([]PkgVersion, error) {
	if client == nil {
		client = defaultClient
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("empty package id")
	}
	entries, err := canisterVersions(ctx, client, id)
	if err != nil {
		return nil, err
	}
	out := make([]PkgVersion, 0, len(entries))
	for i := range entries {
		out = append(out, PkgVersion{
			Version: entries[i].Version,
			RepoID:  entries[i].RepositoryID,
			Latest:  entries[i].Latest,
		})
	}
	// Stable order: latest-flagged first (API order is already newest
	// first), the rest keep their order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Latest != out[j].Latest {
			return out[i].Latest
		}
		return false
	})
	return out, nil
}
