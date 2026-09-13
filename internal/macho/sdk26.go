package macho

import (
	"fmt"

	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
)

// sdk26 is the encoded iOS 26.0.0 SDK version (x.y.z packed as xxxx.yy.zz).
// This is the exact value Feather's Liquid Glass patch writes
// (MachOUtils.m, SDK_VERSION_26_0_0 0x1A0000) so iOS 26 renders the app with
// the new design instead of the legacy compatibility appearance.
const sdk26 = types.Version(0x1A0000)

// BumpSDK26 rewrites every LC_BUILD_VERSION load command's sdk field to
// 26.0.0 across every architecture slice (thin or fat). It is a fixed-size
// in-place edit — the field sits inside an existing load command, so the
// header+load-command region never grows and no data moves. Returns whether
// anything changed. The caller owns the signature discipline (remove before,
// re-sign after), matching the pattern used across this package.
func (b Bin) BumpSDK26() (bool, error) {
	changed := false
	err := editMachO(b.Path, func(f *macho.File, orig []byte) ([]byte, error) {
		for _, bv := range f.BuildVersions() {
			if bv.Sdk == sdk26 {
				continue
			}
			bv.Sdk = sdk26
			changed = true
		}
		return nativeWrite(f, orig, nil)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// SDKVersion returns the first LC_BUILD_VERSION sdk field of the first
// architecture slice, formatted "x.y" (e.g. "26.0"). It returns "" when the
// binary has no LC_BUILD_VERSION load command. This is the read-side
// counterpart of BumpSDK26, used by tests and the Liquid Glass patch to
// verify the value on disk.
func (b Bin) SDKVersion() (string, error) {
	f, err := readFirst(b.Path)
	if err != nil {
		return "", err
	}
	bvs := f.BuildVersions()
	if len(bvs) == 0 {
		return "", nil
	}
	v := bvs[0].Sdk
	return fmt.Sprintf("%d.%d", v/0x10000, (v/0x100)%0x100), nil
}
