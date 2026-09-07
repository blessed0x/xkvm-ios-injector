package app

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/artifact"
	"github.com/blessed0x/xkvm-ios-injector/internal/ipa"
	"github.com/blessed0x/xkvm-ios-injector/internal/log"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
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

func TestExtractArtifactsWritesManifest(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	in := zipApp(t, map[string]string{
		"Payload/Test.app/Regram.dylib":                   "rootdylib",
		"Payload/Test.app/Frameworks/Sparkle.dylib":       "fwdylib",
		"Payload/Test.app/Sparkle.bundle/Contents/info":   "bundle",
		"Payload/Test.app/PlugIns/OpenInRegram.appex/exe": "appex",
	})
	outDir := filepath.Join(t.TempDir(), "out")
	if err := ExtractArtifacts(in, outDir); err != nil {
		t.Fatalf("ExtractArtifacts() error = %v", err)
	}
	m, err := artifact.ReadManifest(filepath.Join(outDir, manifestName))
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	got := map[string]artifact.Placement{}
	for _, e := range m.Artifacts {
		got[e.Name] = e.Placement
	}
	want := map[string]artifact.Placement{
		"Regram.dylib":       artifact.PlacementRoot,
		"Sparkle.dylib":      artifact.PlacementFrameworks,
		"Sparkle.bundle":     artifact.PlacementRoot,
		"OpenInRegram.appex": artifact.PlacementPlugins,
	}
	for name, wantPlacement := range want {
		if got[name] != wantPlacement {
			t.Errorf("placement of %q = %q, want %q", name, got[name], wantPlacement)
		}
	}
	if m.Source != in {
		t.Errorf("manifest source = %q, want %q", m.Source, in)
	}
}

