package macho

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

func newTestBin(t *testing.T) Bin {
	t.Helper()
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	return Bin{Path: testutil.GoBuild(t, tmp, "exe", testutil.MainSource)}
}

func TestDependenciesAndInjectWeak(t *testing.T) {
	b := newTestBin(t)
	deps, err := b.Dependencies()
	if err != nil {
		t.Fatalf("Dependencies: %v", err)
	}
	// A plain Go executable must link libSystem and friends.
	found := false
	for _, d := range deps {
		if strings.Contains(d, "/usr/lib/") || strings.HasPrefix(d, "@") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a system dependency in %v", deps)
	}

	// Inject a weak LC_LOAD_DYLIB and confirm it shows up.
	tweak := filepath.Join(filepath.Dir(b.Path), "Tweak.dylib")
	if err := os.WriteFile(tweak, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.InjectWeak("@rpath/Tweak.dylib"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	deps, err = b.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(deps, "@rpath/Tweak.dylib") {
		t.Errorf("missing injected dep, got %v", deps)
	}
}

// TestDependenciesSkipsInstallName is the regression test for the ID-line
// skip: a real dylib's own LC_ID_DYLIB install name (which can look like a
// dependency, e.g. /Library/MobileSubstrate/...) must not be reported as one.
func TestDependenciesSkipsInstallName(t *testing.T) {
	b := newTestBin(t)
	id := "/Library/MobileSubstrate/DynamicLibraries/MyTweak.dylib"
	if err := b.SetInstallName(id); err != nil {
		t.Fatalf("SetInstallName: %v", err)
	}
	deps, err := b.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if contains(deps, id) {
		t.Errorf("own install name %q treated as a dependency: %v", id, deps)
	}
}

func TestChangeDependency(t *testing.T) {
	b := newTestBin(t)
	deps, err := b.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) == 0 {
		t.Skip("no dependencies to change")
	}
	target := deps[0]
	replacement := "@rpath/Changed.dylib"
	if err := b.ChangeDependency(target, replacement); err != nil {
		t.Fatalf("ChangeDependency: %v", err)
	}
	deps, err = b.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(deps, replacement) {
		t.Errorf("expected %q in deps, got %v", replacement, deps)
	}
	if contains(deps, target) {
		t.Errorf("expected %q gone, got %v", target, deps)
	}
}

func TestFakesignRoundTrip(t *testing.T) {
	b := newTestBin(t)
	if err := b.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}
	if err := b.Fakesign(); err != nil {
		t.Fatalf("Fakesign: %v", err)
	}
	// A bare fakesign writes a signature with no entitlements, so
	// ExtractEntitlements returns empty — but the signature itself must be
	// readable.
	if !b.IsSigned() {
		t.Error("expected a readable code signature after fakesign")
	}
}

func TestSignWithEntitlements(t *testing.T) {
	b := newTestBin(t)
	ents := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>get-task-allow</key><true/></dict></plist>`)
	if err := b.SignWithEntitlements(ents); err != nil {
		t.Fatalf("SignWithEntitlements: %v", err)
	}
	got, err := b.ExtractEntitlements()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "get-task-allow") {
		t.Errorf("expected get-task-allow in entitlements, got %s", got)
	}
}

func TestIsEncryptedAndArchitectures(t *testing.T) {
	b := newTestBin(t)
	enc, err := b.IsEncrypted()
	if err != nil {
		t.Fatalf("IsEncrypted: %v", err)
	}
	if enc {
		t.Error("a fresh Go binary must not be encrypted")
	}
	archs, err := b.Architectures()
	if err != nil {
		t.Fatal(err)
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("expected thin arm64, got %v", archs)
	}
	if err := b.ThintoArm64(); err != nil {
		t.Fatalf("ThintoArm64 on thin arm64: %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
