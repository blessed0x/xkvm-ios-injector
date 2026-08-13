package fetch

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dsnet/compress/bzip2"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

const sampleIndex = `Package: com.example.alpha
Version: 1.0
Depends: mobilesubstrate (>= 0.9.5000), preferenceloader
Filename: debs/alpha.deb
SHA256: aabbccdd

Package: com.example.beta
Version: 2.0
Depends: com.example.alpha,
 com.example.gamma
Filename: debs/beta.deb
`

func TestParseIndex(t *testing.T) {
	entries, err := parseIndex([]byte(sampleIndex))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	a := findEntry(entries, "com.example.alpha")
	if a == nil || a.Filename != "debs/alpha.deb" || a.Version != "1.0" {
		t.Fatalf("bad alpha entry: %+v", a)
	}
	if a.Depends != "mobilesubstrate (>= 0.9.5000), preferenceloader" {
		t.Errorf("alpha Depends folded wrong: %q", a.Depends)
	}
	b := findEntry(entries, "com.example.beta")
	if b == nil {
		t.Fatal("beta entry missing")
	}
	if b.Depends != "com.example.alpha, com.example.gamma" {
		t.Errorf("continuation lines not folded: %q", b.Depends)
	}
	if findEntry(entries, "com.example.nope") != nil {
		t.Error("findEntry matched a missing package")
	}
}

func TestFindEntryPrefersArm64(t *testing.T) {
	entries := []Entry{
		{Package: "com.example.x", Architecture: "armv7", Filename: "debs/old.deb"},
		{Package: "com.example.x", Architecture: "arm64", Filename: "debs/new.deb"},
	}
	if e := findEntry(entries, "com.example.x"); e == nil || e.Filename != "debs/new.deb" {
		t.Fatalf("want the arm64 stanza, got %+v", e)
	}
	if e := findEntry(entries, "com.example.nope"); e != nil {
		t.Fatalf("matched a missing package: %+v", e)
	}
	// Without an arm64 stanza the first match is the fallback.
	plain := []Entry{{Package: "com.example.x", Architecture: "armv7", Filename: "debs/old.deb"}}
	if e := findEntry(plain, "com.example.x"); e == nil || e.Filename != "debs/old.deb" {
		t.Fatalf("want first-match fallback, got %+v", e)
	}
}

func TestParseIndexCommentsAndBlankStanzas(t *testing.T) {
	data := "# a comment\n\n\nPackage: x\nFilename: a.deb\n\n# trailing comment\n"
	entries, err := parseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Package != "x" {
		t.Fatalf("got %+v", entries)
	}
}

func TestParseIndexEmpty(t *testing.T) {
	if _, err := parseIndex([]byte("# nothing here\n\n")); err == nil {
		t.Fatal("want an error for an empty index")
	}
}

func TestDecompressRoundTrips(t *testing.T) {
	for _, name := range []string{"Packages.gz", "Packages.zst", "Packages.xz", "Packages.bz2"} {
		data := compressFor(t, name, []byte(sampleIndex))
		got, err := decompress(data, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, []byte(sampleIndex)) {
			t.Fatalf("%s round-trip produced different bytes", name)
		}
	}
}

func compressFor(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch name {
	case "Packages.gz":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "Packages.zst":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		w.Close()
	case "Packages.xz":
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "Packages.bz2":
		w, err := bzip2.NewWriter(&buf, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestFetchIndexFallsBackToZstd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Packages":
			http.NotFound(w, r)
		case "/Packages.gz", "/Packages.xz", "/Packages.bz2":
			http.NotFound(w, r)
		case "/Packages.zst":
			_, _ = w.Write(compressFor(t, "Packages.zst", []byte(sampleIndex)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	entries, err := fetchIndex(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if findEntry(entries, "com.example.beta") == nil {
		t.Error("zstd index not parsed")
	}
}

func TestFetchIndexAllVariantsMissing(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := fetchIndex(t.Context(), srv.Client(), srv.URL); err == nil {
		t.Fatal("want an error when no Packages variant exists")
	}
}

func TestReadLimited(t *testing.T) {
	if got, err := readLimited(strings.NewReader("hello"), 8); err != nil || string(got) != "hello" {
		t.Fatalf("small read = %q, %v; want hello, nil", got, err)
	}
	if _, err := readLimited(strings.NewReader(""), 8); err != nil {
		t.Fatalf("empty read: %v", err)
	}
	if _, err := readLimited(strings.NewReader("12345678"), 8); err != nil {
		t.Fatalf("exact-cap read: %v", err)
	}
	if _, err := readLimited(strings.NewReader("123456789"), 8); err == nil {
		t.Fatal("want an error when the stream exceeds the cap (no silent truncation)")
	}
}
