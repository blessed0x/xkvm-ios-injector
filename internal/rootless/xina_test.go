package rootless

// Xina-style conversion (ConvertToXina) and its inverse (ConvertToRootful),
// pinned against the real committed Shadow fixture. The forward direction
// ports the Xinam1nePatcher script: install name + substrate dep become
// @rpath forms, the two /var/jb rpaths are added, and the NUL-anchored
// byte-seds rewrite string-table paths to the short symlink forms. The
// reverse direction hoists the payload and restores rootful paths; its
// lossy cases (@rpath/<basename> deps whose file isn't shipped in the
// package, e.g. system frameworks the forward renamed) are left as-is with a
// warning. Both directions are pure Go, so these run on every CI leg.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
	"github.com/xscope0/xkvm-ios-injector/internal/macho"
)

// countBytes returns how many times pat appears in the file at path.
func countBytes(t *testing.T, path, pat string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), pat)
}

// silentLogs silences the package logger for the duration of the test.
func silentLogs(t *testing.T) {
	t.Helper()
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
}

// TestXinaSymlinkTableConsistency pins the documented jailbreak-side
// symlink table against the byte-seds that target it: every short form the
// seds emit must resolve through a symlink in xinaSymlinks (dest <- target,
// with the byte-sed replacement as the dest). This makes the documentary
// table a checkable invariant — a sed that produces a path with no
// jailbreak-side symlink would dangle at runtime.
//
// The one known upstream inconsistency: the script's /bin/sh sed emits
// /var/sh, but the bootstrapper's symlink pairs only create /var/bash
// (/var/sh appears solely in the leftover-wipe list). The port preserves the
// script byte-for-byte, so /var/sh is tolerated here with that note.
func TestXinaSymlinkTableConsistency(t *testing.T) {
	for _, sed := range xinaByteSeds {
		short := strings.TrimPrefix(sed.to, "\x00")
		if short == "/var/sh" {
			continue // upstream quirk, documented above
		}
		// The short form is a first path component under /var; find the
		// symlink whose dest matches it (or a prefix of it).
		found := false
		for _, sl := range xinaSymlinks {
			if short == sl.dest || strings.HasPrefix(short, sl.dest+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("byte-sed target %q has no jailbreak-side symlink in xinaSymlinks", short)
		}
	}
	// And every revert exception's target must be an Apple /usr/lib path
	// (not a symlink destination) so the exceptions actually restore.
	for _, re := range xinaRevertExceptions {
		if !strings.HasPrefix(re.to, "/usr/lib/") {
			t.Errorf("revert exception restores to %q, want an Apple /usr/lib path", re.to)
		}
	}
}

// TestConvertToXinaGoldenShadow converts the real Shadow deb with the Xina
// pipeline and pins the output format: the var/jb layout, the @rpath
// install name, the @rpath/libsubstrate.dylib substrate shim (Cephei and
// /System deps untouched in load commands, exactly like the reference
// script), the two /var/jb rpaths, the NUL-anchored byte-seds on the raw
// string tables (6x /Library/MobileSub, 6x /Library/Frameworks, 18x
// /usr/lib/ in the committed fixture), and the control edits (iphoneos-arm64
// + " (Xinafied-rootless)" Name; no runtime dependency — the Xina script
// never adds one).
func TestConvertToXinaGoldenShadow(t *testing.T) {
	silentLogs(t)
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("golden fixture missing at %s: %v", fixture, err)
	}

	out := filepath.Join(tmp, "shadow-xina.deb")
	if err := ConvertToXina(fixture, out); err != nil {
		t.Fatalf("ConvertToXina: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// --- Layout: payload under var/jb, DEBIAN at the top ---
	dlDir := filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	for _, want := range []string{"Shadow.dylib", "Shadow.plist"} {
		if _, err := os.Stat(filepath.Join(dlDir, want)); err != nil {
			t.Errorf("payload missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(unpacked, "DEBIAN", "control")); err != nil {
		t.Errorf("DEBIAN/control missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "PreferenceBundles", "ShadowSettings.bundle")); err != nil {
		t.Errorf("PreferenceBundle missing: %v", err)
	}

	dylib := filepath.Join(dlDir, "Shadow.dylib")
	b := macho.Bin{Path: dylib}

	// --- Install name + deps: @rpath forms, Cephei and /System untouched ---
	id, err := b.InstallName()
	if err != nil {
		t.Fatal(err)
	}
	if id != "@rpath/Shadow.dylib" {
		t.Errorf("install name = %q, want @rpath/Shadow.dylib", id)
	}
	deps, err := b.AllDependencies()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(deps, "\n")
	// The NUL-anchored byte-seds fire on any string whose preceding byte is
	// a NUL — string tables AND load commands whose preceding field (e.g.
	// compat_version) is zero — exactly like the reference's `sed -i` on the
	// whole binary. So Cephei and librocketbootstrap (compat_version 0) are
	// converted to the short forms; the Apple libs are restored by the
	// revert-exception seds.
	for _, want := range []string{
		"@rpath/libsubstrate.dylib",                   // substrate shim (install_name_tool -change)
		"@rpath/Foundation",                           // /System dep renamed by the reference's install_name_tool loop
		"/var/LIY/Frameworks/Cephei.framework/Cephei", // byte-sed hit the load command (preceding field zero)
		"/var/lib/librocketbootstrap.dylib",           // byte-sed hit the load command
		"/usr/lib/libobjc.A.dylib",                    // Apple lib restored by the revert-exception seds
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deps missing %q; got:\n%s", want, got)
		}
	}
	for _, mustNot := range []string{
		"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
		"/System/Library/Frameworks/Foundation.framework/Foundation",
		"/Library/Frameworks/Cephei.framework/Cephei",
		"/usr/lib/librocketbootstrap.dylib",
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("dep %q must be rewritten; got:\n%s", mustNot, got)
		}
	}
	rpaths, err := b.Rpaths()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rpaths, " ") != "/var/jb/Library/Frameworks /var/jb/usr/lib" {
		t.Errorf("rpaths = %v, want [/var/jb/Library/Frameworks /var/jb/usr/lib]", rpaths)
	}

	// --- Byte-seds: the NUL-anchored rewrites (raw bytes) ---
	// The committed fixture carried "\x00/Library/MobileSub" 6x (the
	// LC_ID_DYLIB + self-load in each of 3 slices). After conversion the
	// install name became @rpath/Shadow.dylib (edit ran before the seds), so
	// only the self-load survives, converted: 3x /var/LIY. The /usr/lib
	// family: 18 string-table hits + 3x librocketbootstrap load commands = 21
	// converted to /var/lib; the 9 remaining /usr/lib are the 3 Apple libs
	// (libobjc/libc++/libSystem) x 3 slices, restored by the revert
	// exceptions.
	if gotN := countBytes(t, dylib, "\x00/var/LIY/MobileSub"); gotN != 3 {
		t.Errorf("string table has %d x /var/LIY/MobileSub, want 3", gotN)
	}
	if gotN := countBytes(t, dylib, "\x00/Library/MobileSub"); gotN != 0 {
		t.Errorf("string table still has %d x /Library/MobileSub, want 0", gotN)
	}
	if gotN := countBytes(t, dylib, "\x00/var/lib/"); gotN != 21 {
		t.Errorf("string table has %d x /var/lib/, want 21", gotN)
	}
	if gotN := countBytes(t, dylib, "\x00/usr/lib/"); gotN != 9 {
		t.Errorf("string table has %d x /usr/lib/, want 9 (the Apple libs, reverted)", gotN)
	}

	// --- Control edits: arm64 + Xinafied Name, NO runtime dependency ---
	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	for _, want := range []string{"Architecture: iphoneos-arm64", "(Xinafied-rootless)"} {
		if !strings.Contains(ctl, want) {
			t.Errorf("control missing %q; got:\n%s", want, ctl)
		}
	}
	if strings.Contains(ctl, RuntimeDep) {
		t.Errorf("control gained the runtime dependency (the Xina script never adds it); got:\n%s", ctl)
	}
}

// TestRootfulRoundTripXina converts Shadow rootful -> Xina -> rootful and
// checks the round trip lands back on rootful: payload hoisted, substrate
// shim restored to the CydiaSubstrate framework path, @rpath install name
// resolved to the file's package path, /var/jb rpaths gone, string tables
// restored, control reverted to iphoneos-arm without the Name suffix.
func TestRootfulRoundTripXina(t *testing.T) {
	silentLogs(t)
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}

	xina := filepath.Join(tmp, "shadow-xina.deb")
	if err := ConvertToXina(fixture, xina); err != nil {
		t.Fatalf("ConvertToXina: %v", err)
	}
	rootful := filepath.Join(tmp, "shadow-rootful.deb")
	if err := ConvertToRootful(xina, rootful); err != nil {
		t.Fatalf("ConvertToRootful: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(rootful, unpacked); err != nil {
		t.Fatal(err)
	}

	// --- Layout hoisted out of var/jb ---
	dylib := filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "Shadow.dylib")
	if _, err := os.Stat(dylib); err != nil {
		t.Fatalf("payload missing at package root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var")); err == nil {
		t.Error("empty var/ dir still present after hoist")
	}

	b := macho.Bin{Path: dylib}
	id, err := b.InstallName()
	if err != nil {
		t.Fatal(err)
	}
	if id != "/Library/MobileSubstrate/DynamicLibraries/Shadow.dylib" {
		t.Errorf("install name = %q, want the resolved package path", id)
	}
	deps, err := b.AllDependencies()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(deps, "\n")
	for _, want := range []string{
		"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate", // shim restored
		"/Library/Frameworks/Cephei.framework/Cephei",                 // /var/LIY load command restored
		"/usr/lib/librocketbootstrap.dylib",                           // /var/lib load command restored
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deps missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "@rpath/libsubstrate.dylib") {
		t.Errorf("@rpath/libsubstrate.dylib still present; got:\n%s", got)
	}
	// System-framework @rpath deps are unrecoverable (their file isn't in
	// the package) — they stay @rpath, the documented lossy case.
	if !strings.Contains(got, "@rpath/Foundation") {
		t.Errorf("unrecoverable @rpath dep missing (should be left as-is); got:\n%s", got)
	}
	rpaths, err := b.Rpaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, rp := range rpaths {
		if strings.HasPrefix(rp, "/var/jb/") {
			t.Errorf("/var/jb rpath %q not removed; rpaths = %v", rp, rpaths)
		}
	}

	// --- String tables fully restored: the 3 self-loads converted in the
	// forward AND the 3 install names (re-resolved from @rpath/Shadow.dylib
	// back to the package path) are all /Library/MobileSub again — the
	// original fixture's count of 6 — and none of the short forms remain ---
	if gotN := countBytes(t, dylib, "\x00/Library/MobileSub"); gotN != 6 {
		t.Errorf("string table has %d x /Library/MobileSub after round trip, want 6 (the original count)", gotN)
	}
	if gotN := countBytes(t, dylib, "\x00/var/LIY/MobileSub"); gotN != 0 {
		t.Errorf("string table still has %d x /var/LIY/MobileSub, want 0", gotN)
	}

	// --- Control reverted ---
	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	for _, want := range []string{"Architecture: iphoneos-arm", "Package: "} {
		if !strings.Contains(ctl, want) {
			t.Errorf("control missing %q; got:\n%s", want, ctl)
		}
	}
	if strings.Contains(ctl, "iphoneos-arm64") {
		t.Errorf("control still iphoneos-arm64; got:\n%s", ctl)
	}
	if strings.Contains(ctl, "Xinafied") {
		t.Errorf("control still carries the Xinafied Name suffix; got:\n%s", ctl)
	}
	if strings.Contains(ctl, RuntimeDep) {
		t.Errorf("control still carries the rootless runtime dependency; got:\n%s", ctl)
	}
}

// TestRootfulRoundTripStandard converts Shadow rootful -> rootless (the
// standard pipeline) -> rootful and checks the reverse restores the /var/jb
// load-command paths, install name, and control edits.
func TestRootfulRoundTripStandard(t *testing.T) {
	silentLogs(t)
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}

	rootless := filepath.Join(tmp, "shadow-rootless.deb")
	if err := Convert(fixture, rootless, false, false); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	rootful := filepath.Join(tmp, "shadow-rootful.deb")
	if err := ConvertToRootful(rootless, rootful); err != nil {
		t.Fatalf("ConvertToRootful: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(rootful, unpacked); err != nil {
		t.Fatal(err)
	}

	dylib := filepath.Join(unpacked, "Library", "MobileSubstrate", "DynamicLibraries", "Shadow.dylib")
	if _, err := os.Stat(dylib); err != nil {
		t.Fatalf("payload missing at package root: %v", err)
	}
	b := macho.Bin{Path: dylib}
	id, err := b.InstallName()
	if err != nil {
		t.Fatal(err)
	}
	if id != "/Library/MobileSubstrate/DynamicLibraries/Shadow.dylib" {
		t.Errorf("install name = %q, want the rootful package path", id)
	}
	deps, err := b.AllDependencies()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(deps, "\n")
	for _, want := range []string{
		"/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate",
		"/usr/lib/librocketbootstrap.dylib",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deps missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "/var/jb/") {
		t.Errorf("deps still carry /var/jb paths; got:\n%s", got)
	}
	rpaths, err := b.Rpaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(rpaths) != 0 {
		t.Errorf("rpaths = %v, want none after rootful round trip", rpaths)
	}

	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	if strings.Contains(ctl, "iphoneos-arm64") {
		t.Errorf("control still iphoneos-arm64; got:\n%s", ctl)
	}
	if strings.Contains(ctl, RuntimeDep) {
		t.Errorf("control still carries the rootless runtime dependency; got:\n%s", ctl)
	}
}

// TestConvertToRootfulAlreadyRootful: a deb whose payload is already rootful
// (no var/jb) is skipped cleanly, mirroring the forward converters' skip.
func TestConvertToRootfulAlreadyRootful(t *testing.T) {
	silentLogs(t)
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out.deb")
	if err := ConvertToRootful(fixture, out); err != nil {
		t.Fatalf("ConvertToRootful: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("skip case must not write an output file")
	}
}

// TestXinaStringTableSedsRuntimeProof is the native-only counterpart to the
// golden byte-sed counts: a clang-built dylib whose dlopen path is compiled
// into __TEXT.__cstring runs through Xina conversion and back. The raw bytes
// prove the NUL-anchored seds hit the runtime string (not just the fixture's
// incidental string tables), and the rootful round trip restores it.
func TestXinaStringTableSedsRuntimeProof(t *testing.T) {
	if _, err := os.Stat("/usr/bin/clang"); err != nil {
		t.Skip("clang not available")
	}
	silentLogs(t)
	tmp := t.TempDir()

	probe := clangDylib(t, filepath.Join(tmp, "build"), "xprobe", dlopenProbeC)

	staging := filepath.Join(tmp, "staging")
	if err := os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(controlFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	payloadDir := filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(t, probe, filepath.Join(payloadDir, "XkvmRuntimeProbe.dylib")); err != nil {
		t.Fatal(err)
	}
	rootful := filepath.Join(tmp, "rootful.deb")
	if err := deb.Build(staging, rootful); err != nil {
		t.Fatal(err)
	}

	xina := filepath.Join(tmp, "xina.deb")
	if err := ConvertToXina(rootful, xina); err != nil {
		t.Fatalf("ConvertToXina: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(xina, unpacked); err != nil {
		t.Fatal(err)
	}
	converted := filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "XkvmRuntimeProbe.dylib")
	if _, err := os.Stat(converted); err != nil {
		t.Fatal(err)
	}
	// The compiled-in dlopen string moved to the short form (and the
	// original is gone).
	if got := countBytes(t, converted, "\x00/var/LIY/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib"); got != 1 {
		t.Errorf("runtime dlopen string not rewritten to /var/LIY (found %d), want 1", got)
	}
	if got := countBytes(t, converted, "\x00/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib"); got != 0 {
		t.Errorf("runtime dlopen string still rootful (found %d), want 0", got)
	}

	// Rootful round trip restores the compiled-in string.
	back := filepath.Join(tmp, "back.deb")
	if err := ConvertToRootful(xina, back); err != nil {
		t.Fatalf("ConvertToRootful: %v", err)
	}
	unpacked2 := filepath.Join(tmp, "unpacked2")
	if err := deb.Unpack(back, unpacked2); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(unpacked2, "Library", "MobileSubstrate", "DynamicLibraries", "XkvmRuntimeProbe.dylib")
	if got := countBytes(t, restored, "\x00/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib"); got != 1 {
		t.Errorf("runtime dlopen string not restored to rootful (found %d), want 1", got)
	}
	if got := countBytes(t, restored, "\x00/var/LIY/MobileSubstrate"); got != 0 {
		t.Errorf("runtime dlopen string still /var/LIY after round trip (found %d), want 0", got)
	}
}
