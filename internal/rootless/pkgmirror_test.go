package rootless

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

// buildRootlessFixture builds a rootless deb (payload under var/jb, a loose
// system file, and a control carrying the given package/version) whose
// tweak links a /var/jb dep and carries a /var/jb rpath. Returns the deb
// path plus the original tweak bytes for byte-completeness comparisons.
func buildRootlessFixture(t *testing.T, tmp, pkg, ver string) (string, []byte) {
	t.Helper()
	tweak := testutil.MakeTweak(t, tmp, "PkgMirrorTweak")
	b := macho.Bin{Path: tweak}
	if err := b.InjectWeak("/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	if err := b.AddRpath("/var/jb/usr/lib"); err != nil {
		t.Fatalf("AddRpath: %v", err)
	}
	origBytes, err := os.ReadFile(tweak)
	if err != nil {
		t.Fatal(err)
	}

	staging := filepath.Join(tmp, "staging")
	payloadDir := filepath.Join(staging, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, "PkgMirrorTweak.dylib")); err != nil {
		t.Fatal(err)
	}
	// A loose system file (not under var/jb) — must land under rootfs/.
	os.MkdirAll(filepath.Join(staging, "usr", "bin"), 0o755)
	os.WriteFile(filepath.Join(staging, "usr", "bin", "roothide-helper"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: " + pkg + "\nVersion: " + ver + "\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: pkgmirror test\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return in, origBytes
}

// TestPkgmirrorInstallContract pins the two consumption paths of a
// --pkgmirror conversion against the upstream reference semantics:
//
//  1. The dpkg install path: the .deb itself unpacks to a package whose
//     control carries the dynamic-mode edits (arm64e arch, ~roothide
//     version, patches-<pkg> Pre-Depends) and whose payload Mach-Os are
//     patched to @loader_path/.jbroot.
//  2. The pkgmirror path: var/mobile/Library/pkgmirror is a snapshot of the
//     hoisted-but-unmodified package (upstream copies it before the control
//     seds and excludes it from the Mach-O patch walk, patch.sh lines
//     233-237/252) — so its DEBIAN.<pkg>/control must carry the ORIGINAL
//     fields, its payload must be byte-complete vs the input, its dylib
//     must keep the original /var/jb load commands, it must not contain
//     itself, and every entry is 0755 (upstream chmod).
func TestPkgmirrorInstallContract(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	in, origBytes := buildRootlessFixture(t, tmp, "pkgmirrortweak", "2.0")

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, true, "dynamic"); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(unpacked, "var", "mobile", "Library", "pkgmirror")
	if _, err := os.Stat(filepath.Join(mirror, "DEBIAN.pkgmirrortweak", "control")); err != nil {
		t.Fatalf("pkgmirror control missing: %v", err)
	}

	// --- Path 1: the dpkg-installable package (real control, patched payload).

	realCtl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	rc := string(realCtl)
	for _, want := range []string{
		"Architecture: " + RoothideOutputArch,
		"Version: 2.0~roothide",
		"patches-pkgmirrortweak(= 2.0~roothide)",
	} {
		if !strings.Contains(rc, want) {
			t.Errorf("real control missing %q; got:\n%s", want, rc)
		}
	}
	installed := filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "PkgMirrorTweak.dylib")
	deps, err := macho.Bin{Path: installed}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies(installed): %v", err)
	}
	if !strings.Contains(strings.Join(deps, "\n"), "@loader_path/.jbroot/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate") {
		t.Errorf("installed dylib not patched to .jbroot; deps:\n%s", strings.Join(deps, "\n"))
	}

	// --- Path 2: the pkgmirror snapshot (original control, unpatched payload).

	mirCtl, err := os.ReadFile(filepath.Join(mirror, "DEBIAN.pkgmirrortweak", "control"))
	if err != nil {
		t.Fatal(err)
	}
	mc := string(mirCtl)
	// The snapshot keeps the INPUT package's control: arm64 arch, plain
	// version, no Pre-Depends — the dynamic edits only hit the real control.
	for _, want := range []string{
		"Package: pkgmirrortweak",
		"Version: 2.0\n",
		"Architecture: iphoneos-arm64\n",
	} {
		if !strings.Contains(mc, want) {
			t.Errorf("mirror control should keep the original field %q; got:\n%s", want, mc)
		}
	}
	for _, mustNot := range []string{"iphoneos-arm64e", "~roothide", "Pre-Depends"} {
		if strings.Contains(mc, mustNot) {
			t.Errorf("mirror control must not carry dynamic edits (%q); got:\n%s", mustNot, mc)
		}
	}

	// Payload byte-completeness vs the input fixture.
	mirDylib := filepath.Join(mirror, "Library", "MobileSubstrate", "DynamicLibraries", "PkgMirrorTweak.dylib")
	mirBytes, err := os.ReadFile(mirDylib)
	if err != nil {
		t.Fatalf("mirror payload missing: %v", err)
	}
	if !bytes.Equal(mirBytes, origBytes) {
		t.Error("mirror dylib differs from the input fixture — the snapshot must be a faithful copy")
	}
	if got, err := os.ReadFile(filepath.Join(mirror, "rootfs", "usr", "bin", "roothide-helper")); err != nil || string(got) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("mirror system file under rootfs/ missing or altered: %v", err)
	}

	// The mirror's dylib keeps the ORIGINAL /var/jb load commands (excluded
	// from the patch walk, matching upstream line 252).
	mdeps, err := macho.Bin{Path: mirDylib}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies(mirror): %v", err)
	}
	if !strings.Contains(strings.Join(mdeps, "\n"), "/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate") {
		t.Errorf("mirror dylib was patched — the snapshot must keep /var/jb deps; deps:\n%s", strings.Join(mdeps, "\n"))
	}
	mrpaths, err := macho.Bin{Path: mirDylib}.Rpaths()
	if err != nil {
		t.Fatalf("Rpaths(mirror): %v", err)
	}
	if !strings.Contains(strings.Join(mrpaths, "\n"), "/var/jb/usr/lib") {
		t.Errorf("mirror dylib rpath was patched; rpaths:\n%s", strings.Join(mrpaths, "\n"))
	}

	// The mirror must not contain itself (no nested pkgmirror), and every
	// entry is 0755 (upstream chmod -R 0755, line 354).
	err = filepath.WalkDir(mirror, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(mirror, path)
		if err != nil {
			return err
		}
		// A nested pkgmirror would appear as a rel path containing the
		// literal var/mobile/Library/pkgmirror segment (the package name
		// "pkgmirrortweak" also contains the substring, so match the full
		// path); the mirror root itself is rel ".".
		if strings.Contains(rel, filepath.Join("var", "mobile", "Library", "pkgmirror")) {
			t.Errorf("mirror contains a nested pkgmirror path: %s", path)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mirror file %s mode = %v, want 0755", path, info.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
