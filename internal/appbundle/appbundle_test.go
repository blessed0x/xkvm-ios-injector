package appbundle

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/plist"
	"github.com/xkvm/xkvm/internal/testutil"
)

func openBundle(t *testing.T, appDir string) *Bundle {
	t.Helper()
	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b
}

func TestMetadataEdits(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	b := openBundle(t, appDir)

	// Give it an appex to test bundle-id propagation.
	extDir := filepath.Join(appDir, "PlugIns", "Ext.appex")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := plist.Write(filepath.Join(extDir, "Info.plist"), plist.Dict{
		"CFBundleExecutable": "Ext",
		"CFBundleIdentifier": "com.example.test.ext",
	}); err != nil {
		t.Fatal(err)
	}

	if err := b.ChangeName("New Name"); err != nil {
		t.Fatalf("ChangeName: %v", err)
	}
	if err := b.ChangeVersion("2.5.1"); err != nil {
		t.Fatalf("ChangeVersion: %v", err)
	}
	if err := b.ChangeBundleID("com.example.changed"); err != nil {
		t.Fatalf("ChangeBundleID: %v", err)
	}
	if err := b.ChangeMinimumOS("14.0"); err != nil {
		t.Fatalf("ChangeMinimumOS: %v", err)
	}

	info, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"CFBundleName":               "New Name",
		"CFBundleDisplayName":        "New Name",
		"CFBundleVersion":            "2.5.1",
		"CFBundleShortVersionString": "2.5.1",
		"CFBundleIdentifier":         "com.example.changed",
		"MinimumOSVersion":           "14.0",
	} {
		if got := info[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}

	// The appex bundle id must have been propagated.
	extInfo, err := plist.Open(filepath.Join(extDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if got := extInfo["CFBundleIdentifier"]; got != "com.example.changed.ext" {
		t.Errorf("appex bundle id = %v, want com.example.changed.ext", got)
	}
}

func TestPlistMergeAndFlags(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// Pre-seed UISupportedDevices to prove removal.
	b := openBundle(t, appDir)
	b.Info["UISupportedDevices"] = []any{"iPhone12,1"}
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}

	mergePath := filepath.Join(tmp, "merge.plist")
	if err := plist.Write(mergePath, plist.Dict{"ITSAppUsesNonExemptEncryption": false}); err != nil {
		t.Fatal(err)
	}
	if err := b.MergePlist(mergePath); err != nil {
		t.Fatalf("MergePlist: %v", err)
	}
	if err := b.RemoveUISupportedDevices(); err != nil {
		t.Fatalf("RemoveUISupportedDevices: %v", err)
	}
	if err := b.EnableDocuments(); err != nil {
		t.Fatalf("EnableDocuments: %v", err)
	}

	info, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := info["ITSAppUsesNonExemptEncryption"]; !ok || v != false {
		t.Errorf("merge key missing or wrong: %v", info["ITSAppUsesNonExemptEncryption"])
	}
	if _, ok := info["UISupportedDevices"]; ok {
		t.Error("UISupportedDevices still present")
	}
	for _, k := range []string{"UISupportsDocumentBrowser", "UIFileSharingEnabled"} {
		if v, ok := info[k]; !ok || v != true {
			t.Errorf("%s not enabled: %v", k, info[k])
		}
	}
}

func TestRemoveWatchAndExtensions(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	for _, d := range []string{"Watch", "WatchKit", "Extensions", "PlugIns"} {
		if err := os.MkdirAll(filepath.Join(appDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := openBundle(t, appDir)
	if err := b.RemoveWatchApps(); err != nil {
		t.Fatalf("RemoveWatchApps: %v", err)
	}
	if err := b.RemoveAllExtensions(); err != nil {
		t.Fatalf("RemoveAllExtensions: %v", err)
	}
	for _, d := range []string{"Watch", "WatchKit", "Extensions", "PlugIns"} {
		if _, err := os.Stat(filepath.Join(appDir, d)); !os.IsNotExist(err) {
			t.Errorf("%s still exists", d)
		}
	}
}

func TestChangeIcon(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// A small solid-color PNG.
	iconPath := filepath.Join(tmp, "icon.png")
	f, err := os.Create(iconPath)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{0, 120, 255, 255})
		}
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := openBundle(t, appDir)
	if err := b.ChangeIcon(iconPath); err != nil {
		t.Fatalf("ChangeIcon: %v", err)
	}

	entries, err := os.ReadDir(appDir)
	if err != nil {
		t.Fatal(err)
	}
	var pngs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			pngs = append(pngs, e.Name())
		}
	}
	if len(pngs) != 2 {
		t.Errorf("expected 2 icon PNGs, got %v", pngs)
	}

	info, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	icons, _ := info["CFBundleIcons"].(map[string]any)
	if icons == nil {
		t.Fatal("CFBundleIcons missing")
	}
	primary, _ := icons["CFBundlePrimaryIcon"].(map[string]any)
	if primary == nil {
		t.Fatal("CFBundlePrimaryIcon missing")
	}
	name, _ := primary["CFBundleIconName"].(string)
	if !strings.HasPrefix(name, "cyan_") || !strings.HasSuffix(name, "a") {
		t.Errorf("unexpected icon name %q", name)
	}
}

func TestMassFakesignAndThin(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// Inject a tweak alongside to make the mass operations work on 2 binaries.
	fwDir := filepath.Join(appDir, "Frameworks")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "MyTweak")
	if err := os.Rename(tweak, filepath.Join(fwDir, "MyTweak.dylib")); err != nil {
		t.Fatal(err)
	}

	b := openBundle(t, appDir)
	n, err := b.FakesignAll()
	if err != nil {
		t.Fatalf("FakesignAll: %v", err)
	}
	if n < 2 {
		t.Errorf("expected >=2 fakesigned binaries, got %d", n)
	}
	// After fakesigning the signature must be readable.
	if !b.Main.IsSigned() {
		t.Error("main not signed after FakesignAll")
	}

	if n, err := b.ThinAll(); err != nil {
		t.Fatalf("ThinAll: %v", err)
	} else if n < 2 {
		t.Errorf("expected >=2 thinned binaries, got %d", n)
	}
	archs, err := macho.Bin{Path: b.Main.Path}.Architectures()
	if err != nil {
		t.Fatal(err)
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("main not thin arm64 after ThinAll: %v", archs)
	}
}
