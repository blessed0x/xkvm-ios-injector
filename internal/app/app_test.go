package app

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/testutil"
)

const testInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>Test</string>
  <key>CFBundleIdentifier</key><string>com.example.test</string>
  <key>CFBundleName</key><string>TestApp</string>
</dict></plist>`

// zipApp writes an .ipa containing Payload/Test.app (with the given extra
// files, if any) and returns its path.
func zipApp(t *testing.T, extras map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ipa")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	mk := func(name, content string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	mk("Payload/Test.app/Info.plist", testInfoPlist)
	mk("Payload/Test.app/Test", "\xcf\xfa\xed\xfe placeholder")
	for name, content := range extras {
		mk(name, content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeAppDir creates a .app directory on disk and returns its path.
func writeAppDir(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Test.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte(testInfoPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Test"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestRunM1ExtractsAndRepacks(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	out := filepath.Join(t.TempDir(), "out.ipa")
	o := &Options{Input: zipApp(t, nil), Output: out}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	defer zr.Close()
	found := false
	for _, f := range zr.File {
		if f.Name == "Payload/Test.app/Info.plist" {
			found = true
		}
	}
	if !found {
		t.Error("output ipa is missing Payload/Test.app/Info.plist")
	}
}

func TestRunRefusesInplaceOverwriteWithoutFlag(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	in := zipApp(t, nil)
	o := &Options{Input: in} // Output defaults to Input
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("expected a refusal to overwrite the input without --overwrite")
	}
}

func TestRunInplaceOverwriteWithFlag(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	in := zipApp(t, nil)
	o := &Options{Input: in, Overwrite: true}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(in); err != nil {
		t.Errorf("input was not rewritten: %v", err)
	}
}

func TestRunAppInputOutputsApp(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	app := writeAppDir(t)
	out := filepath.Join(t.TempDir(), "out.app")
	o := &Options{Input: app, Output: out}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "Info.plist")); err != nil {
		t.Errorf("output app is missing Info.plist: %v", err)
	}
}

func TestExtractArtifactsFromIPA(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	in := zipApp(t, map[string]string{
		"Payload/Test.app/Frameworks/MyTweak.framework/Info.plist": "fw",
		"Payload/Test.app/Frameworks/MyTweak.framework/MyTweak":    "fwbin",
		"Payload/Test.app/Frameworks/lib.dylib":                    "dylib",
	})
	outDir := filepath.Join(t.TempDir(), "out")
	if err := ExtractArtifacts(in, outDir); err != nil {
		t.Fatalf("ExtractArtifacts() error = %v", err)
	}
	for _, want := range []string{"MyTweak.framework", "lib.dylib"} {
		if _, err := os.Stat(filepath.Join(outDir, want)); err != nil {
			t.Errorf("missing extracted artifact %q: %v", want, err)
		}
	}
}

func TestExtractArtifactsRejectsBadInput(t *testing.T) {
	if err := ExtractArtifacts("foo.xyz", t.TempDir()); err == nil {
		t.Fatal("expected an error for a non app/ipa/tipa input")
	}
	if err := ExtractArtifacts("/nonexistent.ipa", t.TempDir()); err == nil {
		t.Fatal("expected an error for a nonexistent input")
	}
}

func TestRunReturnsValidationErrors(t *testing.T) {
	o := &Options{Input: "bad.xyz"}
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("expected Run to surface validation errors")
	}
}

func TestRunRejectsContradictoryLiquidGlassFlags(t *testing.T) {
	// --liquid-glass and --liquid-glass-compat contradict each other (§4.6):
	// one opts the app into the new design, the other forces the legacy one.
	in := zipApp(t, nil)
	o := &Options{
		Input:   in,
		Output:  filepath.Join(t.TempDir(), "out.ipa"),
		Patches: []string{"liquid-glass", "liquid-glass-compat"},
	}
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("expected contradictory liquid-glass flags to be rejected")
	}
}

func TestRunAcceptsSingleLiquidGlassFlag(t *testing.T) {
	// The patch's Mach-O step needs a real binary (the zipApp placeholder is
	// not parseable), so use the native-toolchain app fixture.
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	in := filepath.Join(tmp, "in.ipa")
	testutil.MakeIPA(t, appDir, in)
	o := &Options{
		Input:   in,
		Output:  filepath.Join(tmp, "out.ipa"),
		Patches: []string{"liquid-glass"},
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("single liquid-glass patch should be accepted: %v", err)
	}
}
