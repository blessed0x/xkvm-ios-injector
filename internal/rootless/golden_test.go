package rootless

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gmmacho "github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
)

// goldenShadowDeb is the committed real-world fixture: Shadow_3.0-0.rc3.deb
// (jjolano's jailbreak-detection bypass, fat armv7+arm64+arm64e, with a
// PreferenceBundle), the same deb used to shake out the 32-bit header bug
// and the LC_RPATH serializer drift. Committing it pins those regressions:
// the conversion is pure Go, so this golden test runs on every CI leg.
const goldenShadowDeb = "../../testdata/fixtures/debs/Shadow_3.0-0.rc3.deb"

// TestTweakInjectGoldenShadow converts the real Shadow deb with
// --tweakinject and pins the complete output format: the TweakInject
// layout, the @rpath/libsubstrate.dylib shim, the @rpath install name, the
// /usr/lib + /var/jb/usr/lib rpaths, the untouched PreferenceBundle, the
// control edits, and — the regression pin — the exact armv7 slice load
// command layout (ncmds, sizeofcmds, and every command's type and size,
// ending flush at the sizeofcmds boundary). The armv7 slice is the one that
// broke before the serializeTOC LC_RPATH fix: its preceding commands don't
// total a multiple of 8, which used to push the second rpath 4 bytes late
// and desync every command after it.
func TestTweakInjectGoldenShadow(t *testing.T) {
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()

	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("golden fixture missing at %s (committed under testdata/fixtures/debs/): %v", fixture, err)
	}

	out := filepath.Join(tmp, "shadow-tweakinject.deb")
	if err := Convert(fixture, out, false, true); err != nil {
		t.Fatalf("Convert(tweakinject): %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// --- Layout: DynamicLibraries moved under usr/lib/TweakInject ---
	tiDir := filepath.Join(unpacked, "var", "jb", "usr", "lib", "TweakInject")
	for _, want := range []string{"Shadow.dylib", "Shadow.plist"} {
		if _, err := os.Stat(filepath.Join(tiDir, want)); err != nil {
			t.Errorf("TweakInject payload missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")); err == nil {
		t.Error("legacy DynamicLibraries dir still present")
	}
	// PreferenceBundle (not an injectable) stays in place.
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "PreferenceBundles", "ShadowSettings.bundle")); err != nil {
		t.Errorf("PreferenceBundle missing: %v", err)
	}

	dylib := filepath.Join(tiDir, "Shadow.dylib")
	b := macho.Bin{Path: dylib}

	// --- Load commands: substrate shim + @rpath install name + rpaths ---
	deps, err := b.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies: %v", err)
	}
	got := strings.Join(deps, "\n")
	if !strings.Contains(got, "@rpath/libsubstrate.dylib") {
		t.Errorf("CydiaSubstrate dep not shimmed to @rpath/libsubstrate.dylib; got:\n%s", got)
	}
	for _, mustNot := range []string{
		"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
		"/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("dep %q must be rewritten to the substrate shim; got:\n%s", mustNot, got)
		}
	}
	id, err := b.InstallName()
	if err != nil {
		t.Fatal(err)
	}
	if id != "@rpath/Shadow.dylib" {
		t.Errorf("install name = %q, want @rpath/Shadow.dylib", id)
	}
	rpaths, err := b.Rpaths()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rpaths, " ") != "/usr/lib /var/jb/usr/lib" {
		t.Errorf("rpaths = %v, want [/usr/lib /var/jb/usr/lib]", rpaths)
	}

	// --- Regression pin: exact armv7 slice load command layout ---
	// The table below is the raw on-disk layout (cmd value + cmdsize) that
	// otool reads from the converted binary — NOT go-macho's LoadSize()
	// recomputation, which can differ from disk on real 32-bit binaries
	// whose original cmdsize values were only 4-aligned. Values captured
	// from the conversion of the committed fixture on 2026-08-14 (the
	// trailing LC_CODE_SIGNATURE appeared when the converter started
	// re-signing like Derootifier's ldid step); if this table needs
	// updating, diff against `otool -arch armv7 -l` on the converted dylib
	// and update BOTH the table and this comment.
	wantCmds := []struct {
		cmd  uint32
		size uint32
	}{
		{0x1, 532},       // LC_SEGMENT
		{0x1, 1076},      // LC_SEGMENT
		{0x1, 56},        // LC_SEGMENT
		{0xd, 48},        // LC_ID_DYLIB
		{0x80000022, 48}, // LC_DYLD_INFO_ONLY
		{0x2, 24},        // LC_SYMTAB
		{0xb, 80},        // LC_DYSYMTAB
		{0x1b, 24},       // LC_UUID
		{0x25, 16},       // LC_VERSION_MIN_IPHONEOS
		{0x2a, 16},       // LC_SOURCE_VERSION
		{0x21, 20},       // LC_ENCRYPTION_INFO
		{0xc, 52},        // LC_LOAD_DYLIB
		{0xc, 84},        // LC_LOAD_DYLIB
		{0xc, 92},        // LC_LOAD_DYLIB
		{0xc, 72},        // LC_LOAD_DYLIB
		{0xc, 92},        // LC_LOAD_DYLIB
		{0xc, 80},        // LC_LOAD_DYLIB
		{0xc, 56},        // LC_LOAD_DYLIB
		{0xc, 48},        // LC_LOAD_DYLIB
		{0xc, 52},        // LC_LOAD_DYLIB
		{0x26, 16},       // LC_FUNCTION_STARTS
		{0x29, 16},       // LC_DATA_IN_CODE
		{0x8000001c, 24}, // LC_RPATH (/usr/lib)
		{0x8000001c, 32}, // LC_RPATH (/var/jb/usr/lib)
		{0x1d, 16},       // LC_CODE_SIGNATURE (re-signed like Derootifier's ldid step)
	}
	gotLayout := armv7CommandLayout(t, dylib)
	if gotLayout == nil {
		t.Fatal("no armv7 slice found in converted Shadow.dylib")
	}
	if len(gotLayout) != len(wantCmds) {
		t.Fatalf("armv7 ncmds = %d, want %d\nlayout:\n%s", len(gotLayout), len(wantCmds), formatLayout(gotLayout))
	}
	for i, want := range wantCmds {
		if gotLayout[i].cmd != want.cmd || gotLayout[i].size != want.size {
			t.Errorf("armv7 cmd[%d] = %#x size=%d, want %#x size=%d", i, gotLayout[i].cmd, gotLayout[i].size, want.cmd, want.size)
		}
	}

	// --- Control edits ---
	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	for _, want := range []string{"Architecture: iphoneos-arm64", RuntimeDep} {
		if !strings.Contains(ctl, want) {
			t.Errorf("control missing %q; got:\n%s", want, ctl)
		}
	}
}

