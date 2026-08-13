package macho

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/blacktop/go-macho/types"
	"github.com/xkvm/xkvm/internal/testutil"
)

// readSdk returns the first LC_BUILD_VERSION sdk field of a Mach-O slice (0 if
// the binary has no such load command).
func readSdk(t *testing.T, path string) types.Version {
	t.Helper()
	f, err := readFirst(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	bvs := f.BuildVersions()
	if len(bvs) == 0 {
		return 0
	}
	return bvs[0].Sdk
}

func TestBumpSDK26(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	b := newTestBin(t)
	if readSdk(t, b.Path) == 0 {
		t.Skip("fixture binary has no LC_BUILD_VERSION")
	}

	changed, err := b.BumpSDK26()
	if err != nil {
		t.Fatalf("BumpSDK26: %v", err)
	}
	if !changed {
		t.Error("expected BumpSDK26 to report a change")
	}
	if got := readSdk(t, b.Path); got != 0x1A0000 {
		t.Errorf("sdk = %#x, want 0x1A0000", got)
	}

	// Idempotent: the second call must report no change.
	changed2, err := b.BumpSDK26()
	if err != nil {
		t.Fatalf("BumpSDK26 second call: %v", err)
	}
	if changed2 {
		t.Error("second BumpSDK26 should report no change")
	}

	// The patched binary must still be structurally valid and executable.
	if out, err := exec.Command(b.Path).CombinedOutput(); err != nil {
		t.Errorf("patched binary failed to run: %v: %s", err, out)
	}
}

// TestBumpSDK26Fat verifies every slice of a universal binary gets the bump.
func TestBumpSDK26Fat(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	fat := buildFatFixture(t, "arm64", "x86_64")

	b := Bin{Path: fat}
	changed, err := b.BumpSDK26()
	if err != nil {
		t.Fatalf("BumpSDK26 fat: %v", err)
	}
	if !changed {
		t.Error("expected BumpSDK26 on fat to report a change")
	}
	for _, arch := range []string{"arm64", "x86_64"} {
		slice := filepath.Join(t.TempDir(), "slice-"+arch)
		if err := exec.Command("lipo", "-thin", arch, fat, "-output", slice).Run(); err != nil {
			t.Fatalf("lipo -thin %s: %v", arch, err)
		}
		if got := readSdk(t, slice); got != 0x1A0000 {
			t.Errorf("%s slice sdk = %#x, want 0x1A0000", arch, got)
		}
	}
}

func TestThinToArch(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)

	// Thin arm64 binary: no-op for arm64, error for another arch.
	b := newTestBin(t)
	if err := b.ThinToArch("arm64"); err != nil {
		t.Fatalf("ThinToArch(arm64) on thin arm64: %v", err)
	}
	if err := b.ThinToArch("x86_64"); err == nil {
		t.Error("ThinToArch(x86_64) on thin arm64 should error")
	}

	// Fat binary: thins to the requested slice.
	fat := buildFatFixture(t, "arm64", "x86_64")
	fb := Bin{Path: fat}
	if err := fb.ThinToArch("arm64"); err != nil {
		t.Fatalf("ThinToArch(arm64) on fat: %v", err)
	}
	archs, err := fb.Architectures()
	if err != nil {
		t.Fatal(err)
	}
	if len(archs) != 1 || archs[0] != "arm64" {
		t.Errorf("after thin archs = %v, want [arm64]", archs)
	}

	// Fat binary lacking the requested arch errors.
	fat2 := buildFatFixture(t, "arm64", "x86_64")
	if err := (Bin{Path: fat2}).ThinToArch("arm64e"); err == nil {
		t.Error("ThinToArch(arm64e) on a non-arm64e fat binary should error")
	}
}

// buildFatFixture compiles one thin Mach-O per requested architecture with
// xcrun clang (the host Go toolchain can no longer cross-compile darwin/x86_64
// on Go >= 1.26) and lipo-creates a universal binary from them. Go-built
// binaries carry no LC_BUILD_VERSION guarantee across toolchains, so clang is
// used for every slice to keep the fixture uniform.
func buildFatFixture(t *testing.T, archs ...string) string {
	t.Helper()
	tmp := t.TempDir()
	var inputs []string
	for i, arch := range archs {
		src := filepath.Join(tmp, fmt.Sprintf("a%d.c", i))
		if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(tmp, "bin-"+arch)
		cmd := exec.Command("xcrun", "clang", "-arch", arch, "-o", out, src)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("clang %s: %v: %s", arch, err, b)
		}
		inputs = append(inputs, out)
	}
	fat := filepath.Join(tmp, "fat")
	args := append([]string{"-create"}, inputs...)
	args = append(args, "-output", fat)
	if err := exec.Command("lipo", args...).Run(); err != nil {
		t.Fatalf("lipo -create: %v", err)
	}
	return fat
}
