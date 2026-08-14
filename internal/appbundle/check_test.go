package appbundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/macho"
	"github.com/xscope0/xkvm-ios-injector/internal/testutil"
)

// TestCheckReferencesFlagsMissingFramework is the ffmpegkit-gap reproducer: a
// tweak referencing @rpath/ffmpegkit.framework/ffmpegkit that the app doesn't
// ship must be flagged.
func TestCheckReferencesFlagsMissingFramework(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "RyukGram")
	if err := (macho.Bin{Path: tweak}).InjectWeak("@rpath/ffmpegkit.framework/ffmpegkit"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want exactly the ffmpegkit reference", refs)
	}
	if !strings.Contains(refs[0].Dep, "ffmpegkit.framework") {
		t.Errorf("ref = %+v, want a ffmpegkit.framework reference", refs[0])
	}
	if !strings.Contains(refs[0].From, "RyukGram.dylib") {
		t.Errorf("ref source = %q, want RyukGram.dylib", refs[0].From)
	}
}

// TestCheckReferencesCleanWithDependencyShipped: once the referenced framework
// is actually in Frameworks/, the same reference resolves and nothing is
// flagged — and system paths (/usr/lib) are never flagged.
func TestCheckReferencesCleanWithDependencyShipped(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// Ship ffmpegkit.framework with a real Mach-O binary inside.
	fwDir := filepath.Join(appDir, "Frameworks", "ffmpegkit.framework")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fwBin := testutil.MakeTweak(t, tmp, "ffmpegkit")
	if err := os.Rename(fwBin, filepath.Join(fwDir, "ffmpegkit")); err != nil {
		t.Fatal(err)
	}

	tweak := testutil.MakeTweak(t, tmp, "RyukGram")
	for _, dep := range []string{"@rpath/ffmpegkit.framework/ffmpegkit", "/usr/lib/libSystem.B.dylib"} {
		if err := (macho.Bin{Path: tweak}).InjectWeak(dep); err != nil {
			t.Fatalf("InjectWeak(%s): %v", dep, err)
		}
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none (framework shipped, system path ignored)", refs)
	}
}

// TestCheckReferencesSwiftRuntimeShimAllowed: an @rpath/libswift*.dylib load
// command must never be flagged — dyld resolves those via the OS's
// /usr/lib/swift on iOS 12.2+, apps never bundle them. Without the allowlist
// every Swift-linked tweak would false-positive.
func TestCheckReferencesSwiftRuntimeShimAllowed(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "SwiftTweak")
	for _, dep := range []string{"@rpath/libswiftFoundation.dylib", "@rpath/libswiftCore.dylib"} {
		if err := (macho.Bin{Path: tweak}).InjectWeak(dep); err != nil {
			t.Fatalf("InjectWeak(%s): %v", dep, err)
		}
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "SwiftTweak.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none (Swift runtime shims resolve via the OS)", refs)
	}
}

// appendStrings appends printable payload lines to a Mach-O file so a test
// can simulate runtime-dlopen strings without relying on the Go linker
// keeping an unused string literal (dead-code elimination would drop it).
// Trailing data after the load commands is legal Mach-O and parse-safe.
func appendStrings(path string, lines ...string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("\n")
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	data = append(data, sb.String()...)
	return os.WriteFile(path, data, 0o755)
}

// TestCheckReferencesSuspectedDlopenFlagged: the tier-2 heuristic — a bare
// NAME.framework token in a non-main binary (the runtime-dlopen signature;
// RyukGram dlopens "ffmpegkit.framework" with no load command) whose
// framework the app doesn't ship is flagged with Suspected=true.
func TestCheckReferencesSuspectedDlopenFlagged(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "RyukGram")
	if err := appendStrings(tweak, "ffmpegkit.framework/ffmpegkit"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want exactly the suspected dlopen of ffmpegkit.framework", refs)
	}
	r := refs[0]
	if !r.Suspected || !strings.Contains(r.Dep, "ffmpegkit.framework") {
		t.Errorf("ref = %+v, want a Suspected ffmpegkit.framework dlopen finding", r)
	}
}

