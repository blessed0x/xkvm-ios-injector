package deb

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBuildUnpackRoundTrip builds a deb from a staging tree and unpacks it
// back, verifying control and payload land where dpkg expects them, symlinks
// survive, and the ar archive parses with the package's own reader.
func TestBuildUnpackRoundTrip(t *testing.T) {
	tmp := t.TempDir()

	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte("Package: demo\nVersion: 1.0\nArchitecture: iphoneos-arm64\n"), 0o644)
	dl := filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(dl, 0o755)
	os.WriteFile(filepath.Join(dl, "Demo.dylib"), []byte("macho-bytes"), 0o644)
	os.MkdirAll(filepath.Join(staging, "usr", "lib"), 0o755)
	os.WriteFile(filepath.Join(staging, "usr", "lib", "libextra.dylib"), []byte("extra"), 0o644)
	// A payload symlink must be written as a tar symlink entry (the reader
	// deliberately skips symlinks — see deb.go — so it is asserted at the
	// tar level below, not after Unpack). Windows CI runners can't create
	// symlinks without developer mode, so the link half only runs where
	// os.Symlink works; the rest of the round trip still asserts.
	linkOK := true
	if err := os.Symlink("/var/jb/Library/MobileSubstrate/DynamicLibraries/Demo.dylib", filepath.Join(dl, "Link.dylib")); err != nil {
		if runtime.GOOS == "windows" {
			linkOK = false
		} else {
			t.Fatal(err)
		}
	}

	out := filepath.Join(tmp, "demo.deb")
	if err := Build(staging, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("built deb is empty")
	}

	// The ar archive must parse with the package reader.
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var magic [8]byte
	if _, err := f.Read(magic[:]); err != nil || string(magic[:]) != arMagic {
		t.Fatalf("bad ar magic %q", magic)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := Unpack(out, unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatalf("control missing: %v", err)
	}
	if !strings.Contains(string(ctl), "Package: demo") {
		t.Errorf("control content wrong: %q", ctl)
	}
	payload, err := os.ReadFile(filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "Demo.dylib"))
	if err != nil {
		t.Fatalf("payload missing: %v", err)
	}
	if string(payload) != "macho-bytes" {
		t.Errorf("payload content wrong: %q", payload)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "usr", "lib", "libextra.dylib")); err != nil {
		t.Errorf("usr payload missing: %v", err)
	}

	// The symlink survives inside data.tar as a TypeSymlink entry with the
	// original target (Unpack intentionally skips links, so this is checked
	// directly in the archive).
	f, err = os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Skip the 8-byte global "!<arch>\n" magic before member headers.
	if _, err := io.CopyN(io.Discard, f, 8); err != nil {
		t.Fatal(err)
	}
	foundLink := false
	sawDebian := false
	for {
		m, err := readArMember(f)
		if err != nil {
			break
		}
		if !strings.HasPrefix(m.Name, "data.tar") {
			continue
		}
		zr, err := gzip.NewReader(m.Data)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			if strings.HasPrefix(hdr.Name, "./DEBIAN") {
				sawDebian = true
			}
			if hdr.Name == "./Library/MobileSubstrate/DynamicLibraries/Link.dylib" {
				if hdr.Typeflag != tar.TypeSymlink {
					t.Errorf("Link.dylib is %v, want symlink", hdr.Typeflag)
				}
				if hdr.Linkname != "/var/jb/Library/MobileSubstrate/DynamicLibraries/Demo.dylib" {
					t.Errorf("symlink target wrong: %q", hdr.Linkname)
				}
				foundLink = true
			}
		}
		break // data.tar is the only payload archive
	}
	if sawDebian {
		t.Error("data.tar must not contain DEBIAN entries (dpkg would create /DEBIAN on install)")
	}
	if !foundLink && linkOK {
		t.Error("Link.dylib symlink entry missing from data.tar")
	}
}

// TestBuildThenExtract verifies the artifact collector sees the payload of a
// deb produced by Build (the injection pipeline's entry point).
func TestBuildThenExtract(t *testing.T) {
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte("Package: t\nVersion: 1\nArchitecture: iphoneos-arm64\n"), 0o644)
	dl := filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(dl, 0o755)
	os.WriteFile(filepath.Join(dl, "Hooker.dylib"), []byte("dylib"), 0o644)

	out := filepath.Join(tmp, "t.deb")
	if err := Build(staging, out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	dest := filepath.Join(tmp, "x")
	arts, err := Extract(out, dest)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %v", len(arts), arts)
	}
	if filepath.Base(arts[0]) != "Hooker.dylib" {
		t.Errorf("unexpected artifact %q", arts[0])
	}
}

// TestControlFormat pins the generated control rendering.
func TestControlFormat(t *testing.T) {
	c := Control{
		Package:      "cooltweak",
		Name:         "CoolTweak",
		Version:      "2.0",
		Architecture: "iphoneos-arm64",
		Depends:      []string{"mobilesubstrate", "preferenceloader"},
		Description:  "A tweak",
		Maintainer:   "xkvm",
		Author:       "xkvm",
		Section:      "Tweaks",
	}
	got := c.String()
	for _, want := range []string{
		"Package: cooltweak\n",
		"Name: CoolTweak\n",
		"Version: 2.0\n",
		"Architecture: iphoneos-arm64\n",
		"Depends: mobilesubstrate, preferenceloader\n",
		"Description: A tweak\n",
		"Section: Tweaks\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("control missing %q; got:\n%s", want, got)
		}
	}
}
