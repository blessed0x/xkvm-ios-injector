package inject

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
	"github.com/blessed0x/xkvm-ios-injector/internal/testutil"
)

// TestInjectEndToEnd builds a real app bundle and a real Mach-O tweak, gives
// the tweak a legacy CydiaSubstrate dependency, injects it, and verifies the
// full chain: placement in Frameworks, LC_LOAD_DYLIB on the main binary,
// dependency rewriting to @rpath/CydiaSubstrate.framework, and the automatic
// framework injection.
func TestInjectEndToEnd(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	tweak := testutil.MakeTweak(t, tmp, "MyTweak")

	// Simulate a tweak built against the legacy substrate runtime: add a weak
	// LC_LOAD_DYLIB for MobileSubstrate, then let injection fix it.
	unsigned := macho.Bin{Path: tweak}
	if err := unsigned.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature on tweak: %v", err)
	}
	if err := unsigned.InjectWeak("/Library/MobileSubstrate/MobileSubstrate.dylib"); err != nil {
		t.Fatalf("InjectWeak fake dep: %v", err)
	}

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject"))
	if err := inj.Inject([]string{tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// 1. The tweak landed in Frameworks/.
	fwDir := filepath.Join(appDir, "Frameworks")
	installedTweak := filepath.Join(fwDir, "MyTweak.dylib")
	if _, err := os.Stat(installedTweak); err != nil {
		t.Fatalf("tweak not in Frameworks: %v", err)
	}

	// 2. The main binary now links @rpath/MyTweak.dylib.
	main := macho.Bin{Path: mainExe}
	deps, err := main.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(deps, "@rpath/MyTweak.dylib") {
		t.Errorf("main binary missing injected dep, got %v", deps)
	}

	// 3. The tweak's substrate dependency was rewritten to the canonical path.
	tweakDeps, err := macho.Bin{Path: installedTweak}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(tweakDeps, "@rpath/CydiaSubstrate.framework/CydiaSubstrate") {
		t.Errorf("substrate dep not fixed, got %v", tweakDeps)
	}

	// 4. CydiaSubstrate.framework was auto-injected.
	if _, err := os.Stat(filepath.Join(fwDir, "CydiaSubstrate.framework")); err != nil {
		t.Errorf("CydiaSubstrate.framework not auto-injected: %v", err)
	}
}

// TestInjectNoFrameworks verifies plain file/dir items copy to the app root
// without touching Frameworks.
func TestInjectNoFrameworks(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	item := filepath.Join(tmp, "item.txt")
	if err := os.WriteFile(item, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	inj := New(appDir, filepath.Join(appDir, "TestApp"), filepath.Join(tmp, "inject"))
	if err := inj.Inject([]string{item}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "item.txt")); err != nil {
		t.Errorf("item not copied to app root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "Frameworks")); !os.IsNotExist(err) {
		t.Errorf("Frameworks should not be created for a plain item")
	}
}

// TestInjectElleKitMode verifies the --ellekit runtime mode: a tweak linked
// against the legacy MobileSubstrate path gets its dependency rewritten to
// @rpath/ElleKit.framework/ElleKit, the real ElleKit.framework is auto-injected
// (thinned to the app's arm64), and no CydiaSubstrate.framework appears.
func TestInjectElleKitMode(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	tweak := testutil.MakeTweak(t, tmp, "LegacyTweak")
	unsigned := macho.Bin{Path: tweak}
	if err := unsigned.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature on tweak: %v", err)
	}
	if err := unsigned.InjectWeak("/Library/MobileSubstrate/MobileSubstrate.dylib"); err != nil {
		t.Fatalf("InjectWeak fake dep: %v", err)
	}

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject"))
	inj.SetMode(ModeElleKit)
	if err := inj.Inject([]string{tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	fwDir := filepath.Join(appDir, "Frameworks")
	installedTweak := filepath.Join(fwDir, "LegacyTweak.dylib")

	// The legacy MobileSubstrate dependency is rewritten to the ElleKit path.
	tweakDeps, err := macho.Bin{Path: installedTweak}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(tweakDeps, "@rpath/ElleKit.framework/ElleKit") {
		t.Errorf("substrate dep not rewritten to ElleKit, got %v", tweakDeps)
	}

	// ElleKit.framework is auto-injected and thinned to the app's arm64.
	ellekit := filepath.Join(fwDir, "ElleKit.framework", "ElleKit")
	if _, err := os.Stat(ellekit); err != nil {
		t.Errorf("ElleKit.framework not auto-injected: %v", err)
	}
	archs, err := macho.Bin{Path: ellekit}.Architectures()
	if err != nil {
		t.Fatal(err)
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("ElleKit not thinned to arm64, archs = %v", archs)
	}

	// The substrate-named framework must not appear in ellekit mode (D1).
	if _, err := os.Stat(filepath.Join(fwDir, "CydiaSubstrate.framework")); err == nil {
		t.Error("CydiaSubstrate.framework must not be injected in ellekit mode")
	}

	// The main binary still gets the weak load for the tweak itself.
	mainDeps, err := macho.Bin{Path: mainExe}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(mainDeps, "@rpath/LegacyTweak.dylib") {
		t.Errorf("main binary missing injected dep, got %v", mainDeps)
	}
}

// TestInjectLibhookerAutoSwitch verifies spec D6: a tweak that needs libhooker
// (the Dopamine/ElleKit-native runtime) auto-switches a substrate-mode run to
// the ElleKit runtime, without the user passing --ellekit.
func TestInjectLibhookerAutoSwitch(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	tweak := testutil.MakeTweak(t, tmp, "HookerTweak")
	unsigned := macho.Bin{Path: tweak}
	if err := unsigned.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature on tweak: %v", err)
	}
	if err := unsigned.InjectWeak("/usr/lib/libhooker.dylib"); err != nil {
		t.Fatalf("InjectWeak fake dep: %v", err)
	}

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject")) // default: ModeSubstrate
	if err := inj.Inject([]string{tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if inj.Mode != ModeElleKit {
		t.Errorf("mode = %d, want ModeElleKit after libhooker auto-switch", inj.Mode)
	}

	fwDir := filepath.Join(appDir, "Frameworks")
	tweakDeps, err := macho.Bin{Path: filepath.Join(fwDir, "HookerTweak.dylib")}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(tweakDeps, "@rpath/ElleKit.framework/ElleKit") {
		t.Errorf("libhooker dep not rewritten to ElleKit, got %v", tweakDeps)
	}
	if _, err := os.Stat(filepath.Join(fwDir, "ElleKit.framework", "ElleKit")); err != nil {
		t.Errorf("ElleKit.framework not auto-injected: %v", err)
	}
}

// TestInjectMixedSubstrateThenLibhookerOrderIndependent is a regression test
// for spec D6: the libhooker auto-switch must be decided by a PRE-SCAN of all
// staged tweaks, before any dependency is rewritten. A substrate-first mixed
// set must still produce a consistent ElleKit output — if the mode flips
// inline mid-loop, the substrate tweak keeps a CydiaSubstrate.framework dep
// while autoInject materializes ElleKit.framework for the same key, leaving a
// dangling reference.
func TestInjectMixedSubstrateThenLibhookerOrderIndependent(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	// Tweaks are processed in slice order, so list the substrate tweak FIRST
	// to expose any inline-flip ordering hazard.
	substrateTweak := testutil.MakeTweak(t, tmp, "OldSubstrateTweak")
	sub := macho.Bin{Path: substrateTweak}
	if err := sub.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}
	if err := sub.InjectWeak("/Library/MobileSubstrate/MobileSubstrate.dylib"); err != nil {
		t.Fatalf("InjectWeak substrate dep: %v", err)
	}

	hookerTweak := testutil.MakeTweak(t, tmp, "NewHookerTweak")
	hk := macho.Bin{Path: hookerTweak}
	if err := hk.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}
	if err := hk.InjectWeak("/usr/lib/libhooker.dylib"); err != nil {
		t.Fatalf("InjectWeak libhooker dep: %v", err)
	}

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject")) // default ModeSubstrate
	if err := inj.Inject([]string{substrateTweak, hookerTweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if inj.Mode != ModeElleKit {
		t.Errorf("mode = %d, want ModeElleKit (libhooker pre-scan)", inj.Mode)
	}

	fwDir := filepath.Join(appDir, "Frameworks")
	// BOTH tweaks must reference the ElleKit runtime — the substrate tweak
	// must not keep a CydiaSubstrate.framework dep that is never materialized.
	subDeps, err := macho.Bin{Path: filepath.Join(fwDir, "OldSubstrateTweak.dylib")}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(subDeps, "@rpath/ElleKit.framework/ElleKit") {
		t.Errorf("substrate tweak dep not rewritten to ElleKit runtime, got %v", subDeps)
	}
	if contains(subDeps, "CydiaSubstrate") {
		t.Errorf("substrate tweak keeps a dangling CydiaSubstrate dep, got %v", subDeps)
	}

	// Exactly one hooking runtime present: ElleKit, never CydiaSubstrate.
	if _, err := os.Stat(filepath.Join(fwDir, "ElleKit.framework", "ElleKit")); err != nil {
		t.Errorf("ElleKit.framework not auto-injected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fwDir, "CydiaSubstrate.framework")); err == nil {
		t.Error("CydiaSubstrate.framework must not appear in a libhooker-switched run")
	}
}

// TestInjectRootDylibUsesExecutablePath is the regression test for the
// Regram-style contract: a dylib marked as root-placed must land in the app
// root (NOT Frameworks/) and get an @executable_path/{name} load command on
// the main binary — never @rpath. dlopen-based tweaks (e.g. Regram) resolve
// their resources relative to @executable_path and crash if relocated to
// Frameworks with an @rpath load.
func TestInjectRootDylibUsesExecutablePath(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	tweak := testutil.MakeTweak(t, tmp, "RegramStyle")

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject"))
	inj.SetRootDylibs([]string{tweak})
	if err := inj.Inject([]string{tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// 1. The dylib landed in the APP ROOT, not Frameworks/.
	rootDylib := filepath.Join(appDir, "RegramStyle.dylib")
	if _, err := os.Stat(rootDylib); err != nil {
		t.Fatalf("root dylib not at app root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "Frameworks", "RegramStyle.dylib")); err == nil {
		t.Error("root dylib must NOT also be placed in Frameworks/")
	}

	// 2. The main binary links it via @executable_path — never @rpath.
	deps, err := macho.Bin{Path: mainExe}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(deps, "@executable_path/RegramStyle.dylib") {
		t.Errorf("main binary missing @executable_path dep, got %v", deps)
	}
	if contains(deps, "@rpath/RegramStyle.dylib") {
		t.Errorf("root dylib must NOT get an @rpath load command, got %v", deps)
	}
}

// TestInjectRootDylibNoFrameworksDir verifies that injecting ONLY root-placed
// dylibs does not create a Frameworks/ directory or add the
// @executable_path/Frameworks rpath — a root-only run should leave the bundle
// exactly as if nothing framework-y were injected.
func TestInjectRootDylibNoFrameworksDir(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	tweak := testutil.MakeTweak(t, tmp, "RootOnly")
	inj := New(appDir, mainExe, filepath.Join(tmp, "inject"))
	inj.SetRootDylibs([]string{tweak})
	if err := inj.Inject([]string{tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if _, err := os.Stat(filepath.Join(appDir, "Frameworks")); !os.IsNotExist(err) {
		t.Errorf("Frameworks/ must not exist for a root-only injection")
	}
}

// TestInjectRootDylibDependencyRewriting verifies that a Frameworks/ tweak
// depending on a root-placed dylib gets its reference rewritten to
// @executable_path (where the root dylib actually landed) rather than @rpath
// — the two contracts must be consistent so dyld can resolve the chain.
func TestInjectRootDylibDependencyRewriting(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	mainExe := filepath.Join(appDir, "TestApp")

	// Root-placed dylib (the "library").
	lib := testutil.MakeTweak(t, tmp, "RootLib")
	// Frameworks-placed tweak that depends on the root dylib. The dep path
	// must NOT contain a common-deps key (substrate/libhooker/...) — those
	// are rewritten by fixCommonDeps first, which would shadow this test's
	// fixInjectedDeps assertion.
	tweak := testutil.MakeTweak(t, tmp, "UsesRootLib")
	unsigned := macho.Bin{Path: tweak}
	if err := unsigned.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}
	if err := unsigned.InjectWeak("/usr/lib/RootLib.dylib"); err != nil {
		t.Fatalf("InjectWeak root-lib dep: %v", err)
	}

	inj := New(appDir, mainExe, filepath.Join(tmp, "inject"))
	inj.SetRootDylibs([]string{lib})
	if err := inj.Inject([]string{lib, tweak}); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	tweakDeps, err := macho.Bin{Path: filepath.Join(appDir, "Frameworks", "UsesRootLib.dylib")}.Dependencies()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(tweakDeps, "@executable_path/RootLib.dylib") {
		t.Errorf("dep on root dylib not rewritten to @executable_path, got %v", tweakDeps)
	}
	if contains(tweakDeps, "@rpath/RootLib.dylib") {
		t.Errorf("dep on root dylib must NOT be rewritten to @rpath, got %v", tweakDeps)
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
