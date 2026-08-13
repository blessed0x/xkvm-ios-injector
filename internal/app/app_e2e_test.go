package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/artifact"
	"github.com/xkvm/xkvm/internal/cyanfile"
	"github.com/xkvm/xkvm/internal/ipa"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/patch"
	"github.com/xkvm/xkvm/internal/plist"
	"github.com/xkvm/xkvm/internal/testutil"
)

// TestRunFullPipelineE2E runs the entire xkvm pipeline against a real IPA:
// inject a Mach-O tweak, rewrite metadata, fakesign and thin everything, then
// verify the output by extracting and inspecting it.
func TestRunFullPipelineE2E(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	ipaPath := filepath.Join(tmp, "in.ipa")
	testutil.MakeIPA(t, appDir, ipaPath)

	tweak := testutil.MakeTweak(t, tmp, "MyTweak")

	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:     ipaPath,
		Output:    out,
		Files:     []string{tweak},
		Name:      "Fancy App",
		Version:   "9.9.9",
		BundleID:  "com.example.fancy",
		MinimumOS: "15.0",
		Fakesign:  true,
		Thin:      true,
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Extract the output and verify the injection + metadata landed.
	outApp, err := ipa.Extract(out, filepath.Join(tmp, "extract"))
	if err != nil {
		t.Fatalf("extracting output: %v", err)
	}
	mainExe := filepath.Join(outApp, "TestApp")
	deps, err := macho.Bin{Path: mainExe}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(deps, " "), "@rpath/MyTweak.dylib") {
		t.Errorf("output main binary missing injected dep, got %v", deps)
	}
	if _, err := os.Stat(filepath.Join(outApp, "Frameworks", "MyTweak.dylib")); err != nil {
		t.Errorf("tweak not in output Frameworks: %v", err)
	}

	info, err := plist.Open(filepath.Join(outApp, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"CFBundleDisplayName": "Fancy App",
		"CFBundleVersion":     "9.9.9",
		"CFBundleIdentifier":  "com.example.fancy",
		"MinimumOSVersion":    "15.0",
	} {
		if got := info[key]; got != want {
			t.Errorf("output plist %s = %v, want %v", key, got, want)
		}
	}

	// --fakesign must leave the main binary with a readable signature.
	if !(macho.Bin{Path: mainExe}).IsSigned() {
		t.Error("output main binary not fakesigned")
	}

	// And `xkvm extract` must be able to dump the tweak back out.
	artDir := filepath.Join(tmp, "arts")
	if err := ExtractArtifacts(out, artDir); err != nil {
		t.Fatalf("ExtractArtifacts: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDir, "MyTweak.dylib")); err != nil {
		t.Errorf("extract didn't find injected tweak: %v", err)
	}
}

// TestRunElleKitAndLiquidGlassE2E runs the full pipeline with --ellekit and
// --liquid-glass: the substrate-spelled tweak's dependency must be rewritten
// to the ElleKit runtime, ElleKit.framework auto-injected (thinned to the
// app's arch), no CydiaSubstrate in the output, and the Liquid Glass patch
// applied to Info.plist + the main executable's SDK.
func TestRunElleKitAndLiquidGlassE2E(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	ipaPath := filepath.Join(tmp, "in.ipa")
	testutil.MakeIPA(t, appDir, ipaPath)

	tweak := testutil.MakeTweak(t, tmp, "SubstrateTweak")
	// Link the tweak against the classic MobileSubstrate spelling so the
	// ElleKit-mode rewrite is exercised end to end.
	if err := (macho.Bin{Path: tweak}).InjectWeak("/Library/MobileSubstrate/MobileSubstrate.dylib"); err != nil {
		t.Fatalf("InjectWeak MobileSubstrate: %v", err)
	}

	out := filepath.Join(tmp, "out.ipa")
	o := &Options{
		Input:    ipaPath,
		Output:   out,
		Files:    []string{tweak},
		Fakesign: true,
		ElleKit:  true,
		Patches:  []string{"liquid-glass"},
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	outApp, err := ipa.Extract(out, filepath.Join(tmp, "extract"))
	if err != nil {
		t.Fatalf("extracting output: %v", err)
	}

	// (a) the tweak's MobileSubstrate dependency was rewritten to ElleKit.
	tweakOut := filepath.Join(outApp, "Frameworks", "SubstrateTweak.dylib")
	tweakDeps, err := macho.Bin{Path: tweakOut}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(tweakDeps, " "), "@rpath/ElleKit.framework/ElleKit") {
		t.Errorf("tweak deps not rewritten to ElleKit runtime, got %v", tweakDeps)
	}
	if strings.Contains(strings.Join(tweakDeps, " "), "MobileSubstrate") {
		t.Errorf("tweak still references MobileSubstrate, got %v", tweakDeps)
	}

	// (b) ElleKit.framework auto-injected, thinned to the app arch (arm64),
	// and no CydiaSubstrate.framework present.
	ek := filepath.Join(outApp, "Frameworks", "ElleKit.framework", "ElleKit")
	if _, err := os.Stat(ek); err != nil {
		t.Fatalf("ElleKit.framework not in output: %v", err)
	}
	archs, err := macho.Bin{Path: ek}.Architectures()
	if err != nil {
		t.Fatalf("ElleKit architectures: %v", err)
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("ElleKit not thinned to arm64, got %v", archs)
	}
	if _, err := os.Stat(filepath.Join(outApp, "Frameworks", "CydiaSubstrate.framework")); err == nil {
		t.Error("CydiaSubstrate.framework present in --ellekit output")
	}

	// (c) Liquid Glass: Info.plist key set to false.
	info, err := plist.Open(filepath.Join(outApp, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := info["UIDesignRequiresCompatibility"].(bool); !ok || got {
		t.Errorf("UIDesignRequiresCompatibility = %v, want false", info["UIDesignRequiresCompatibility"])
	}

	// (d) Liquid Glass: main executable SDK bumped to 26.0.0.
	mainExe := filepath.Join(outApp, "TestApp")
	b := macho.Bin{Path: mainExe}
	sdk, err := b.SDKVersion()
	if err != nil {
		t.Fatalf("reading SDK version: %v", err)
	}
	if sdk != "26.0" {
		t.Errorf("main executable SDK = %q, want 26.0", sdk)
	}

	// (e) signatures validate after the patch (patch ran before fakesign).
	if !b.IsSigned() {
		t.Error("main binary not signed after patch + fakesign")
	}
}

// TestRunDebInjectionE2E builds a real .deb whose payload contains a dylib,
// injects it into an app, and verifies the dylib landed in Frameworks with an
// LC_LOAD_DYLIB on the main binary.
func TestRunDebInjectionE2E(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })

	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	ipaPath := filepath.Join(tmp, "in.ipa")
	testutil.MakeIPA(t, appDir, ipaPath)

	// The tweak shipped inside a deb payload (the standard MobileSubstrate
	// layout tweaks actually use).
	tweak := testutil.MakeTweak(t, tmp, "DebTweak")
	tweakBytes, err := os.ReadFile(tweak)
	if err != nil {
		t.Fatal(err)
	}
	debPath := filepath.Join(tmp, "tweak.deb")
	writeDeb(t, debPath, map[string][]byte{
		"./Library/MobileSubstrate/DynamicLibraries/DebTweak.dylib": tweakBytes,
	})

	out := filepath.Join(tmp, "out.ipa")
	o := &Options{Input: ipaPath, Output: out, Files: []string{debPath}}
	if err := Run(context.Background(), o); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	outApp, err := ipa.Extract(out, filepath.Join(tmp, "extract"))
	if err != nil {
		t.Fatalf("extracting output: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outApp, "Frameworks", "DebTweak.dylib")); err != nil {
		t.Errorf("deb-injected tweak missing from Frameworks: %v", err)
	}
	deps, err := macho.Bin{Path: filepath.Join(outApp, "TestApp")}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(deps, " "), "@rpath/DebTweak.dylib") {
		t.Errorf("main binary missing deb-injected dep, got %v", deps)
	}
}

// TestCyanCheckGoldenExtractedSet is the golden end-to-end for cyan-check
// against a REAL extracted tweak set (macos-14/arm64 CI leg only): build a
// fixture app carrying a root dylib + a Frameworks dylib + a bundle, extract
// them with the production ExtractArtifacts, generate a .cyan config from the
// extracted files with cyanfile.Generate, and Validate it with the real patch
// registry — zero error-level issues. Then the golden negative: the same
// archive with config.json's root_dylibs rewritten to a missing basename must
// produce exactly one error.
func TestCyanCheckGoldenExtractedSet(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()

	// 1. Fixture app: RootLib.dylib at the app root, FrameLib.dylib in
	// Frameworks/, Assets.bundle next to the binary.
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
	bundle := filepath.Join(appDir, "Assets.bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "info"), []byte("assets"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcIPA := filepath.Join(tmp, "src.ipa")
	testutil.MakeIPA(t, appDir, srcIPA)

	// 2. Extract with the production path; the manifest must record both
	// placements (root + frameworks) for the real dylibs.
	extractDir := filepath.Join(tmp, "extract")
	if err := ExtractArtifacts(srcIPA, extractDir); err != nil {
		t.Fatalf("ExtractArtifacts: %v", err)
	}
	m, err := artifact.ReadManifest(filepath.Join(extractDir, "xkvm-manifest.json"))
	if err != nil {
		t.Fatalf("manifest missing after extract: %v", err)
	}
	placements := map[string]artifact.Placement{}
	for _, e := range m.Artifacts {
		placements[e.Name] = e.Placement
	}
	if placements["RootLib.dylib"] != artifact.PlacementRoot || placements["FrameLib.dylib"] != artifact.PlacementFrameworks || placements["Assets.bundle"] != artifact.PlacementRoot {
		t.Fatalf("manifest placements = %v", placements)
	}

	// 3. Generate a .cyan config from the real extracted dylibs, marking the
	// root one, and validate it with the real patch registry. The bundle is
	// intentionally left out: inject/ payloads are flat basenames by design
	// (upstream cgen parity + the zip-slip guard), so cgen can't yet bundle
	// directory payloads — tracked as a separate limitation.
	cyan := filepath.Join(tmp, "golden.cyan")
	files := []string{
		filepath.Join(extractDir, "RootLib.dylib"),
		filepath.Join(extractDir, "FrameLib.dylib"),
	}
	if err := cyanfile.Generate(cyanfile.GenerateOptions{
		Output:     cyan,
		Files:      files,
		RootDylibs: files[:1],
		Name:       "GoldenApp",
		Fakesign:   true,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	knownPatches := map[string]bool{}
	for _, n := range patch.Names() {
		knownPatches[n] = true
	}
	issues, err := cyanfile.Validate(cyan, knownPatches)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, is := range issues {
		if is.Level == cyanfile.IssueError {
			t.Errorf("generated config failed cyan-check: %s", is.Message)
		}
	}

	// 4. Golden negative: same archive, config.json's root_dylibs rewritten
	// to a basename with no inject payload. Exactly one error.
	broken := filepath.Join(tmp, "broken.cyan")
	rewriteCyanConfig(t, cyan, broken, func(cfg map[string]any) {
		cfg["root_dylibs"] = []string{"Missing.dylib"}
	})
	issues, err = cyanfile.Validate(broken, knownPatches)
	if err != nil {
		t.Fatalf("Validate(broken): %v", err)
	}
	errs := 0
	for _, is := range issues {
		if is.Level == cyanfile.IssueError {
			errs++
			if !strings.Contains(is.Message, "Missing.dylib") {
				t.Errorf("unexpected error: %s", is.Message)
			}
		}
	}
	if errs != 1 {
		t.Errorf("broken config produced %d error(s), want exactly 1", errs)
	}
}

// rewriteCyanConfig copies src to dst, mutating config.json in place.
func rewriteCyanConfig(t *testing.T, src, dst string, mutate func(map[string]any)) {
	t.Helper()
	zin, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zin.Close()
	zout, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zout)
	for _, f := range zin.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == "config.json" {
			var cfg map[string]any
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			mutate(cfg)
			if data, err = json.Marshal(cfg); err != nil {
				t.Fatal(err)
			}
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zout.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeDeb writes a minimal ar archive with a single data.tar.gz member
// containing files (relative tar name -> content).
func writeDeb(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(data)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("!<arch>\n"); err != nil {
		t.Fatal(err)
	}
	writeArMember(f, "data.tar.gz", gzBuf.Bytes())
}

func writeArMember(w io.Writer, name string, data []byte) {
	hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
	if _, err := io.WriteString(w, hdr); err != nil {
		return
	}
	_, _ = w.Write(data)
	if len(data)%2 == 1 {
		_, _ = w.Write([]byte("\n"))
	}
}