// TestCheckReferencesSuspectedCleanWhenShippedOrSystem mirrors the WORKING
// FIXED2 layout: ffmpegkit.framework shipped at the app ROOT (the SK-style
// placement) satisfies RyukGram's dlopen; FLEX.framework nested inside
// Regram.bundle satisfies Regram's dlopen (same tweak family — the RC build
// worked); UIKit.framework is a system framework. Nothing is flagged.
func TestCheckReferencesSuspectedCleanWhenShippedOrSystem(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// Ship ffmpegkit.framework at the app ROOT (the SK-style placement).
	fwDir := filepath.Join(appDir, "ffmpegkit.framework")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fwBin := testutil.MakeTweak(t, tmp, "ffmpegkit")
	if err := os.Rename(fwBin, filepath.Join(fwDir, "ffmpegkit")); err != nil {
		t.Fatal(err)
	}

	// Ship FLEX.framework nested inside Regram.bundle (the RC-style
	// placement — FLEX.framework lives at Regram.bundle/FLEX.framework).
	nestedDir := filepath.Join(appDir, "Regram.bundle", "FLEX.framework")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flexBin := testutil.MakeTweak(t, tmp, "FLEX")
	if err := os.Rename(flexBin, filepath.Join(nestedDir, "FLEX")); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}

	// RyukGram.dylib (Frameworks/) dlopens ffmpegkit (shipped at root →
	// reachable) and names UIKit (system).
	ryuk := testutil.MakeTweak(t, tmp, "RyukGram")
	if err := appendStrings(ryuk, "ffmpegkit.framework/ffmpegkit", "UIKit.framework/UIKit"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ryuk, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	// Regram.dylib (app root, family Regram) dlopens FLEX (nested in
	// Regram.bundle — same family → reachable).
	regram := testutil.MakeTweak(t, tmp, "Regram")
	if err := appendStrings(regram, "FLEX.framework/FLEX"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(regram, filepath.Join(appDir, "Regram.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none (root + same-family nested shipped; system ignored)", refs)
	}
}

// TestCheckReferencesSuspectedNestedNotReachableCrossFamily is the FIXED-IPA
// reproducer: ffmpegkit.framework exists ONLY nested inside Regram.bundle
// (came over with the RC base), but RyukGram.dylib's dlopen searches
// Frameworks//its own bundle — a different family. The nested copy does NOT
// satisfy it, so the reference must be flagged (this exact layout crashed on
// device).
func TestCheckReferencesSuspectedNestedNotReachableCrossFamily(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	// Ship ffmpegkit.framework ONLY nested inside Regram.bundle.
	nestedDir := filepath.Join(appDir, "Regram.bundle", "ffmpegkit.framework")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fwBin := testutil.MakeTweak(t, tmp, "ffmpegkit")
	if err := os.Rename(fwBin, filepath.Join(nestedDir, "ffmpegkit")); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	ryuk := testutil.MakeTweak(t, tmp, "RyukGram")
	if err := appendStrings(ryuk, "ffmpegkit.framework/ffmpegkit"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ryuk, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %v, want exactly the ffmpegkit reference (nested in another family's bundle is unreachable)", refs)
	}
	if !refs[0].Suspected || !strings.Contains(refs[0].Dep, "ffmpegkit.framework") {
		t.Errorf("ref = %+v, want a Suspected ffmpegkit.framework finding", refs[0])
	}
}

// TestCheckReferencesGenericAppNotTiedToInstagram: the check is app-agnostic.
// A fully generic app + tweak + framework (StockApp / CoolTweak /
// MissingMedia — no Instagram-family naming anywhere) must behave identically
// to the RyukGram case: the missing framework is flagged (deterministically,
// tier 1), and shipping it resolves the reference. This pins the guarantee
// that nothing in the check keys on the motivating example's names.
func TestCheckReferencesGenericAppNotTiedToInstagram(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "StockApp", "com.example.stockapp")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "CoolTweak")
	if err := (macho.Bin{Path: tweak}).InjectWeak("@rpath/MissingMedia.framework/MissingMedia"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "CoolTweak.dylib")); err != nil {
		t.Fatal(err)
	}

	// Missing framework -> flagged, tier-1 (not Suspected), naming the gap.
	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 1 || refs[0].Suspected || !strings.Contains(refs[0].Dep, "MissingMedia.framework") {
		t.Fatalf("refs = %v, want exactly the deterministic MissingMedia reference", refs)
	}

	// Ship MissingMedia.framework -> the same reference resolves, clean.
	fwDir := filepath.Join(appDir, "Frameworks", "MissingMedia.framework")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fwBin := testutil.MakeTweak(t, tmp, "MissingMedia")
	if err := os.Rename(fwBin, filepath.Join(fwDir, "MissingMedia")); err != nil {
		t.Fatal(err)
	}
	refs, err = b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none once MissingMedia.framework is shipped", refs)
	}
}

// TestCheckReferencesExecutablePathRoot: a root-placed dylib's
// @executable_path reference resolves against the app root; absent target is
// flagged, present target is clean.
func TestCheckReferencesExecutablePathRoot(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	rootLib := testutil.MakeTweak(t, tmp, "RootLib")
	if err := os.Rename(rootLib, filepath.Join(appDir, "RootLib.dylib")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "Loader")
	if err := (macho.Bin{Path: tweak}).InjectWeak("@executable_path/RootLib.dylib"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "Loader.dylib")); err != nil {
		t.Fatal(err)
	}

	b, err := Open(appDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if refs, err := b.CheckReferences(); err != nil || len(refs) != 0 {
		t.Fatalf("with RootLib present: refs = %v, err = %v, want none", refs, err)
	}

	// Remove the root dylib; the same reference must now be flagged.
	if err := os.Remove(filepath.Join(appDir, "RootLib.dylib")); err != nil {
		t.Fatal(err)
	}
	refs, err := b.CheckReferences()
	if err != nil {
		t.Fatalf("CheckReferences: %v", err)
	}
	if len(refs) != 1 || !strings.Contains(refs[0].Dep, "RootLib.dylib") {
		t.Errorf("refs = %v, want the @executable_path/RootLib.dylib reference", refs)
	}
}
