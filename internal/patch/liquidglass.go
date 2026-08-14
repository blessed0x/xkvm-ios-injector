package patch

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xscope0/xkvm-ios-injector/internal/log"
	"github.com/xscope0/xkvm-ios-injector/internal/macho"
	"github.com/xscope0/xkvm-ios-injector/internal/plist"
)

func init() {
	Register(&liquidGlass{})
	Register(&liquidGlass{compat: true})
}

// liquidGlass is the iOS 26 Liquid Glass patch, ported from Feather's
// experiment_supportLiquidGlass / experiment_disableLiquidGlass toggles
// (Feather/Utilities/Handlers/SigningHandler.swift:_modifyDict).
//
// Enable (--liquid-glass) does two things so iOS 26 renders the app with the
// new Liquid Glass design:
//  1. Info.plist: UIDesignRequiresCompatibility = false (the system opt-out
//     key for the legacy appearance), and
//  2. the main executable's LC_BUILD_VERSION.sdk is bumped to 26.0.0
//     (0x1A0000) on every slice — apps recorded with SDK >= 26 get the new
//     design automatically.
//
// The compat variant (--liquid-glass-compat) only sets
// UIDesignRequiresCompatibility = true to force the legacy appearance; no
// Mach-O change. This mirrors Feather exactly (only the enable path calls
// _locateMachosAndChangeToSDK26).
//
// Attribution: the SDK-26 Mach-O edit follows Feather's MachOUtils.m, itself
// derived from a LiveContainer commit (Apache-2.0).
type liquidGlass struct {
	// compat=true selects the --liquid-glass-compat variant (force legacy).
	compat bool
}

func (p *liquidGlass) Name() string {
	if p.compat {
		return "liquid-glass-compat"
	}
	return "liquid-glass"
}

func (p *liquidGlass) Apply(appDir string) error {
	infoPath := filepath.Join(appDir, "Info.plist")
	info, err := plist.Open(infoPath)
	if err != nil {
		return err
	}
	info["UIDesignRequiresCompatibility"] = p.compat
	if err := plist.Write(infoPath, info); err != nil {
		return err
	}
	log.Infof("patch %s: set UIDesignRequiresCompatibility=%v", p.Name(), p.compat)

	if p.compat {
		return nil // disable path: plist key only (Feather parity)
	}

	exe, _ := info["CFBundleExecutable"].(string)
	if exe == "" {
		return fmt.Errorf("info.plist has no CFBundleExecutable")
	}
	main := macho.Bin{Path: filepath.Join(appDir, exe)}
	if _, err := os.Stat(main.Path); err != nil {
		return err
	}
	return bumpSDK26(main)
}

// bumpSDK26 applies the Mach-O SDK-26 edit with the same signature discipline
// as injection: preserve entitlements, strip, edit, re-sign. The binary is
// ALWAYS re-signed after the edit (ad-hoc, with the preserved entitlements
// even when there are none): the patch stripped the original signature, so
// leaving it unsigned would make the patched app uninstallable unless the
// user also passed --fakesign.
func bumpSDK26(main macho.Bin) error {
	ents, err := main.ExtractEntitlements()
	if err != nil {
		log.Warnf("liquid-glass: couldn't read entitlements: %v", err)
		ents = nil
	}
	if err := main.RemoveSignature(); err != nil {
		return err
	}
	changed, err := main.BumpSDK26()
	if err != nil {
		return err
	}
	if !changed {
		log.Infof("liquid-glass: main executable SDK is already 26.0.0 (or has no LC_BUILD_VERSION)")
	}
	return main.SignWithEntitlements(ents)
}
