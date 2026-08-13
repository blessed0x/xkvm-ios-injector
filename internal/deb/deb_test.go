package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	dsnetbzip2 "github.com/dsnet/compress/bzip2"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// testMember mirrors the package's arMember but with lowercase fields, so the
// test can construct ar archives independently of the production type.
type testMember struct {
	name string
	data []byte
}

// writeAr builds an ar archive, independently of the package code.
func writeAr(t *testing.T, path string, members []testMember) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("!<arch>\n"); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", m.name, 0, 0, 0, 0o644, len(m.data))
		if len(hdr) != 60 {
			t.Fatalf("bad ar header length %d", len(hdr))
		}
		if _, err := f.WriteString(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(m.data); err != nil {
			t.Fatal(err)
		}
		if len(m.data)%2 == 1 {
			if _, err := f.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func compress(t *testing.T, data []byte, ext string) []byte {
	t.Helper()
	if ext == "" {
		return data
	}
	var buf bytes.Buffer
	var err error
	switch ext {
	case ".gz":
		w := gzip.NewWriter(&buf)
		_, err = w.Write(data)
		w.Close()
	case ".xz":
		w, e := xz.NewWriter(&buf)
		err = e
		if err == nil {
			_, err = w.Write(data)
			w.Close()
		}
	case ".zst":
		w, e := zstd.NewWriter(&buf)
		err = e
		if err == nil {
			_, err = w.Write(data)
			w.Close()
		}
	case ".bz2":
		w, e := dsnetbzip2.NewWriter(&buf, nil)
		err = e
		if err == nil {
			_, err = w.Write(data)
			w.Close()
		}
	case ".lzma":
		w, e := lzma.NewWriter(&buf)
		err = e
		if err == nil {
			_, err = w.Write(data)
			w.Close()
		}
	default:
		t.Fatalf("unknown compression %q", ext)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dataMember(t *testing.T, ext string) testMember {
	t.Helper()
	data := tarBytes(t, map[string]string{
		"Library/MobileSubstrate/DynamicLibraries/Tweak.dylib": "tweak",
		"Library/Application Support/Prefs.bundle/Info.plist":  "prefs",
		"Frameworks/MyTweak.framework/Info.plist":              "fw",
		"Frameworks/MyTweak.framework/MyTweak":                 "fwbin",
		"Frameworks/Nested.framework/Sub.bundle/x":             "nested",
	})
	return testMember{name: "data.tar" + ext, data: compress(t, data, ext)}
}

func TestExtractAllCompressions(t *testing.T) {
	for _, ext := range []string{".gz", ".xz", ".zst", ".bz2", ".lzma", ""} {
		t.Run(ext, func(t *testing.T) {
			debPath := filepath.Join(t.TempDir(), "t.deb")
			writeAr(t, debPath, []testMember{
				{name: "debian-binary", data: []byte("2.0\n")},
				{name: "control.tar.gz", data: compress(t, tarBytes(t, map[string]string{"control": "Package: x\n"}), ".gz")},
				dataMember(t, ext),
			})

			arts, err := Extract(debPath, t.TempDir())
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			got := basenames(arts)
			// Nested.framework is a legit top-level artifact; its inner
			// Sub.bundle must NOT be collected separately (nested-bundle skip).
			want := []string{"Tweak.dylib", "Prefs.bundle", "MyTweak.framework", "Nested.framework"}
			sort.Strings(got)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("artifacts = %v, want %v", got, want)
			}
		})
	}
}

func TestExtractSkipsSymlinks(t *testing.T) {
	// A tar with a symlink named like an artifact must not produce it, and no
	// symlink must be created on disk.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := "evil"
	if err := tw.WriteHeader(&tar.Header{
		Name: "Library/MobileSubstrate/DynamicLibraries/Evil.dylib",
		Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.WriteHeader(&tar.Header{
		Name: "Library/MobileSubstrate/DynamicLibraries/Real.dylib",
		Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()

	dest := t.TempDir()
	debPath := filepath.Join(t.TempDir(), "t.deb")
	writeAr(t, debPath, []testMember{{name: "data.tar.gz", data: compress(t, buf.Bytes(), ".gz")}})

	arts, err := Extract(debPath, dest)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if got := basenames(arts); !reflect.DeepEqual(got, []string{"Real.dylib"}) {
		t.Errorf("artifacts = %v, want [Real.dylib]", got)
	}
	if _, err := os.Lstat(filepath.Join(dest, "Library/MobileSubstrate/DynamicLibraries/Evil.dylib")); err == nil {
		t.Error("symlink was created on disk")
	}
}

func TestExtractRejectsPathEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{
		Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()

	dest := t.TempDir()
	debPath := filepath.Join(t.TempDir(), "t.deb")
	writeAr(t, debPath, []testMember{{name: "data.tar.gz", data: compress(t, buf.Bytes(), ".gz")}})

	if _, err := Extract(debPath, dest); err == nil {
		t.Fatal("expected an error for a traversing tar entry")
	}
}

func TestExtractNotADeb(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.deb")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(p, t.TempDir()); err == nil {
		t.Fatal("expected an error for a non-ar input")
	}
}

func basenames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}
