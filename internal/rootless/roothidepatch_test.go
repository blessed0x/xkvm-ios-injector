package rootless

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/testutil"
)

// dataTarSymlinks walks the data.tar member of a deb (the exact bytes dpkg
// installs — internal/deb.Unpack deliberately skips symlinks, so the shipped
// archive is the source of truth here) and returns path → link target for
// every TypeSymlink entry.
func dataTarSymlinks(t *testing.T, debPath string) map[string]string {
	t.Helper()
	f, err := os.Open(debPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// ar: 8-byte global header, then 60-byte member headers, payload padded
	// to even length.
	var g [8]byte
	if _, err := io.ReadFull(f, g[:]); err != nil {
		t.Fatal(err)
	}
	if string(g[:]) != "!<arch>\n" {
		t.Fatalf("not an ar archive: %q", g)
	}
	for {
		var h [60]byte
		if _, err := io.ReadFull(f, h[:]); err == io.EOF {
			t.Fatal("no data.tar member found")
		} else if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSpace(string(h[0:16]))
		size := 0
		for _, c := range strings.TrimSpace(string(h[48:58])) {
			size = size*10 + int(c-'0')
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(f, body); err != nil {
			t.Fatal(err)
		}
		if size%2 == 1 {
			if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(name, "data.tar") {
			// gzip only — the deb builder emits data.tar.gz.
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			out := map[string]string{}
			tr := tar.NewReader(zr)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if hdr.Typeflag == tar.TypeSymlink {
					out[strings.TrimPrefix(hdr.Name, "./")] = hdr.Linkname
				}
			}
			return out
		}
	}
}

// TestRoothideAutoPatchesSymlinks pins the AutoPatches mechanism against the
// reference: --mode auto ships a sibling <file>.roothidepatch symlink (→
// /usr/lib/DynamicPatches/AutoPatches.dylib) for EVERY patched payload
// Mach-O — but not for non-Mach-O files, not in dynamic mode, and not inside
// the pkgmirror snapshot (which is pre-patch, matching upstream's findcmd
// exclusion). Also pins the mirror-side copy of DEBIAN/*.roothidepatch files
// (upstream line 352).
func TestRoothideAutoPatchesSymlinks(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	in, _ := buildRootlessFixture(t, tmp, "autopatchtweak", "1.0")

	// The fixture ships a .roothidepatch file in DEBIAN/ (the upstream
	// mirror-copy source) — add it to the built fixture deb.
	// buildRootlessFixture built 'in'; rebuild with the extra DEBIAN file.
	in = withDebianRoothidepatch(t, in, tmp)

	// --- auto mode: symlinks in the payload, none in the mirror, plus the
	// rootless-compat Pre-Depends.
	out := filepath.Join(tmp, "auto.deb")
	if err := ConvertToRoothide(in, out, true, "auto"); err != nil {
		t.Fatalf("ConvertToRoothide(auto): %v", err)
	}
	links := dataTarSymlinks(t, out)

	dylib := "Library/MobileSubstrate/DynamicLibraries/PkgMirrorTweak.dylib"
	link := dylib + ".roothidepatch"
	if links[link] != autoPatchesDylib {
		t.Errorf("auto mode: payload symlink %s = %q, want %q; all symlinks:\n%v", link, links[link], autoPatchesDylib, links)
	}
	// Only Mach-Os get symlinks — the helper script must not.
	if _, ok := links["rootfs/usr/bin/roothide-helper.roothidepatch"]; ok {
		t.Error("non-Mach-O file got a .roothidepatch symlink")
	}
	// The mirror is a pre-patch snapshot: no payload symlinks inside it.
	for p := range links {
		if strings.HasPrefix(p, "var/mobile/Library/pkgmirror/") {
			t.Errorf("mirror contains a .roothidepatch symlink: %s", p)
		}
	}
	// The shipped DEBIAN/*.roothidepatch file lands in the mirror's control
	// dir as a regular file (upstream line 352 copy).
	if !hasEntry(t, out, "var/mobile/Library/pkgmirror/DEBIAN.autopatchtweak/shipped.roothidepatch") {
		t.Error("mirror missing the shipped DEBIAN/*.roothidepatch copy")
	}

	// Control carries the auto Pre-Depends.
	unpacked := filepath.Join(tmp, "unpacked-auto")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctl), "rootless-compat(>= 0.9)") {
		t.Errorf("auto control missing rootless-compat; got:\n%s", ctl)
	}

	// --- dynamic mode: no payload symlinks at all (version + patches dep only).
	out2 := filepath.Join(tmp, "dynamic.deb")
	if err := ConvertToRoothide(in, out2, false, "dynamic"); err != nil {
		t.Fatalf("ConvertToRoothide(dynamic): %v", err)
	}
	links2 := dataTarSymlinks(t, out2)
	for p := range links2 {
		t.Errorf("dynamic mode must not create .roothidepatch symlinks, found: %s", p)
	}

	// --- default mode: no symlinks either (upstream without a mode arg).
	out3 := filepath.Join(tmp, "default.deb")
	if err := ConvertToRoothide(in, out3, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide(default): %v", err)
	}
	for p := range dataTarSymlinks(t, out3) {
		t.Errorf("default mode must not create .roothidepatch symlinks, found: %s", p)
	}
}

// hasEntry reports whether the deb's data.tar contains the path (any type).
func hasEntry(t *testing.T, debPath, want string) bool {
	t.Helper()
	f, err := os.Open(debPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var g [8]byte
	if _, err := io.ReadFull(f, g[:]); err != nil {
		t.Fatal(err)
	}
	for {
		var h [60]byte
		if _, err := io.ReadFull(f, h[:]); err == io.EOF {
			return false
		} else if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSpace(string(h[0:16]))
		size := 0
		for _, c := range strings.TrimSpace(string(h[48:58])) {
			size = size*10 + int(c-'0')
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(f, body); err != nil {
			t.Fatal(err)
		}
		if size%2 == 1 {
			if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(name, "data.tar") {
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			tr := tar.NewReader(zr)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					return false
				}
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimPrefix(hdr.Name, "./") == want {
					return true
				}
			}
		}
	}
}

// withDebianRoothidepatch adds DEBIAN/shipped.roothidepatch to the fixture
// deb and returns the rebuilt deb path.
func withDebianRoothidepatch(t *testing.T, in, tmp string) string {
	t.Helper()
	dir := filepath.Join(tmp, "staging-withpatch")
	if err := deb.Unpack(in, dir); err != nil {
		t.Fatal(err)
	}
	// Unpack skips symlinks and control files; add the extra control file.
	if err := os.WriteFile(filepath.Join(dir, "DEBIAN", "shipped.roothidepatch"), []byte("dummy patch"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "rootless-withpatch.deb")
	if err := deb.Build(dir, out); err != nil {
		t.Fatal(err)
	}
	return out
}
