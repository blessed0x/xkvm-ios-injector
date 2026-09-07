package rootless

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/log"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

// buildRootfulFixture builds a rootful deb whose tweak links the classic
// rootful runtime set plus a sibling dylib, with the given install name.
// Returns the deb path and the tweak's basename.
func buildRootfulFixture(t *testing.T, tmp, name, installName string, deps []string) string {
	t.Helper()
	tweak := testutil.MakeTweak(t, tmp, name)
	b := macho.Bin{Path: tweak}
	if installName != "" {
		if err := b.SetInstallName(installName); err != nil {
			t.Fatalf("SetInstallName: %v", err)
		}
	}
	for _, dep := range deps {
		if err := b.InjectWeak(dep); err != nil {
			t.Fatalf("InjectWeak(%s): %v", dep, err)
		}
	}

	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: " + strings.ToLower(name) + "\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: test tweak\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	payloadDir := filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, name+".dylib")); err != nil {
		t.Fatal(err)
	}
	rootful := filepath.Join(tmp, "rootful.deb")
	if err := deb.Build(staging, rootful); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return rootful
}

// TestConvertTweakInject: the --tweakinject mode moves DynamicLibraries to
// usr/lib/TweakInject, rewrites CydiaSubstrate-family deps to
// @rpath/libsubstrate.dylib, turns the install name into @rpath/<basename>,
// and adds the /usr/lib + /var/jb/usr/lib rpaths (Derootifier conventions).
func TestConvertTweakInject(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	rootful := buildRootfulFixture(t, tmp, "CoolTweak",
		"/Library/MobileSubstrate/DynamicLibraries/CoolTweak.dylib",
		[]string{
			"/usr/lib/libsubstrate.dylib",
			"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
			"/Library/MobileSubstrate/DynamicLibraries/Sibling.dylib",
		})

	out := filepath.Join(tmp, "tweakinject.deb")
	if err := Convert(rootful, out, false, true); err != nil {
		t.Fatalf("Convert(tweakinject): %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// Layout: DynamicLibraries moved under usr/lib/TweakInject.
	converted := filepath.Join(unpacked, "var", "jb", "usr", "lib", "TweakInject", "CoolTweak.dylib")
	if _, err := os.Stat(converted); err != nil {
		t.Fatalf("TweakInject payload missing at %s: %v", converted, err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")); err == nil {
		t.Fatal("legacy DynamicLibraries dir still present")
	}

	deps, err := macho.Bin{Path: converted}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies: %v", err)
	}
	got := strings.Join(deps, "\n")
	// Substrate-family deps become the ellekit shim; a plain sibling dep
	// stays a /var/jb path (only substrate-family deps get the @rpath shim).
	for _, want := range []string{
		"@rpath/libsubstrate.dylib",
		"/var/jb/Library/MobileSubstrate/DynamicLibraries/Sibling.dylib",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted deps missing %q; got:\n%s", want, got)
		}
	}
	for _, mustNot := range []string{
		"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
		"/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
		"/usr/lib/libsubstrate.dylib",
		"/var/jb/usr/lib/libsubstrate.dylib",
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("dep %q must be rewritten to the substrate shim; got:\n%s", mustNot, got)
		}
	}

	// Install name becomes @rpath/<basename>.
	id, err := macho.Bin{Path: converted}.InstallName()
	if err != nil {
		t.Fatalf("InstallName: %v", err)
	}
	if id != "@rpath/CoolTweak.dylib" {
		t.Errorf("install name = %q, want @rpath/CoolTweak.dylib", id)
	}

	// The /usr/lib + /var/jb/usr/lib rpaths are present.
	rpaths, err := macho.Bin{Path: converted}.Rpaths()
	if err != nil {
		t.Fatalf("Rpaths: %v", err)
	}
	rp := strings.Join(rpaths, "\n")
	for _, want := range []string{"/usr/lib", "/var/jb/usr/lib"} {
		if !strings.Contains(rp, want) {
			t.Errorf("rpaths missing %q; got:\n%s", want, rp)
		}
	}
}

