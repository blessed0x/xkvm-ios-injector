package patch

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	gmacho "github.com/blacktop/go-macho"
	"github.com/xscope0/xkvm-ios-injector/internal/plist"
	"github.com/xscope0/xkvm-ios-injector/internal/testutil"
)

func TestNames(t *testing.T) {
	got := Names()
	want := []string{"force-fullscreen", "liquid-glass", "liquid-glass-compat"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

func TestForceFullScreen(t *testing.T) {
	// The patch itself is plist-only, but the MakeApp fixture is built with
	// the native toolchain, so guard like every other test in this package.
	testutil.SkipUnlessNativeToolchain(t)
	appDir := testutil.MakeApp(t, t.TempDir(), "TestApp", "com.example.test")

	if err := Apply(appDir, []string{"force-fullscreen"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. UIRequiresFullScreen is set to true.
	d, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := d["UIRequiresFullScreen"].(bool); !ok || !v {
		t.Errorf("UIRequiresFullScreen = %v, want true", d["UIRequiresFullScreen"])
	}

	// 2. Unknown keys survive the round-trip (plist.Dict is a map).
	if got := d["CFBundleIdentifier"]; got != "com.example.test" {
		t.Errorf("CFBundleIdentifier = %v, want com.example.test", got)
	}
}

func TestLiquidGlassEnable(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	appDir := testutil.MakeApp(t, t.TempDir(), "TestApp", "com.example.test")
	main := filepath.Join(appDir, "TestApp")

	if err := Apply(appDir, []string{"liquid-glass"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. Info.plist opt-out key is false (liquid glass enabled).
	d, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := d["UIDesignRequiresCompatibility"].(bool); !ok || v {
		t.Errorf("UIDesignRequiresCompatibility = %v, want false", d["UIDesignRequiresCompatibility"])
	}

	// 2. Main executable SDK bumped to 26.0.0 and still runs.
	if got := readSDK(t, main); got != 0x1A0000 {
		t.Errorf("sdk = %#x, want 0x1A0000", got)
	}
	if out, err := exec.Command(main).CombinedOutput(); err != nil {
		t.Errorf("patched main failed to run: %v: %s", err, out)
	}
}

func TestLiquidGlassCompat(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	appDir := testutil.MakeApp(t, t.TempDir(), "TestApp", "com.example.test")
	main := filepath.Join(appDir, "TestApp")
	before := readSDK(t, main)

	if err := Apply(appDir, []string{"liquid-glass-compat"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 1. Opt-out key is true (force legacy appearance).
	d, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := d["UIDesignRequiresCompatibility"].(bool); !ok || !v {
		t.Errorf("UIDesignRequiresCompatibility = %v, want true", d["UIDesignRequiresCompatibility"])
	}

	// 2. No Mach-O change: SDK untouched.
	if got := readSDK(t, main); got != before {
		t.Errorf("compat patch changed sdk: %#x -> %#x", before, got)
	}
}

func TestApplyUnknown(t *testing.T) {
	if err := Apply(t.TempDir(), []string{"no-such-patch"}); err == nil {
		t.Fatal("expected an error for an unknown patch name")
	}
}

// readSDK returns the first LC_BUILD_VERSION sdk field (0 if absent).
func readSDK(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := gmacho.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	bvs := f.BuildVersions()
	if len(bvs) == 0 {
		return 0
	}
	return uint32(bvs[0].Sdk)
}
