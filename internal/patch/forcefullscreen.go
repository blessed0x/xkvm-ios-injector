package patch

import (
	"path/filepath"

	"github.com/blessed0x/xkvm-ios-injector/internal/log"
	"github.com/blessed0x/xkvm-ios-injector/internal/plist"
)

func init() {
	Register(&forceFullScreen{})
}

// forceFullScreen is the --force-fullscreen compatibility patch.
//
// It sets UIRequiresFullScreen = true in the app's Info.plist, which tells
// iOS the app must run full-screen and cannot participate in Split View /
// Slide Over. This is the standard fix for sideloaded apps whose layout
// breaks (or that refuse to launch) under multitasking — e.g. iPad-only
// titles sideloaded without their native multitasking support.
//
// It is deliberately plist-only (no Mach-O surgery), so it also serves as
// the reference template for writing a new compatibility patch. To add one:
//
//  1. Create internal/patch/<name>.go with an init() that calls
//     Register(&myPatch{}). Registration is the only wiring step — the CLI
//     binds a --<name> bool flag per registered patch automatically
//     (internal/cli/cli.go), so no flag plumbing is needed.
//  2. Implement the Patch interface (internal/patch/patch.go):
//     Name()  — the flag name and registry key (kebab-case, e.g. "foo-bar").
//     Apply(appDir) — mutate the extracted app bundle in place. appDir is
//     the *.app directory; plist.Open/plist.Write round-trip binary plists
//     while preserving every unknown key (plist.Dict is a map).
//  3. If the patch touches a Mach-O (like liquid-glass does), follow the
//     strip-edit-resign discipline: ExtractEntitlements → RemoveSignature →
//     edit → SignWithEntitlements — never leave the binary unsigned.
//  4. Add a unit test in patch_test.go mirroring TestForceFullScreen (open
//     the plist, assert the key, assert unrelated keys survived, guard with
//     testutil.SkipUnlessNativeToolchain like every other test here).
//  5. If two patches are mutually exclusive (like the liquid-glass pair),
//     enforce it in Options.validate() (internal/app/options.go).
type forceFullScreen struct{}

func (p *forceFullScreen) Name() string { return "force-fullscreen" }

func (p *forceFullScreen) Apply(appDir string) error {
	infoPath := filepath.Join(appDir, "Info.plist")
	info, err := plist.Open(infoPath)
	if err != nil {
		return err
	}
	info["UIRequiresFullScreen"] = true
	if err := plist.Write(infoPath, info); err != nil {
		return err
	}
	log.Infof("patch %s: set UIRequiresFullScreen=true", p.Name())
	return nil
}
