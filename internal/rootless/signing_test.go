package rootless

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/testutil"
)

// dylibSource is a minimal c-shared Go source: `go build -buildmode=c-shared`
// emits a real MH_DYLIB (testutil.MakeTweak's plain `go build` emits an
// MH_EXECUTE named *.dylib, which would take the executable branch).
const dylibSource = `package main
import "C"
//export probe
func probe() {}
func main() {}
`

// goBuildShared builds a real MH_DYLIB (c-shared) at dir/name.
func goBuildShared(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "shared_main.go")
	if err := os.WriteFile(src, []byte(dylibSource), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	if b, err := exec.Command("go", "build", "-buildmode=c-shared", "-o", out, src).CombinedOutput(); err != nil {
		t.Fatalf("go build -buildmode=c-shared: %v: %s", err, b)
	}
	return out
}

// TestRoothideSigning pins the roothide conversion's signing step against
// upstream's ldid pass (patch.sh): executables are re-signed with the
// roothide platform entitlements MERGED over any existing ones (upstream
// replaces wholesale — the merge is the deliberate improvement), and
// non-executables get a plain ad-hoc signature (upstream's `-S`).
// Native toolchain only (real Mach-O fixtures).
func TestRoothideSigning(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")

	// Executable, pre-signed with a custom entitlement (proves the merge).
	exe := testutil.GoBuild(t, filepath.Join(tmp, "build"), "ProbeExec", testutil.MainSource)
	customEnts := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.private.custom-probe</key><true/></dict></plist>`
	if err := (macho.Bin{Path: exe}).SignWithEntitlements([]byte(customEnts)); err != nil {
		t.Fatalf("pre-sign executable: %v", err)
	}

	// Real dylib, left unsigned.
	dylib := goBuildShared(t, filepath.Join(tmp, "build"), "ProbeDylib.dylib")

	exeRel := "var/jb/Library/PreferenceBundles/ProbeTweak.bundle/ProbeExec"
	dylibRel := "var/jb/Library/MobileSubstrate/DynamicLibraries/ProbeDylib.dylib"
	for _, f := range []struct{ rel, src string }{
		{exeRel, exe},
		{dylibRel, dylib},
	} {
		full := filepath.Join(staging, f.rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(f.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctl := "Package: signingprobe\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: signing test\nMaintainer: xkvm\n"
	if err := os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(ctl), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless.deb")
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

	// The hoist moved var/jb/* to the package root — the converted binary
	// lives at Library/..., not var/jb/Library/...
	const jbPrefix = "var/jb/"
	exeOut := filepath.Join(unpacked, strings.TrimPrefix(exeRel, jbPrefix))
	dylibOut := filepath.Join(unpacked, strings.TrimPrefix(dylibRel, jbPrefix))

	// Executable: signed, custom entitlement preserved, roothide base present.
	b := macho.Bin{Path: exeOut}
	if !b.IsSigned() {
		t.Fatalf("converted executable is not signed")
	}
	ents, err := b.ExtractEntitlements()
	if err != nil {
		t.Fatalf("extract entitlements: %v", err)
	}
	for _, want := range []string{
		"platform-application",
		"com.apple.private.security.no-sandbox",
		"com.apple.private.security.storage.AppBundles",
		"com.apple.private.security.storage.AppDataContainers",
		"com.apple.private.custom-probe",
	} {
		if !strings.Contains(string(ents), "<key>"+want+"</key>") {
			t.Errorf("converted executable missing entitlement %s; got:\n%s", want, ents)
		}
	}

	// Dylib: signed, no entitlements (upstream's plain `-S` — the slot is
	// absent, so ExtractEntitlements returns empty bytes, not an error).
	d := macho.Bin{Path: dylibOut}
	if !d.IsSigned() {
		t.Fatalf("converted dylib is not signed")
	}
	if ents, err := d.ExtractEntitlements(); err == nil && len(ents) > 0 {
		t.Errorf("converted dylib should carry no entitlements; got %q", ents)
	}
}

// TestRootlessSigning pins the ROOTLESS converter's re-sign step against
// Derootifier's ldid pass (the same patch.sh function): executables get the
// roothide platform entitlements merged over the entitlements captured
// BEFORE the early signature strip, non-executables get a plain ad-hoc
// signature. Native toolchain only.
func TestRootlessSigning(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")

	// Executable, pre-signed with a custom entitlement. The rootless
	// converter strips the old signature BEFORE editing — the custom key
	// surviving the round-trip proves the pre-strip capture feeds the merge.
	exe := testutil.GoBuild(t, filepath.Join(tmp, "build"), "ProbeExec", testutil.MainSource)
	customEnts := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.private.custom-probe</key><true/></dict></plist>`
	if err := (macho.Bin{Path: exe}).SignWithEntitlements([]byte(customEnts)); err != nil {
		t.Fatalf("pre-sign executable: %v", err)
	}
	dylib := goBuildShared(t, filepath.Join(tmp, "build"), "ProbeDylib.dylib")

	// Rootful layout (no var/jb): Library/... directly.
	exeRel := "Library/PreferenceBundles/ProbeTweak.bundle/ProbeExec"
	dylibRel := "Library/MobileSubstrate/DynamicLibraries/ProbeDylib.dylib"
	for _, f := range []struct{ rel, src string }{
		{exeRel, exe},
		{dylibRel, dylib},
	} {
		full := filepath.Join(staging, f.rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(f.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctl := "Package: signingprobe\nVersion: 1.0\nArchitecture: iphoneos-arm\nDepends: mobilesubstrate\nDescription: signing test\nMaintainer: xkvm\n"
	if err := os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(ctl), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootful.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := filepath.Join(tmp, "rootless.deb")
	if err := Convert(in, out, false, false); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// Rootless layout: payload under var/jb.
	const jbPrefix = "var/jb/"
	exeOut := filepath.Join(unpacked, jbPrefix+exeRel)
	dylibOut := filepath.Join(unpacked, jbPrefix+dylibRel)

	// Executable: signed, custom entitlement preserved (pre-strip capture),
	// roothide base present.
	b := macho.Bin{Path: exeOut}
	if !b.IsSigned() {
		t.Fatalf("converted rootless executable is not signed")
	}
	ents, err := b.ExtractEntitlements()
	if err != nil {
		t.Fatalf("extract entitlements: %v", err)
	}
	for _, want := range []string{
		"platform-application",
		"com.apple.private.security.no-sandbox",
		"com.apple.private.security.storage.AppBundles",
		"com.apple.private.security.storage.AppDataContainers",
		"com.apple.private.custom-probe",
	} {
		if !strings.Contains(string(ents), "<key>"+want+"</key>") {
			t.Errorf("converted rootless executable missing entitlement %s; got:\n%s", want, ents)
		}
	}

	// Dylib: signed, no entitlements.
	d := macho.Bin{Path: dylibOut}
	if !d.IsSigned() {
		t.Fatalf("converted rootless dylib is not signed")
	}
	if ents, err := d.ExtractEntitlements(); err == nil && len(ents) > 0 {
		t.Errorf("converted rootless dylib should carry no entitlements; got %q", ents)
	}
}
