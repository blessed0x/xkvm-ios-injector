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
	"time"
)

// TestMain pins defaultRepos to an empty sweep list so no test accidentally
// sweeps the real 80-repo list over the network (the miss path would
// otherwise turn an unresolvable-id test into 80 sequential real requests).
// Tests that exercise the sweep swap in their own httptest servers (see
// TestResolveFallsBackToDefaultRepos).
func TestMain(m *testing.M) {
	defaultRepos = nil
	os.Exit(m.Run())
}

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
	// The repoServer index writes Version: 1.0, so cache files are
	// versioned: <id>__1.0.deb.
	if filepath.Base(path) != id+"__1.0.deb" {
		t.Errorf("deb path %q does not end with %s__1.0.deb", path, id)
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
	// The traversal id is unknown to Canister too; stub it to 404 so the
	// lookup fails locally instead of leaking to the real index.
	canisterRepoStub(t, map[string]string{}, map[string]string{})

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
// countingRepo wraps a repoServer handler and counts deb downloads (paths
// under /debs/), so tests can prove the cache short-circuits the network.
type countingRepo struct {
	srv       *httptest.Server
	downloads int
}

func newCountingRepo(t *testing.T, rs *repoServer) *countingRepo {
	t.Helper()
	c := &countingRepo{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/debs/") {
			c.downloads++
		}
		rs.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// TestResolveReusesCachedDependency pins the smart-dependency cache: the
// second resolve of the same id+version into the same cache dir must not hit
// the network for the deb (only the index is fetched again).
func TestResolveReusesCachedDependency(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.alpha": ""},
		files:   map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	c := newCountingRepo(t, rs)
	cache := t.TempDir()

	if _, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{c.srv.URL}, true, cache, c.srv.Client()); err != nil {
		t.Fatal(err)
	}
	if c.downloads != 1 {
		t.Fatalf("first resolve should download the deb once, got %d", c.downloads)
	}
	if _, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{c.srv.URL}, true, cache, c.srv.Client()); err != nil {
		t.Fatal(err)
	}
	if c.downloads != 1 {
		t.Errorf("second resolve must reuse the cached deb (downloads=%d, want 1)", c.downloads)
	}
}

// TestCachedShaMismatchRedownloads pins that a cached file failing the
// expected sha (e.g. the repo republished the same version) is re-downloaded
// rather than trusted blindly.
func TestCachedShaMismatchRedownloads(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.alpha": ""},
		files:   map[string][]byte{"debs/com.example.alpha.deb": []byte("alpha-deb")},
	}
	srv := httptest.NewServer(rs.handler())
	defer srv.Close()
	cache := t.TempDir()

	// Prime the cache with a stale file that doesn't match the deb's sha.
	stale := filepath.Join(cache, "com.example.alpha__1.0.deb")
	if err := os.WriteFile(stale, []byte("wrong-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	debs, err := Resolve(t.Context(), []string{"com.example.alpha"}, []string{srv.URL}, true, cache, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(debs[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "alpha-deb" {
		t.Errorf("cached stale bytes must be replaced by the real download, got %q", data)
	}
}

// TestPruneCacheTTL pins the 7-day automatic cleanup: entries older than
// cacheTTL are dropped, fresh ones survive.
func TestPruneCacheTTL(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "com.example.old__1.0.deb")
	fresh := filepath.Join(dir, "com.example.fresh__1.0.deb")
	notDeb := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, fresh, notDeb} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	daysAgo := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, daysAgo, daysAgo); err != nil {
		t.Fatal(err)
	}

	pruneCache(dir, now)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("entry older than the 7-day TTL should be pruned, stat err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh entry must survive the prune: %v", err)
	}
	if _, err := os.Stat(notDeb); err != nil {
		t.Errorf("non-deb files must never be pruned: %v", err)
	}
}

// TestResolveFallsBackToDefaultRepos pins the smart dependency solver's last
// tier: when neither the explicit sources nor Canister know the id, the
// built-in default repo list is swept for a Packages-index hit.
func TestResolveFallsBackToDefaultRepos(t *testing.T) {
	rs := &repoServer{
		entries: map[string]string{"com.example.niche": ""},
		files:   map[string][]byte{"debs/com.example.niche.deb": []byte("niche-deb")},
	}
	fallback := httptest.NewServer(rs.handler())
	defer fallback.Close()

	old := defaultRepos
	defaultRepos = []string{fallback.URL}
	t.Cleanup(func() { defaultRepos = old })
	canisterRepoStub(t, map[string]string{}, map[string]string{}) // Canister 404s everything

	debs, err := Resolve(t.Context(), []string{"com.example.niche"}, nil, true, t.TempDir(), fallback.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(debs) != 1 {
		t.Fatalf("want the fallback-resolved deb, got %v", debs)
	}
	assertDeb(t, debs[0], "com.example.niche", "niche-deb")
}

// TestCountCache pins the cache inspection used by `xkvm cache` and the TUI
// cache menu: only .deb files count, sizes add up, non-deb files are ignored.
func TestCountCache(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"com.example.a__1.0.deb": "0123456789",       // 10 bytes
		"com.example.b__2.0.deb": "0123456789012345", // 16 bytes
		"notes.txt":              "ignore me",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	debs, bytes, err := countCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if debs != 2 || bytes != 26 {
		t.Errorf("countCache = %d debs, %d bytes; want 2 debs, 26 bytes", debs, bytes)
	}
}

// TestClearCacheDir pins the explicit wipe: every .deb goes regardless of
// age; non-deb files and directories survive.
func TestClearCacheDir(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a__1.0.deb", "b__1.0.deb", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := clearCacheDir(dir); got != 2 {
		t.Fatalf("clearCacheDir removed %d, want 2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("non-deb file must survive a cache clear: %v", err)
	}
}

// TestPruneExpiredAt pins the exported prune path (used by the TUI cache
// menu and `xkvm cache`): only entries older than the 7-day TTL are removed,
// and the count matches.
func TestPruneExpiredAt(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old__1.0.deb")
	fresh := filepath.Join(dir, "fresh__1.0.deb")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := pruneExpiredAt(dir, now); got != 1 {
		t.Fatalf("pruneExpiredAt removed %d, want 1 (only the 8-day-old entry)", got)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh entry must survive: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired entry must be gone: %v", err)
	}
}

func TestCacheFileName(t *testing.T) {
	for _, tc := range []struct {
		id, version, want string
	}{
		{"com.example.foo", "1.0", "com.example.foo__1.0.deb"},
		{"com.example.foo", "2.0~beta1", "com.example.foo__2.0~beta1.deb"},
		{"com.example.foo", "1.0+dfsg-2", "com.example.foo__1.0+dfsg-2.deb"},
		{"com.example.foo", "1:1.0-2+deb12", "com.example.foo__1_1.0-2+deb12.deb"},
		{"com.example.foo", "", "com.example.foo.deb"},
	} {
		if got := cacheFileName(tc.id, tc.version); got != tc.want {
			t.Errorf("cacheFileName(%q, %q) = %q, want %q", tc.id, tc.version, got, tc.want)
		}
	}
}

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
