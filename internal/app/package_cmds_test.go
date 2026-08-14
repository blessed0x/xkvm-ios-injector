package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/artifact"
	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/plist"
	"github.com/xkvm/xkvm/internal/testutil"
)

// TestDebifyThenUndebRoundTrip exercises the dylib→deb→dylib loop: Debify
// wraps a real tweak dylib into a MobileSubstrate deb (DynamicLibraries
// placement + Filter/Bundles plist), and Undeb extracts it back with the
// placement manifest.
func TestDebifyThenUndebRoundTrip(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	tweak := testutil.MakeTweak(t, tmp, "Hooker")

	debOut := filepath.Join(tmp, "hooker.deb")
	if err := Debify(DebifyOptions{
		Input:     tweak,
		Output:    debOut,
		BundleIDs: []string{"com.example.app"},
		Version:   "2.0",
	}); err != nil {
		t.Fatalf("Debify: %v", err)
	}
	if _, err := os.Stat(debOut); err != nil {
		t.Fatalf("deb not written: %v", err)
	}

	// The payload lands in DynamicLibraries under the sanitized package name.
	extracted := filepath.Join(tmp, "extracted")
	arts, err := deb.Extract(debOut, extracted)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	if filepath.Base(arts[0]) != "hooker.dylib" {
		t.Errorf("unexpected artifact path %q", arts[0])
	}

	// The generated filter plist names the target bundle.
	filterPath := filepath.Join(extracted, "Library", "MobileSubstrate", "DynamicLibraries", "hooker.plist")
	filter, err := plist.Open(filterPath)
	if err != nil {
		t.Fatalf("filter plist missing: %v", err)
	}
	bundles, ok := filter["Filter"].(plist.Dict)["Bundles"].([]any)
	if !ok || len(bundles) != 1 || bundles[0] != "com.example.app" {
		t.Errorf("filter Bundles wrong: %#v", filter["Filter"])
	}

	// Control file (full unpack to see DEBIAN/).
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(debOut, unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Package: hooker", "Version: 2.0", "Architecture: iphoneos-arm64", "Depends: mobilesubstrate"} {
		if !strings.Contains(string(ctl), want) {
			t.Errorf("control missing %q; got:\n%s", want, ctl)
		}
	}

	// Undeb: extract artifacts + placement manifest for re-injection.
	undebDir := filepath.Join(tmp, "undeb")
	if err := Undeb(debOut, undebDir); err != nil {
		t.Fatalf("Undeb: %v", err)
	}
	if _, err := os.Stat(filepath.Join(undebDir, "hooker.dylib")); err != nil {
		t.Errorf("undeb did not export the dylib: %v", err)
	}
	m, err := artifact.ReadManifest(filepath.Join(undebDir, manifestName))
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if len(m.Artifacts) != 1 || m.Artifacts[0].Name != "hooker.dylib" {
		t.Errorf("manifest wrong: %+v", m.Artifacts)
	}
}

// TestRootlessOnDebifyOutput converts a deb produced by Debify and verifies
// the rootless layout + control edits at the app level.
func TestRootlessOnDebifyOutput(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	tweak := testutil.MakeTweak(t, tmp, "Hooker")

	rootful := filepath.Join(tmp, "rootful.deb")
	if err := Debify(DebifyOptions{Input: tweak, Output: rootful}); err != nil {
		t.Fatalf("Debify: %v", err)
	}
	rootlessOut := filepath.Join(tmp, "rootless.deb")
	if err := Rootless(rootful, rootlessOut, false); err != nil {
		t.Fatalf("Rootless: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(rootlessOut, unpacked); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "hooker.dylib")
	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("rootless payload missing at %s: %v", payload, err)
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctl), "Architecture: iphoneos-arm64") {
		t.Errorf("control missing iphoneos-arm64:\n%s", ctl)
	}
	if !strings.Contains(string(ctl), "cy+cpu.arm64v8") {
		t.Errorf("control missing rootless runtime dep:\n%s", ctl)
	}
	// The dylib itself is still a valid Mach-O after conversion.
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "hooker.dylib")); err != nil {
		t.Errorf("dylib missing: %v", err)
	}
}

// TestDebifyResourceAndDirectoryInput: --resource adds extra payload files
// and a directory input is copied wholesale as the payload root.
func TestDebifyResourceAndDirectoryInput(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	tweak := testutil.MakeTweak(t, tmp, "Hooker")

	extra := filepath.Join(tmp, "Extra.bundle")
	os.MkdirAll(filepath.Join(extra, "Contents"), 0o755)
	os.WriteFile(filepath.Join(extra, "Contents", "Info.plist"), []byte("<plist/>"), 0o644)

	out := filepath.Join(tmp, "hooker.deb")
	if err := Debify(DebifyOptions{
		Input:     tweak,
		Output:    out,
		Resources: []string{extra + ":Library/Application Support/Hooker.bundle"},
	}); err != nil {
		t.Fatalf("Debify: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "Library", "Application Support", "Hooker.bundle", "Contents", "Info.plist")); err != nil {
		t.Errorf("--resource payload missing: %v", err)
	}

	// Directory input: copied verbatim as the payload root.
	dirOut := filepath.Join(tmp, "dir.deb")
	payloadDir := filepath.Join(tmp, "payloadroot")
	os.MkdirAll(filepath.Join(payloadDir, "usr", "lib"), 0o755)
	os.WriteFile(filepath.Join(payloadDir, "usr", "lib", "libx.dylib"), []byte("x"), 0o644)
	if err := Debify(DebifyOptions{Input: payloadDir, Output: dirOut}); err != nil {
		t.Fatalf("Debify(dir): %v", err)
	}
	if err := deb.Unpack(dirOut, tmp+"/dir-unpacked"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "dir-unpacked", "usr", "lib", "libx.dylib")); err != nil {
		t.Errorf("directory payload missing: %v", err)
	}
}