func TestRootDylibsFromManifests(t *testing.T) {
	tmp := t.TempDir()
	// An extraction dir with a manifest marking RootLib as app-root and
	// FrameLib as frameworks.
	extractDir := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &artifact.Manifest{
		Format: 1,
		Artifacts: []artifact.ManifestEntry{
			{Name: "RootLib.dylib", Kind: "dylib", Placement: artifact.PlacementRoot},
			{Name: "FrameLib.dylib", Kind: "dylib", Placement: artifact.PlacementFrameworks},
			{Name: "Assets.bundle", Kind: "bundle", Placement: artifact.PlacementRoot},
		},
	}
	if err := artifact.WriteManifest(filepath.Join(extractDir, manifestName), m); err != nil {
		t.Fatal(err)
	}

	roots, err := rootDylibsFromManifests([]string{
		filepath.Join(extractDir, "RootLib.dylib"),
		filepath.Join(extractDir, "FrameLib.dylib"),
	})
	if err != nil {
		t.Fatalf("rootDylibsFromManifests: %v", err)
	}
	if len(roots) != 1 || roots[0] != "RootLib.dylib" {
		t.Fatalf("roots = %v, want [RootLib.dylib]", roots)
	}

	// A dylib with no manifest anywhere above it yields nothing.
	noManifest := filepath.Join(t.TempDir(), "plain.dylib")
	if err := os.WriteFile(noManifest, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if roots, err := rootDylibsFromManifests([]string{noManifest}); err != nil || len(roots) != 0 {
		t.Fatalf("no-manifest dir: roots = %v, err = %v, want empty", roots, err)
	}

	// A corrupt manifest must not abort; it just yields nothing.
	corrupt := filepath.Join(tmp, "corrupt")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, manifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if roots, err := rootDylibsFromManifests([]string{filepath.Join(corrupt, "X.dylib")}); err != nil || len(roots) != 0 {
		t.Fatalf("corrupt manifest: roots = %v, err = %v, want empty/nil", roots, err)
	}
}

// TestExtractReinjectHonorsPlacement is the money test: extract a fixture app
// that ships a root dylib (Regram-style) plus a Frameworks dylib, then
// re-inject from the extraction dir with no --root-dylib flag and confirm the
// @executable_path contract is restored automatically.
func TestExtractReinjectHonorsPlacement(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()

	// Fixture app: RootLib.dylib at the app root, FrameLib.dylib in
	// Frameworks/.
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	rootLib := testutil.MakeTweak(t, filepath.Join(tmp, "build"), "RootLib")
	frameLib := testutil.MakeTweak(t, filepath.Join(tmp, "build"), "FrameLib")
	if err := os.Rename(rootLib, filepath.Join(appDir, "RootLib.dylib")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(frameLib, filepath.Join(appDir, "Frameworks", "FrameLib.dylib")); err != nil {
		t.Fatal(err)
	}
	srcIPA := filepath.Join(tmp, "src.ipa")
	testutil.MakeIPA(t, appDir, srcIPA)

	// 1. extract — manifest must record both placements.
	extractDir := filepath.Join(tmp, "extract")
	if err := ExtractArtifacts(srcIPA, extractDir); err != nil {
		t.Fatalf("ExtractArtifacts: %v", err)
	}
	m, err := artifact.ReadManifest(filepath.Join(extractDir, manifestName))
	if err != nil {
		t.Fatalf("manifest missing after extract: %v", err)
	}
	placements := map[string]artifact.Placement{}
	for _, e := range m.Artifacts {
		placements[e.Name] = e.Placement
	}
	if placements["RootLib.dylib"] != artifact.PlacementRoot || placements["FrameLib.dylib"] != artifact.PlacementFrameworks {
		t.Fatalf("manifest placements = %v", placements)
	}

	// 2. fresh base app, re-inject with -f pointing into the extract dir.
	baseDir := testutil.MakeApp(t, tmp, "BaseApp", "com.example.base")
	baseIPA := filepath.Join(tmp, "base.ipa")
	testutil.MakeIPA(t, baseDir, baseIPA)
	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:    baseIPA,
		Output:   out,
		Files:    []string{filepath.Join(extractDir, "RootLib.dylib"), filepath.Join(extractDir, "FrameLib.dylib")},
		Fakesign: true,
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// 3. the output main binary must load RootLib via @executable_path and
	// FrameLib via @rpath — placement restored with no explicit flag.
	outApp, err := ipa.Extract(out, t.TempDir())
	if err != nil {
		t.Fatalf("extracting output ipa: %v", err)
	}
	deps, err := (macho.Bin{Path: filepath.Join(outApp, "BaseApp")}).Dependencies()
	if err != nil {
		t.Fatalf("reading output deps: %v", err)
	}
	hasRoot, hasFrame := false, false
	for _, d := range deps {
		if d == "@executable_path/RootLib.dylib" {
			hasRoot = true
		}
		if d == "@rpath/FrameLib.dylib" {
			hasFrame = true
		}
	}
	if !hasRoot {
		t.Errorf("output deps missing @executable_path/RootLib.dylib: %v", deps)
	}
	if !hasFrame {
		t.Errorf("output deps missing @rpath/FrameLib.dylib: %v", deps)
	}
}

func TestRunReturnsValidationErrors(t *testing.T) {
	o := &Options{Input: "bad.xyz"}
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("expected Run to surface validation errors")
	}
}

// writeCyan is a minimal .cyan archive builder for the apply-flow validation
// hook tests.
func writeCyan(t *testing.T, path, config string, entries map[string]string) {
	t.Helper()
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRunRejectsInvalidCyanConfig: a -z config that fails cyan-check (a
// root_dylibs entry with no matching inject payload) aborts the run BEFORE
// any output is produced.
func TestRunRejectsInvalidCyanConfig(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "bad.cyan")
	writeCyan(t, bad, `{"f": true, "root_dylibs": ["Nope.dylib"]}`, map[string]string{
		"inject/Other.dylib": "other",
	})
	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:  zipApp(t, nil),
		Output: out,
		Cyans:  []string{bad},
	}
	err := Run(context.Background(), o)
	if err == nil {
		t.Fatal("expected Run to reject an invalid -z config")
	}
	if !strings.Contains(err.Error(), "cyan-check") {
		t.Errorf("error = %q, want a cyan-check mention", err.Error())
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("output was produced despite the invalid config")
	}
}

// TestRunRejectsUnknownPatchInCyanConfig: a config referencing a patch this
// build doesn't have is rejected up front, not mid-pipeline after partial
// work (patch.Apply would fail later with less context).
func TestRunRejectsUnknownPatchInCyanConfig(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "badpatch.cyan")
	writeCyan(t, bad, `{"patches": ["made-up-patch"]}`, map[string]string{
		"inject/x.dylib": "x",
	})
	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:  zipApp(t, nil),
		Output: out,
		Cyans:  []string{bad},
	}
	err := Run(context.Background(), o)
	if err == nil {
		t.Fatal("expected Run to reject a config with an unknown patch")
	}
	if !strings.Contains(err.Error(), "cyan-check") {
		t.Errorf("error = %q, want a cyan-check mention", err.Error())
	}
}

// TestRunAcceptsWarningsOnlyCyanConfig: warning-level findings (an unknown
// config key — forward-compatible by design) must NOT block the apply; the
// run completes and produces output.
func TestRunAcceptsWarningsOnlyCyanConfig(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()
	warn := filepath.Join(tmp, "warn.cyan")
	// No inject/ payloads: the fixture's placeholder main binary isn't a real
	// Mach-O, and injection would try to edit it. The warnings-only config
	// exercises the plist rename path, which the placeholder handles fine.
	writeCyan(t, warn, `{"future_feature": true, "n": "WarnApp"}`, nil)
	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:  zipApp(t, nil),
		Output: out,
		Cyans:  []string{warn},
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v (warnings must not block the apply)", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output missing after warnings-only config: %v", err)
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
