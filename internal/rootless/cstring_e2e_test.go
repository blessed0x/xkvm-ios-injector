package rootless

// This file proves the __cstring boundary is closed at RUNTIME, not just
// statically: a clang-built dylib whose dlopen path is compiled into
// __TEXT.__cstring is run through the full Convert pipeline, then loaded,
// and the dlerror string proves dlopen actually used the /var/jb path.
// The dlopen path grows under conversion (/Library -> /var/jb/Library), so
// the relocation path (__PATCH_ROOTLESS segment insertion + ADRP/ADD
// reference retargeting) is exercised end to end.
//
// Requires the darwin/arm64 native toolchain (clang + python3 + codesign);
// skipped elsewhere. No cgo: Go 1.26 removed cgo support in _test.go files,
// and loading through a python3/ctypes subprocess exercises the same real
// dyld dlopen path without a test-side cgo dependency. The test host signs
// fixtures ad-hoc because Apple Silicon refuses to load unsigned arm64 code.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/testutil"
)

const dlopenProbeC = `
#include <dlfcn.h>

// dlopen a rootful path at runtime and report what dlopen actually tried.
const char *xkvm_probe(void) {
    void *h = dlopen("/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib", RTLD_NOW);
    if (h != 0) {
        dlclose(h);
        return "OK";
    }
    const char *e = dlerror();
    return e != 0 ? e : "unknown error";
}
`

// pythonLoader loads a dylib via ctypes and calls xkvm_probe, printing what
// the probe reported dlopen tried (the dlerror text, or "OK").
const pythonLoader = `
import ctypes, sys
lib = ctypes.CDLL(sys.argv[1])
lib.xkvm_probe.restype = ctypes.c_char_p
print(lib.xkvm_probe().decode("utf-8", "replace"))
`

// clangDylib compiles src into dir/name.dylib (arm64, @rpath install name so
// the fixture's own install name is never a conversion target) and returns
// the path. The output is ad-hoc signed so the host can load it.
func clangDylib(t *testing.T, dir, name, src string) string {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not available")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, name+".c")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name+".dylib")
	cmd := exec.Command("clang", "-arch", "arm64", "-dynamiclib",
		"-install_name", "@rpath/"+name+".dylib", "-o", out, srcPath)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clang: %v: %s", err, b)
	}
	cmd = exec.Command("codesign", "-s", "-", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v: %s", err, b)
	}
	return out
}

// probe loads the dylib at path in a python3 subprocess, calls xkvm_probe,
// and returns what the probe reported dlopen tried.
func probe(t *testing.T, path string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	b, err := exec.Command("python3", "-c", pythonLoader, path).CombinedOutput()
	if err != nil {
		t.Fatalf("probe %s: %v: %s", path, err, b)
	}
	return strings.TrimSpace(string(b))
}

// TestCStringDlopenRuntimeProof: the dlopen path compiled into __cstring is
// rewritten by Convert, and the converted dylib actually dlopens the /var/jb
// path at runtime.
func TestCStringDlopenRuntimeProof(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	probeDylib := clangDylib(t, filepath.Join(tmp, "build"), "dlprobe", dlopenProbeC)

	// Pre-conversion baseline: the probe's dlerror names the ROOTFUL path.
	before := probe(t, probeDylib)
	if !strings.Contains(before, "/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib") {
		t.Fatalf("pre-conversion probe reported %q; expected the rootful path", before)
	}

	// Wrap into a rootful deb and convert.
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
	probeName := filepath.Join(payloadDir, "XkvmRuntimeProbe.dylib")
	if err := copyFile(t, probeDylib, probeName); err != nil {
		t.Fatal(err)
	}
	rootful := filepath.Join(tmp, "rootful.deb")
	if err := deb.Build(staging, rootful); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := filepath.Join(tmp, "rootless.deb")
	if err := Convert(rootful, out, false, false); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	converted := filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "XkvmRuntimeProbe.dylib")
	if _, err := os.Stat(converted); err != nil {
		t.Fatalf("converted dylib missing at %s: %v", converted, err)
	}

	// The conversion re-signs ad-hoc (like Derootifier's ldid step), so the
	// rewritten binary is already host-loadable — no post-conversion sign.

	// The money assertion: at runtime, dlopen now tries the /var/jb path.
	after := probe(t, converted)
	if !strings.Contains(after, "/var/jb/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib") {
		t.Fatalf("post-conversion probe reported %q; expected the /var/jb path", after)
	}
	if strings.Contains(after, "/Library/MobileSubstrate/DynamicLibraries/XkvmRuntimeProbe.dylib") && !strings.Contains(after, "/var/jb/Library") {
		t.Fatalf("post-conversion probe still names the rootful path: %q", after)
	}
}

// controlFixture is a minimal rootful deb control file (also used by the
// package's other fixture tests).
const controlFixture = "Package: dlprobe\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: dlopen probe\nMaintainer: xkvm\n"
