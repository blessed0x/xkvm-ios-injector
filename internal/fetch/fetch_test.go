package fetch

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoServer is a hermetic MobileAPT repo: a Packages.gz index built from
// entries, plus .deb files served from files.
type repoServer struct {
	entries map[string]string // package id -> Depends value
	files   map[string][]byte // Filename -> deb content
}

func (rs *repoServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Packages.gz":
			var sb strings.Builder
			for id, depends := range rs.entries {
				file := "debs/" + id + ".deb"
				fmt.Fprintf(&sb, "Package: %s\nVersion: 1.0\nDepends: %s\nFilename: %s\nSHA256: %s\n\n",
					id, depends, file, shaOf(rs.files[file]))
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(sb.String()))
			_ = gz.Close()
		default:
			// URL paths carry a leading slash; the files map keys do not.
			if data, ok := rs.files[strings.TrimPrefix(r.URL.Path, "/")]; ok {
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canisterRepoStub points the Canister base at handlers serving /package/{id}
// and /repository/{repoID}, with repoID mapped to uri.
func canisterRepoStub(t *testing.T, pkgs map[string]string, repos map[string]string) {
	t.Helper()
	canisterStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/package/"):
			id := strings.TrimPrefix(r.URL.Path, "/package/")
			repoID, ok := pkgs[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"data":[{"package_id":%q,"repository_id":%q,"version":"1.0","package_filename":"debs/%s.deb","price":"Free","visible":true,"latest_version":true,"quality":1}]}`, id, repoID, id)
		case strings.HasPrefix(r.URL.Path, "/repository/"):
			id := strings.TrimPrefix(r.URL.Path, "/repository/")
			uri, ok := repos[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"data":{"id":%q,"uri":%q}}`, id, uri)
		default:
			http.NotFound(w, r)
		}
	}))
}