// TestConvertRoothide: the roothide direction hoists var/jb/* to the package
// root, moves remaining payload under rootfs/, rewrites /var/jb load
// commands and rpaths to @loader_path/.jbroot, and re-archs the control to
// iphoneos-arm64e. Also exercises the dynamic mode version/Pre-Depends edits.
func TestConvertRoothide(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	// A rootless deb: payload under var/jb with a /var/jb dep + rpath, plus a
	// loose system file that must land under rootfs/.
	tweak := testutil.MakeTweak(t, tmp, "RoothideTweak")
	b := macho.Bin{Path: tweak}
	if err := b.InjectWeak("/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	if err := b.AddRpath("/var/jb/usr/lib"); err != nil {
		t.Fatalf("AddRpath: %v", err)
	}

	staging := filepath.Join(tmp, "staging")
	payloadDir := filepath.Join(staging, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, "RoothideTweak.dylib")); err != nil {
		t.Fatal(err)
	}
	// A loose system file (not under var/jb) goes to rootfs/ on conversion.
	os.MkdirAll(filepath.Join(staging, "usr", "bin"), 0o755)
	os.WriteFile(filepath.Join(staging, "usr", "bin", "roothide-helper"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: roothidetweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: roothide test\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, true, "dynamic"); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// var/jb hoisted: the tweak now sits at the package root's DynamicLibraries.
	converted := filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "RoothideTweak.dylib")
	if _, err := os.Stat(converted); err != nil {
		t.Fatalf("hoisted payload missing at %s: %v", converted, err)
	}
	// Loose system file under rootfs/.
	if _, err := os.Stat(filepath.Join(unpacked, "rootfs", "usr", "bin", "roothide-helper")); err != nil {
		t.Fatalf("system file missing under rootfs/: %v", err)
	}
	// No var/jb anywhere.
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb")); err == nil {
		t.Fatal("var/jb still present after hoist")
	}

	// Load commands rewritten to @loader_path/.jbroot.
	deps, err := macho.Bin{Path: converted}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies: %v", err)
	}
	got := strings.Join(deps, "\n")
	if !strings.Contains(got, "@loader_path/.jbroot/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate") {
		t.Errorf("dep not rewritten to .jbroot; got:\n%s", got)
	}
	if strings.Contains(got, "/var/jb/") {
		t.Errorf("raw /var/jb dep survived; got:\n%s", got)
	}
	rpaths, err := macho.Bin{Path: converted}.Rpaths()
	if err != nil {
		t.Fatalf("Rpaths: %v", err)
	}
	rp := strings.Join(rpaths, "\n")
	if !strings.Contains(rp, "@loader_path/.jbroot/usr/lib") {
		t.Errorf("rpath not rewritten to .jbroot; got:\n%s", rp)
	}
	if strings.Contains(rp, "/var/jb/") {
		t.Errorf("raw /var/jb rpath survived; got:\n%s", rp)
	}

	// Control: arm64e arch + dynamic mode edits.
	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	if !strings.Contains(ctl, "Architecture: "+RoothideOutputArch) {
		t.Errorf("control missing iphoneos-arm64e; got:\n%s", ctl)
	}
	if !strings.Contains(ctl, "Version: 1.0~roothide") {
		t.Errorf("dynamic mode missing ~roothide version suffix; got:\n%s", ctl)
	}
	if !strings.Contains(ctl, "patches-roothidetweak(= 1.0~roothide)") {
		t.Errorf("dynamic mode missing Pre-Depends; got:\n%s", ctl)
	}

	// pkgmirror: var/mobile/Library/pkgmirror with the control dir renamed.
	mirror := filepath.Join(unpacked, "var", "mobile", "Library", "pkgmirror")
	if _, err := os.Stat(filepath.Join(mirror, "DEBIAN.roothidetweak", "control")); err != nil {
		t.Fatalf("pkgmirror control missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "Library", "MobileSubstrate", "DynamicLibraries", "RoothideTweak.dylib")); err != nil {
		t.Fatalf("pkgmirror payload missing: %v", err)
	}
}

// TestWarnFixedPaths: the post-conversion audit reports a surviving rootful
// load-command dependency and rootful paths inside text payload files.
func TestWarnFixedPaths(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	// Mach-O still linking a rootful jailbreak path (a conversion miss).
	tweak := testutil.MakeTweak(t, tmp, "Leftover")
	b := macho.Bin{Path: tweak}
	if err := b.InjectWeak("/Library/MobileSubstrate/DynamicLibraries/Leftover.dylib"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	// A text payload file containing a rootful path (not rewritten).
	os.MkdirAll(filepath.Join(tmp, "payload", "Library", "MobileSubstrate", "DynamicLibraries"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "payload", "Library", "PreferenceLoader", "Preferences"), 0o755)
	if err := copyFile(t, tweak, filepath.Join(tmp, "payload", "Library", "MobileSubstrate", "DynamicLibraries", "Leftover.dylib")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tmp, "payload", "Library", "PreferenceLoader", "Preferences", "Leftover.plist"),
		[]byte("<?xml version=\"1.0\"?>\n<plist version=\"1.0\"><dict><key>entry</key><string>/Library/PreferenceLoader/Preferences/Leftover</string></dict></plist>\n"), 0o644)

	var buf bytes.Buffer
	log.SetWriters(&buf, &buf)
	defer log.SetWriters(os.Stdout, os.Stderr)

	if err := WarnFixedPaths(filepath.Join(tmp, "payload")); err != nil {
		t.Fatalf("WarnFixedPaths: %v", err)
	}
	got := buf.String()
	// Byte-identical to upstream's echo -e output: the Mach-O dep gets a
	// banner with NO `=>` line, the text plist gets `=>` + banner; no log
	// prefix, no summary line (patch.sh lines 341-347, spelling included).
	for _, block := range []string{
		"*****fixed-paths-warnning*****\n/Library/MobileSubstrate/DynamicLibraries/Leftover.dylib\n*******************************",
		"=> /var/jb/Library/PreferenceLoader/Preferences/Leftover.plist\n*****fixed-paths-warnning*****\n/Library/PreferenceLoader/Preferences/Leftover\n*******************************",
	} {
		if !strings.Contains(got, block) {
			t.Errorf("expected the exact upstream advisory block %q; output:\n%s", block, got)
		}
	}
	if strings.Contains(got, "[?] *****fixed-paths-warnning") || strings.Contains(got, "fixed-paths:") {
		t.Errorf("advisory must be raw (no [?] prefix) and have no xkvm summary line; output:\n%s", got)
	}
}
