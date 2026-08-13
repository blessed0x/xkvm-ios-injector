package extras

// This file adds the bundled sideload-fix dylibs injected by xkvm --patch.
// They repair common App Store / keychain / entitlement issues that show up
// on sideloaded apps (they came from the user's AyuGram sideload kit, thinned
// to arm64 so the native signer never has to round-trip a fat binary).
//
// PROVENANCE: these are third-party jailbreak/sideload binaries of unknown
// license, embedded into the xkvm distributable. Verify redistribution
// rights before any public release — see the repo NOTICE; do not ship these
// in a published binary without clearing provenance.

import (
	"fmt"
	"os"
	"path/filepath"
)

// SideloadFixNames lists the bundled sideload-fix dylibs, in injection order.
var SideloadFixNames = []string{
	"sideloadFixerLol.dylib",
	"Sideloadbypass1.dylib",
	"Sideloadbypass2.dylib",
	"sideloadKeychainFix.dylib",
}

// MaterializeSideloadFixes writes every bundled sideload-fix dylib into dir
// and returns their paths, ready to feed the injector.
func MaterializeSideloadFixes(dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(SideloadFixNames))
	for _, name := range SideloadFixNames {
		data, err := fsys.ReadFile(filepath.Join("extras", "sideload", name))
		if err != nil {
			return nil, fmt.Errorf("embedded sideload fix %s: %w", name, err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o755); err != nil {
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
		paths = append(paths, p)
	}
	return paths, nil
}