func assertDeb(t *testing.T, path, id, want string) {
	t.Helper()
	if filepath.Base(path) != id+".deb" {
		t.Errorf("deb path %q does not end with %s.deb", path, id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("deb %s content = %q, want %q", id, data, want)
	}
}

func TestResolveRecursionSkipsCoreAndGuardsCycles(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{
			"com.example.alpha": "mobilesubstrate (>= 0.9.5000), com.example.beta",
			"com.example.beta":  "com.example.alpha", // cycle back to alpha
		},
		files: map[string][]byte{
			"debs/com.example.alpha.deb": []byte("alpha-deb"),
			"debs/com.example.beta.deb":  []byte("beta-deb"),
		},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	debs, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, false, t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 2 {
		t.Fatalf("want alpha+beta only (core dep + cycle skipped), got %v", debs)
	}
	assertDeb(t, debs[0], "com.example.alpha", "alpha-deb")
	assertDeb(t, debs[1], "com.example.beta", "beta-deb")
}

func TestResolveNoRecurse(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{
			"com.example.alpha": "com.example.beta",
			"com.example.beta":  "",
		},
		files: map[string][]byte{
			"debs/com.example.alpha.deb": []byte("alpha-deb"),
			"debs/com.example.beta.deb":  []byte("beta-deb"),
		},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	debs, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, true, t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 1 {
		t.Fatalf("--no-recurse must fetch only alpha, got %v", debs)
	}
}

func TestResolveDedupesAcrossIds(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.alpha": ""},
		files:   map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	debs, err := Resolve(t.Context(), []string{"com.example.alpha", "com.example.alpha"}, []string{srv.URL}, true, t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 1 {
		t.Fatalf("duplicate id must download once, got %v", debs)
	}
}

func TestResolveSkipsSatisfiedAlternativeGroup(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{
			"com.example.alpha": "com.example.beta | com.example.gamma",
			"com.example.beta":  "",
			"com.example.gamma": "",
		},
		// gamma has no deb on the server: if the resolver wrongly picks it
		// despite beta already satisfying the group, the download 404s and
		// the test fails red.
		files: map[string][]byte{
			"debs/com.example.alpha.deb": []byte("alpha-deb"),
			"debs/com.example.beta.deb":  []byte("beta-deb"),
		},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	// beta is requested at top level, so alpha's "beta | gamma" group is
	// already satisfied; gamma must not be fetched.
	debs, err := Resolve(t.Context(), []string{"com.example.alpha", "com.example.beta"}, []string{srv.URL}, false, t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 2 {
		t.Fatalf("want alpha + beta only, got %v", debs)
	}
}

func TestResolvePicksFirstSatisfiableAlternative(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{
			"com.example.alpha": "firmware | com.example.beta",
			"com.example.beta":  "",
		},
		files: map[string][]byte{
			"debs/com.example.alpha.deb": []byte("alpha-deb"),
			"debs/com.example.beta.deb":  []byte("beta-deb"),
		},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	// First alternative (firmware) is core, so beta must be chosen.
	debs, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, false, t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 2 {
		t.Fatalf("want alpha + beta, got %v", debs)
	}
}

func TestResolveViaCanister(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.alpha": ""},
		files:   map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	repoSrv := httptest.NewServer(rs.handler())
	defer repoSrv.Close()

	canisterRepoStub(t,
		map[string]string{"com.example.alpha": "testrepo"},
		map[string]string{"testrepo": repoSrv.URL},
	)

	debs, err := Resolve(t.Context(), []string{"com.example.alpha"}, nil, false, t.TempDir(), repoSrv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 1 {
		t.Fatalf("want alpha, got %v", debs)
	}
	assertDeb(t, debs[0], "com.example.alpha", "alpha-deb")
}

func TestValidPackageID(t *testing.T) {
	for id, want := range map[string]bool{
		"com.example.foo":     true,
		"com.example.foo.deb": true,
		"":                    false,
		"..":                  false,
		"../evil":             false,
		"../../etc/passwd":    false,
		"a/b":                 false,
		"a\\b":                false,
		"a b":                 true, // spaces are ugly but not a path escape
	} {
		if got := validPackageID(id); got != want {
			t.Errorf("validPackageID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestResolveRefusesTraversalDepends(t *testing.T) {
	// A hostile repo index pointing a dependency at a path-traversal id
	// must fail resolution, never write outside the cache dir.
	rs := &repoServer{
		entries: map[string]string{
			"com.example.alpha": "../../../../tmp/xkvm-evil",
		},
		files: map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()

	_, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, false, t.TempDir(), srv.Client())
	if err == nil {
		t.Fatal("want an error refusing the traversal dependency id")
	}
}

func TestResolveUnresolvable(t *testing.T) {
	rs := &repoServer{entries: map[string]string{}, files: map[string][]byte{}}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()
	canisterRepoStub(t, map[string]string{}, map[string]string{})

	if _, err := Resolve(t.Context(), []string{"com.example.ghost"}, []string{srv.URL}, false, t.TempDir(), srv.Client()); err == nil {
		t.Fatal("want an error for an unresolvable package id")
	}
}

func TestResolveSHA256Mismatch(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.alpha": ""},
		files:   map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Packages.gz" {
			// Hand-write an index with a wrong SHA256 for the deb.
			body := "Package: com.example.alpha\nFilename: debs/com.example.alpha.deb\nSHA256: deadbeef\n\n"
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(body))
			_ = gz.Close()
			return
		}
		rs.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	if _, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, false, t.TempDir(), srv.Client()); err == nil {
		t.Fatal("want an error when the downloaded deb fails its sha256 check")
	}
}

// TestLiveCanisterFetch is the network-gated smoke test: it hits the real
// Canister index and a real repo. Skipped unless XKVM_LIVE_FETCH=1.
func TestLiveCanisterFetch(t *testing.T) {
	if os.Getenv("XKVM_LIVE_FETCH") == "" {
		t.Skip("set XKVM_LIVE_FETCH=1 to hit the real Canister index and repo")
	}
	debs, err := Resolve(t.Context(), []string{"ws.hbang.alderis"}, nil, true, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 1 {
		t.Fatalf("want one deb, got %v", debs)
	}
	st, err := os.Stat(debs[0])
	if err != nil || st.Size() == 0 {
		t.Fatalf("bad deb at %s: %v (size %d)", debs[0], err, st.Size())
	}
	t.Logf("fetched %s (%d bytes)", debs[0], st.Size())
}
