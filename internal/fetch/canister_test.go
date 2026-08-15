package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// canisterStub runs handler against the Canister base and restores the real
// base on cleanup.
func canisterStub(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := canisterBase
	canisterBase = srv.URL
	t.Cleanup(func() {
		canisterBase = old
		srv.Close()
	})
	return srv
}

func TestCanisterLookupPrefersLatestFree(t *testing.T) {
	srv := canisterStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/package/ws.hbang.alderis" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"status":"200 OK","count":2,"data":[
{"package_id":"ws.hbang.alderis","repository_id":"chariz","version":"1.2.3","package_filename":"debs/old.deb","price":"Free","visible":true,"latest_version":false,"quality":1,"sha256_hash":"aaa"},
{"package_id":"ws.hbang.alderis","repository_id":"havoc","version":"1.3.0","package_filename":"debs/new.deb","price":"Free","visible":true,"latest_version":true,"quality":5,"sha256_hash":"bbb"}]}`))
	}))
	defer srv.Close()

	repo, file, sha, version, err := canisterLookup(context.Background(), srv.Client(), "ws.hbang.alderis")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "havoc" || file != "debs/new.deb" || sha != "bbb" || version != "1.3.0" {
		t.Fatalf("got repo=%q file=%q sha=%q version=%q, want the havoc/latest entry", repo, file, sha, version)
	}
}

func TestCanisterLookupSkipsPaidAndHidden(t *testing.T) {
	srv := canisterStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
{"package_id":"x","repository_id":"a","version":"1","package_filename":"p.deb","price":"$4.99","visible":true,"latest_version":true,"quality":9},
{"package_id":"x","repository_id":"b","version":"1","package_filename":"q.deb","price":"Free","visible":false,"latest_version":true,"quality":9}]}`))
	}))
	defer srv.Close()

	if _, _, _, _, err := canisterLookup(context.Background(), srv.Client(), "x"); err == nil {
		t.Fatal("want an error when only paid/hidden entries exist")
	}
}

func TestCanisterRepoURI(t *testing.T) {
	srv := canisterStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repository/chariz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"chariz","uri":"https://repo.chariz.com"}}`))
	}))
	defer srv.Close()

	uri, err := canisterRepoURI(context.Background(), srv.Client(), "chariz")
	if err != nil {
		t.Fatal(err)
	}
	if uri != "https://repo.chariz.com" {
		t.Fatalf("uri = %q", uri)
	}
}

func TestCanisterLookupHTTPError(t *testing.T) {
	srv := canisterStub(t, http.NotFoundHandler())
	defer srv.Close()
	if _, _, _, _, err := canisterLookup(context.Background(), srv.Client(), "x"); err == nil {
		t.Fatal("want an error on 404")
	}
}
