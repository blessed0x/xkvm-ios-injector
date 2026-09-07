package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/ipa"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

// brokenApp builds a TestApp whose Frameworks/ holds a tweak with an
// unresolved @rpath/MissingMedia.framework/MissingMedia load command, plus a
// tweak that dlopens MissingMedia.framework by string (tier-2). Returns the
// app dir.
func brokenApp(t *testing.T, tmp string) string {
	t.Helper()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Tier-1: a deterministic load-command reference to a missing framework.
	t1 := testutil.MakeTweak(t, tmp, "CoolTweak")
	if err := (macho.Bin{Path: t1}).InjectWeak("@rpath/MissingMedia.framework/MissingMedia"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(t1, filepath.Join(appDir, "Frameworks", "CoolTweak.dylib")); err != nil {
		t.Fatal(err)
	}
	addDlopenTweak(t, tmp, appDir)
	return appDir
}

// dlopenOnlyApp builds a TestApp whose only gap is a tier-2 finding: a
// non-main binary carrying the bare dlopen string, with no load command for
// the missing framework. Used to pin the tier-2 confirmation gate in
// isolation (in brokenApp the same framework is also referenced by a tier-1
// load command, so placing it for tier-1 incidentally satisfies tier-2).
func dlopenOnlyApp(t *testing.T, tmp string) string {
	t.Helper()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	addDlopenTweak(t, tmp, appDir)
	return appDir
}

// addDlopenTweak places a non-main binary whose bytes contain the bare
// dlopen string. The string is appended after the Mach-O's own data with a
// NUL boundary so the token scan reliably sees it as a bare token (the char
// before 'M' is NUL, not a letter that would make it look like part of a
// longer name).
func addDlopenTweak(t *testing.T, tmp, appDir string) {
	t.Helper()
	t2 := testutil.GoBuild(t, filepath.Join(tmp, "dlbuild"), "DlopenTweak.dylib",
		"package main\nfunc main() {}\n")
	f, err := os.OpenFile(t2, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("\x00MissingMedia.framework\x00")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if err := os.Rename(t2, filepath.Join(appDir, "Frameworks", "DlopenTweak.dylib")); err != nil {
		t.Fatal(err)
	}
}

// missingMediaFramework creates fixDir/MissingMedia.framework/MissingMedia as
// a real Mach-O so the fixer can place a genuine binary.
func missingMediaFramework(t *testing.T, tmp, fixDir string) string {
	t.Helper()
	fwDir := filepath.Join(fixDir, "MissingMedia.framework")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := testutil.MakeTweak(t, tmp, "MissingMedia")
	if err := os.Rename(bin, filepath.Join(fwDir, "MissingMedia")); err != nil {
		t.Fatal(err)
	}
	return fwDir
}

func TestCheckAndFixResolvesTier1FromFixDir(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := brokenApp(t, tmp)
	fixDir := filepath.Join(tmp, "fixes")
	missingMediaFramework(t, tmp, fixDir)

	out := filepath.Join(tmp, "TestApp-fixed.app")
	if err := CheckAndFix(appDir, out, []string{fixDir}, FixOptions{AutoYes: true}); err != nil {
		t.Fatalf("CheckAndFix: %v", err)
	}
	placed := filepath.Join(out, "Frameworks", "MissingMedia.framework", "MissingMedia")
	if _, err := os.Stat(placed); err != nil {
		t.Errorf("fixed copy missing the resolved framework at %s: %v", placed, err)
	}
	// The original input must be untouched.
	if _, err := os.Stat(filepath.Join(appDir, "Frameworks", "MissingMedia.framework")); err == nil {
		t.Error("the input app must not be modified")
	}
}

func TestCheckAndFixTier2RequiresConfirmation(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	// dlopen-only app: nothing else provides the framework, so refusing the
	// tier-2 injection genuinely leaves the gap open.
	appDir := dlopenOnlyApp(t, tmp)
	fixDir := filepath.Join(tmp, "fixes")
	missingMediaFramework(t, tmp, fixDir)

	// No AutoYes, Confirm nil → tier-2 must be refused: nothing applied, no
	// output, and the remaining reference surfaces as an error.
	out := filepath.Join(tmp, "TestApp-fixed.app")
	err := CheckAndFix(appDir, out, []string{fixDir}, FixOptions{})
	if err == nil {
		t.Fatal("expected an error: the tier-2 finding was not confirmed")
	}
	if !strings.Contains(err.Error(), "remain") {
		t.Errorf("error should mention remaining references; got: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(out, "Frameworks", "MissingMedia.framework")); serr == nil {
		t.Error("refused tier-2 injection must not be applied")
	}

	// Confirm returning true → fixed, no error.
	out2 := filepath.Join(tmp, "TestApp-fixed2.app")
	confirmed := false
	err = CheckAndFix(appDir, out2, []string{fixDir}, FixOptions{Confirm: func(_, _ string) bool {
		confirmed = true
		return true
	}})
	if err != nil {
		t.Fatalf("confirmed fix should succeed: %v", err)
	}
	if !confirmed {
		t.Error("Confirm callback was never consulted")
	}
	if _, serr := os.Stat(filepath.Join(out2, "Frameworks", "MissingMedia.framework")); serr != nil {
		t.Errorf("confirmed tier-2 artifact missing: %v", serr)
	}

	// AutoYes behaves like always-yes.
	out3 := filepath.Join(tmp, "TestApp-fixed3.app")
	if err := CheckAndFix(appDir, out3, []string{fixDir}, FixOptions{AutoYes: true}); err != nil {
		t.Fatalf("AutoYes fix should succeed: %v", err)
	}
}

func TestCheckAndFixStillMissingExitsError(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := brokenApp(t, tmp)

	// No sources, no fetch → nothing can be found, and the unresolved
	// references must surface as an error (so `check --fix` exits non-zero).
	err := CheckAndFix(appDir, filepath.Join(tmp, "out.app"), nil, FixOptions{})
	if err == nil {
		t.Fatal("expected an error when nothing can be located")
	}
	if !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("error should mention unresolved references; got: %v", err)
	}
	// No artifact was found → no output should be written.
	if _, serr := os.Stat(filepath.Join(tmp, "out.app")); serr == nil {
		t.Error("no output should be written when nothing was fixed")
	}
}

func TestCheckAndFixFindsArtifactInsideDeb(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := brokenApp(t, tmp)

	// Ship the framework inside a .deb, and make sure the fix dir contains
	// ONLY the .deb (an on-disk payload tree would satisfy the search before
	// the deb-unpack path is reached, and this test is about that path).
	debRoot := filepath.Join(tmp, "debfix")
	payload := filepath.Join(debRoot, "payload")
	missingMediaFramework(t, tmp, payload)
	if err := os.MkdirAll(filepath.Join(payload, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	control := "Package: com.example.missingmedia\nVersion: 1.0\nArchitecture: iphoneos-arm\n"
	if err := os.WriteFile(filepath.Join(payload, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	debPath := filepath.Join(debRoot, "missingmedia.deb")
	if err := deb.Build(payload, debPath); err != nil {
		t.Fatal(err)
	}
	// Leave only the .deb in the fix dir.
	if err := os.RemoveAll(payload); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(tmp, "TestApp-fixed.app")
	if err := CheckAndFix(appDir, out, []string{debRoot}, FixOptions{AutoYes: true}); err != nil {
		t.Fatalf("CheckAndFix via deb should succeed: %v", err)
	}
	placed := filepath.Join(out, "Frameworks", "MissingMedia.framework", "MissingMedia")
	if _, err := os.Stat(placed); err != nil {
		t.Errorf("framework found inside the deb not placed at %s: %v", placed, err)
	}
}

func TestCheckAndFixIPARoundTrip(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := brokenApp(t, tmp)
	ipaIn := filepath.Join(tmp, "TestApp.ipa")
	testutil.MakeIPA(t, appDir, ipaIn)
	fixDir := filepath.Join(tmp, "fixes")
	missingMediaFramework(t, tmp, fixDir)

	ipaOut := filepath.Join(tmp, "TestApp-fixed.ipa")
	if err := CheckAndFix(ipaIn, ipaOut, []string{fixDir}, FixOptions{AutoYes: true}); err != nil {
		t.Fatalf("CheckAndFix ipa: %v", err)
	}
	// Unzip the fixed ipa and confirm the framework is inside.
	appDirOut, err := ipa.Extract(ipaOut, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	placed := filepath.Join(appDirOut, "Frameworks", "MissingMedia.framework", "MissingMedia")
	if _, err := os.Stat(placed); err != nil {
		t.Errorf("fixed ipa missing the resolved framework at %s: %v", placed, err)
	}
}