// armv7CommandLayout re-parses the converted fat dylib and returns the
// armv7 slice's load commands as (raw cmd, raw cmdsize) pairs read from the
// slice bytes — the same values otool prints — or nil if there is no armv7
// slice. Re-parsing with go-macho first is the whole point: the layout is
// only reachable if go-macho can walk it end-to-end, which is exactly what
// failed before the LC_RPATH serializer fix ("invalid command block size in
// record at byte 0xa28"). The raw walk then pins the on-disk layout.
func armv7CommandLayout(t *testing.T, path string) []struct {
	cmd  uint32
	size uint32
} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := gmmacho.OpenFat(path)
	if err != nil {
		t.Fatalf("OpenFat (the layout must re-parse): %v", err)
	}
	defer f.Close()
	for _, arch := range f.Arches {
		if arch.CPU != types.CPUArm {
			continue
		}
		slice := data[arch.Offset : arch.Offset+arch.Size]
		if _, err := gmmacho.NewFile(bytes.NewReader(slice)); err != nil {
			t.Fatalf("NewFile(armv7 slice): %v", err)
		}
		bo := binary.LittleEndian
		ncmds := int(bo.Uint32(slice[16:20]))
		sizeofcmds := bo.Uint32(slice[20:24])
		out := make([]struct {
			cmd  uint32
			size uint32
		}, 0, ncmds)
		p := 28 // 32-bit mach header
		for i := 0; i < ncmds; i++ {
			if p+8 > len(slice) {
				t.Fatalf("armv7 command walk ran off the slice at byte %#x", p)
			}
			cmd := bo.Uint32(slice[p : p+4])
			sz := bo.Uint32(slice[p+4 : p+8])
			if sz < 8 || p+int(sz) > len(slice) {
				t.Fatalf("armv7 cmd[%d] has invalid size %d at byte %#x — layout is desynced", i, sz, p)
			}
			out = append(out, struct {
				cmd  uint32
				size uint32
			}{cmd, sz})
			p += int(sz)
		}
		if p != 28+int(sizeofcmds) {
			t.Fatalf("armv7 command walk ends at %#x but sizeofcmds says %#x — layout is not flush", p, 28+int(sizeofcmds))
		}
		return out
	}
	return nil
}

func formatLayout(layout []struct {
	cmd  uint32
	size uint32
}) string {
	var sb strings.Builder
	for i, l := range layout {
		sb.WriteString("cmd=" + hex(l.cmd) + " size=" + itoa(l.size))
		if i < len(layout)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func hex(v uint32) string {
	const digits = "0123456789abcdef"
	var b [10]byte
	b[0], b[1] = '0', 'x'
	for i := 9; i >= 2; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
