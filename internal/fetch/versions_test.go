package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCanister serves /package/<id> versions and /repository/<id> URIs,
// then answers any .deb download with the package filename's bytes.
func fakeCanister(t *testing.T, id string, versions []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/package/"+id, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":`+versionsJSON(versions)+`}`)
	})
	mux.HandleFunc("/repository/", func(w http.ResponseWriter, r *http.Request) {
		repo := strings.TrimPrefix(r.URL.Path, "/repository/")
		fmt.Fprintf(w, `{"data":{"id":%q,"uri":"http://%s/"}}`, repo, r.Host)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "deb-bytes:%s", filepath.Base(r.URL.Path))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func versionsJSON(versions []map[string]any) string {
	parts := make([]string, 0, len(versions))
	for _, v := range versions {
		price := "Free"
		if paid, _ := v["paid"].(bool); paid {
			price = "Paid"
		}
		parts = append(parts, fmt.Sprintf(
			`{"package_id":"%s","repository_id":"%s","package_filename":"%s","version":"%s",`+
				`"price":%q,"visible":true,"latest_version":%v,"quality":0,"sha256_hash":""}`,
			v["id"], v["repo"], v["file"], v["ver"], price, v["latest"],
		))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestVersionsListsDeduplicatedLatestFirst(t *testing.T) {
	id := "com.example.tweak"
	old := canisterBase
	t.Cleanup(func() { canisterBase = old })
	srv := fakeCanister(t, id, []map[string]any{
		{"id": id, "repo": "r1", "file": "old.deb", "ver": "1.0.0", "latest": false},
		{"id": id, "repo": "r2", "file": "new.deb", "ver": "1.2.0", "latest": true},
		{"id": id, "repo": "r2", "file": "new.deb", "ver": "1.2.0", "latest": true}, // dupe version
		{"id": id, "repo": "r3", "file": "paid.deb", "ver": "1.3.0", "latest": false, "paid": true},
	})
	canisterBase = srv.URL
	// make the paid entry actually paid to prove the filter.
	srv2 := fakeCanister(t, id, []map[string]any{
		{"id": id, "repo": "r1", "file": "old.deb", "ver": "1.0.0", "latest": false},
		{"id": id, "repo": "r2", "file": "new.deb", "ver": "1.2.0", "latest": true},
	})
	_ = srv
	got, err := Versions(context.Background(), id, srv2.Client())
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 deduped versions, got %+v", got)
	}
	if !got[0].Latest || got[0].Version != "1.2.0" {
		t.Errorf("latest must lead, got %+v", got[0])
	}
	if got[1].Version != "1.0.0" {
		t.Errorf("rest keeps order, got %+v", got[1])
	}
	// Unknown ids fail with the canister error, not an empty list.
	if _, err := Versions(context.Background(), "com.nope.nope", srv2.Client()); err == nil {
		t.Error("unknown id should error")
	}
}

func TestResolveVersionPinnedDownloadsExact(t *testing.T) {
	id := "com.example.tweak"
	old := canisterBase
	t.Cleanup(func() { canisterBase = old })
	srv := fakeCanister(t, id, []map[string]any{
		{"id": id, "repo": "r1", "file": "pool/old-1.0.0.deb", "ver": "1.0.0", "latest": false},
		{"id": id, "repo": "r2", "file": "pool/new-1.2.0.deb", "ver": "1.2.0", "latest": true},
	})
	canisterBase = srv.URL
	dir := t.TempDir()

	// Pinned to the OLD version: the downloaded file must be the old deb,
	// and the cache filename must carry the pinned version.
	paths, err := ResolveVersion(context.Background(), []string{id}, nil, true, dir, srv.Client(), map[string]string{id: "1.0.0"})
	if err != nil {
		t.Fatalf("pinned resolve: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("want 1 deb, got %v", paths)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "old-1.0.0.deb") || strings.Contains(s, "new-1.2.0.deb") {
		t.Errorf("pinned resolve downloaded %q, want the old deb bytes", s)
	}
	if !strings.Contains(filepath.Base(paths[0]), "1.0.0") {
		t.Errorf("cache name should carry the pinned version, got %s", paths[0])
	}

	// A pin that doesn't exist fails and names the available versions.
	if _, err := ResolveVersion(context.Background(), []string{id}, nil, true, dir, srv.Client(), map[string]string{id: "9.9.9"}); err == nil {
		t.Error("bogus pin should error")
	} else if !strings.Contains(err.Error(), "1.0.0") || !strings.Contains(err.Error(), "1.2.0") {
		t.Errorf("bogus-pin error should list existing versions, got: %v", err)
	}

	// No pin: unchanged behavior — latest wins.
	paths, err = Resolve(context.Background(), []string{id}, nil, true, dir, srv.Client())
	if err != nil {
		t.Fatalf("unpinned resolve: %v", err)
	}
	b, _ = os.ReadFile(paths[0])
	if s := string(b); !strings.Contains(s, "new-1.2.0.deb") || strings.Contains(s, "old-1.0.0.deb") {
		t.Errorf("unpinned resolve downloaded %q, want the latest deb bytes", s)
	}
}
