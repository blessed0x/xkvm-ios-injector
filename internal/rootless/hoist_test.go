package rootless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

// TestRoothideHoistNoVar pins the no-var/ hoist case: an iphoneos-arm64
// deb whose payload is NOT under var/jb (a "claims rootless but isn't"
// package) must hoist cleanly — every top-level entry joins rootfs/. Pure
// Go (no Mach-O), so it runs on every CI leg. Before the fix, the no-var
// else-branch treated Remove(var)'s IsNotExist as "var exists but is
// non-empty" and put a phantom "var" in the loose set, failing the later
// rename with ENOENT.
func TestRoothideHoistNoVar(t *testing.T) {
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: novartweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: no var test\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	// Payload at the package root, NOT under var/jb — and no var/ at all.
	os.MkdirAll(filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries"), 0o755)
	if err := os.WriteFile(filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries", "NovaTweak.dylib"), []byte("not a real dylib, just a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "novar.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "rootfs", "Library", "MobileSubstrate", "DynamicLibraries", "NovaTweak.dylib")); err != nil {
		t.Fatalf("payload missing under rootfs/: %v", err)
	}
}

// TestHoistVarCollision pins the hoist's var/ handling against upstream's
// documented case: "some packages have both /var/jb/var/xxx and /var/xxx,
// same file same name". The jbroot copy must win at the package root while
// the system copy lands under rootfs/var/ — and a var/ with no jb at all
// (pure system content) must also go under rootfs/var/.
func TestHoistVarCollision(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	// --- Scenario 1: var/jb/var/xxx AND var/xxx in the same package.
	tweak := testutil.MakeTweak(t, tmp, "HoistTweak")
	b := macho.Bin{Path: tweak}
	if err := b.InjectWeak("/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}

	staging := filepath.Join(tmp, "staging")
	payloadDir := filepath.Join(staging, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, "HoistTweak.dylib")); err != nil {
		t.Fatal(err)
	}
	// jbroot var content (the copy that must win at the package root).
	jbVar := filepath.Join(staging, "var", "jb", "var", "mobile", "Library", "Preferences")
	os.MkdirAll(jbVar, 0o755)
	os.WriteFile(filepath.Join(jbVar, "jb-copy.txt"), []byte("jbroot copy\n"), 0o644)
	// system var content alongside the jbroot (the documented collision).
	sysVar := filepath.Join(staging, "var", "mobile", "Library", "Preferences")
	os.MkdirAll(sysVar, 0o755)
	os.WriteFile(filepath.Join(sysVar, "system-copy.txt"), []byte("system copy\n"), 0o644)
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	os.WriteFile(filepath.Join(staging, "DEBIAN", "control"),
		[]byte("Package: hoistcollision\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: hoist test\nMaintainer: xkvm\n"), 0o644)

	in := filepath.Join(tmp, "in.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := filepath.Join(tmp, "out.deb")
	if err := ConvertToRoothide(in, out, false, "dynamic"); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(unpacked, "var", "mobile", "Library", "Preferences")
	if got, err := os.ReadFile(filepath.Join(root, "jb-copy.txt")); err != nil || string(got) != "jbroot copy\n" {
		t.Errorf("jbroot copy missing at package root: %v (got %q)", err, got)
	}
	if _, err := os.Stat(filepath.Join(root, "system-copy.txt")); err == nil {
		t.Error("system copy leaked into the package root — jbroot content must win there")
	}
	rootfs := filepath.Join(unpacked, "rootfs", "var", "mobile", "Library", "Preferences")
	if got, err := os.ReadFile(filepath.Join(rootfs, "system-copy.txt")); err != nil || string(got) != "system copy\n" {
		t.Errorf("system copy missing under rootfs/var: %v (got %q)", err, got)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "jb-copy.txt")); err == nil {
		t.Error("jbroot copy duplicated under rootfs — only system content belongs there")
	}

	// The tweak hoisted to the package root and patched to .jbroot.
	conv := filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "HoistTweak.dylib")
	deps, err := macho.Bin{Path: conv}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies: %v", err)
	}
	if !strings.Contains(strings.Join(deps, "\n"), "@loader_path/.jbroot/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate") {
		t.Errorf("hoisted tweak not patched to .jbroot; deps:\n%s", strings.Join(deps, "\n"))
	}

	// --- Scenario 2: var/ with NO jb — pure system content goes to rootfs/var.
	staging2 := filepath.Join(tmp, "staging2")
	sysVar2 := filepath.Join(staging2, "var", "mobile", "Library", "Preferences")
	os.MkdirAll(sysVar2, 0o755)
	os.WriteFile(filepath.Join(sysVar2, "system.txt"), []byte("system\n"), 0o644)
	os.MkdirAll(filepath.Join(staging2, "DEBIAN"), 0o755)
	os.WriteFile(filepath.Join(staging2, "DEBIAN", "control"),
		[]byte("Package: jblessvar\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: jbless var\nMaintainer: xkvm\n"), 0o644)
	in2 := filepath.Join(tmp, "in2.deb")
	if err := deb.Build(staging2, in2); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out2 := filepath.Join(tmp, "out2.deb")
	if err := ConvertToRoothide(in2, out2, false, "dynamic"); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked2 := filepath.Join(tmp, "unpacked2")
	if err := deb.Unpack(out2, unpacked2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unpacked2, "var")); err == nil {
		t.Error("jb-less system var left at the package root — must move under rootfs/var")
	}
	if got, err := os.ReadFile(filepath.Join(unpacked2, "rootfs", "var", "mobile", "Library", "Preferences", "system.txt")); err != nil || string(got) != "system\n" {
		t.Errorf("jb-less system var missing under rootfs/var: %v", err)
	}
}
