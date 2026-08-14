package macho

// Regression test for the 32-bit slice serialization bug found converting
// Shadow_3.0-0.rc3.deb (a fat armv7+arm64+arm64e dylib): go-macho's
// FileHeader.Write always emits the 8-field struct (32 bytes), but a 32-bit
// Mach-O header is 7 fields (28 bytes, no Reserved). The 4-byte overrun
// shifted every load command, so a re-written armv7 slice failed to re-parse
// with "invalid command block size in record at byte 0x1c" (the stale
// Reserved field read as cmd=1, cmdsize=1). serializeTOC now truncates the
// Reserved field for Magic32 slices; these tests pin that path end to end.
//
// armv7 cannot link against the macOS SDK, so fixtures use
// `xcrun -sdk iphoneos clang` (the iPhoneOS SDK still ships armv7
// libSystem); the rewriting target is /usr/lib/libSystem.B.dylib, a real
// import that grows under the /var/jb prefix — the same growing-rename the
// rootless converter performs.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/blacktop/go-macho"
)

func skipNoToolchain(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires the darwin/arm64 native toolchain")
	}
	if _, err := exec.LookPath("xcrun"); err != nil {
		t.Skip("xcrun not available")
	}
	if _, err := exec.LookPath("lipo"); err != nil {
		t.Skip("lipo not available")
	}
}

// buildSlice builds one armv7 or arm64 dylib with the iPhoneOS SDK and
// returns its path.
func buildSlice(t *testing.T, dir, name, arch string) string {
	t.Helper()
	src := filepath.Join(dir, name+"-"+arch+".m")
	if err := os.WriteFile(src, []byte(
		"#include <dlfcn.h>\n"+
			"__attribute__((constructor)) static void init(void) {\n"+
			"    dlopen(\"/Library/MobileSubstrate/DynamicLibraries/"+name+".dylib\", RTLD_NOW);\n"+
			"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name+"-"+arch+".dylib")
	cmd := exec.Command("xcrun", "-sdk", "iphoneos", "clang", "-arch", arch,
		"-dynamiclib", "-miphoneos-version-min=9.0",
		"-install_name", "/Library/MobileSubstrate/DynamicLibraries/"+name+".dylib",
		"-o", out, src)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang(%s): %v: %s", arch, err, b)
	}
	return out
}

// depsOf parses path with go-macho and returns the (unfiltered) imports of
// every slice in order.
func depsOf(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	if isFatData(data) {
		ff, err := macho.NewFatFile(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("NewFatFile after rewrite: %v", err)
		}
		defer ff.Close()
		for _, arch := range ff.Arches {
			out = append(out, arch.File.ImportedLibraries())
		}
		return out
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewFile after rewrite: %v", err)
	}
	out = append(out, f.ImportedLibraries())
	return out
}

const (
	libSystemOld = "/usr/lib/libSystem.B.dylib"
	libSystemNew = "/var/jb/usr/lib/libSystem.B.dylib"
)

// TestFat32HeaderRoundTrip: a fat armv7+arm64 dylib survives a growing
// ChangeDependency + RemoveSignature and re-parses with every slice's load
// commands intact (the armv7 slice would previously be corrupted by the
// 32-byte header write).
func TestFat32HeaderRoundTrip(t *testing.T) {
	skipNoToolchain(t)
	tmp := t.TempDir()
	armv7 := buildSlice(t, tmp, "Fat32Tweak", "armv7")
	arm64 := buildSlice(t, tmp, "Fat32Tweak", "arm64")
	path := filepath.Join(tmp, "Fat32Tweak.dylib")
	if b, err := exec.Command("lipo", "-create", "-output", path, armv7, arm64).CombinedOutput(); err != nil {
		t.Fatalf("lipo: %v: %s", err, b)
	}

	b := Bin{Path: path}
	if err := b.ChangeDependency(libSystemOld, libSystemNew); err != nil {
		t.Fatalf("ChangeDependency: %v", err)
	}
	if err := b.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}

	all := depsOf(t, path)
	if len(all) != 2 {
		t.Fatalf("expected 2 slices, got %d", len(all))
	}
	for i, deps := range all {
		found := false
		for _, d := range deps {
			if d == libSystemNew {
				found = true
			}
			if d == libSystemOld {
				t.Errorf("slice %d still has the old dependency %s", i, libSystemOld)
			}
		}
		if !found {
			t.Errorf("slice %d missing converted dependency %s; got %v", i, libSystemNew, deps)
		}
	}

	// otool is the independent oracle: the armv7 slice must parse as a valid
	// 32-bit Mach-O after the rewrite.
	if b, err := exec.Command("otool", "-L", "-arch", "armv7", path).CombinedOutput(); err != nil {
		t.Fatalf("otool armv7 after rewrite: %v: %s", err, b)
	}
}

// TestThin32HeaderRoundTrip covers the same path for a thin armv7 dylib
// (buildFat is not involved; serializeTOC alone must emit a 28-byte header).
func TestThin32HeaderRoundTrip(t *testing.T) {
	skipNoToolchain(t)
	tmp := t.TempDir()
	path := buildSlice(t, tmp, "Thin32Tweak", "armv7")

	b := Bin{Path: path}
	if err := b.ChangeDependency(libSystemOld, libSystemNew); err != nil {
		t.Fatalf("ChangeDependency: %v", err)
	}

	all := depsOf(t, path)
	if len(all) != 1 {
		t.Fatalf("expected 1 slice, got %d", len(all))
	}
	found := false
	for _, d := range all[0] {
		if d == libSystemNew {
			found = true
		}
		if d == libSystemOld {
			t.Errorf("still has old dependency %s", libSystemOld)
		}
	}
	if !found {
		t.Errorf("converted dependency missing; got %v", all[0])
	}
	if b, err := exec.Command("otool", "-L", "-arch", "armv7", path).CombinedOutput(); err != nil {
		t.Fatalf("otool armv7 after rewrite: %v: %s", err, b)
	}
}
