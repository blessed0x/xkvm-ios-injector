// Package testutil builds real Mach-O fixtures for xkvm's end-to-end tests:
// the app main executable and a "tweak" dylib are produced by the actual Go
// toolchain on the host, then exercised through the native (pure-Go) macho
// operations.
//
// The tweak is built with a plain `go build` and given a .dylib extension:
// everything xkvm does to a dylib (otool -L parsing, install_name_tool
// -change, insert_dylib LC_LOAD_DYLIB injection, ldid signing) operates on
// Mach-O load commands and is identical for executables and dylibs. This
// avoids a cgo/clang dependency for -buildmode=c-shared while exercising the
// same code paths.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xkvm/xkvm/internal/ipa"
	"github.com/xkvm/xkvm/internal/plist"
)

// SkipUnlessNativeToolchain skips the test unless this host can produce real
// Mach-O fixtures. Only darwin/arm64 is exercised: `go build` there produces
// Mach-O binaries the native (pure-Go) backend can edit. On Linux `go build`
// produces ELF, not Mach-O, so the fixtures couldn't be built — do NOT try
// to widen this to run on Linux CI without a Mach-O fixture strategy.
func SkipUnlessNativeToolchain(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires the darwin/arm64 native toolchain")
	}
}

// GoBuild compiles src (a Go source file body) into dir/name using the real
// go toolchain and returns the binary path.
func GoBuild(t *testing.T, dir, name, src string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", out, srcPath)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, b)
	}
	return out
}

// MainSource is a minimal valid Go program for the app's main executable.
const MainSource = "package main\nfunc main() {}\n"

// MakeApp creates <tmp>/<appName>.app containing a real Go-built executable
// and a binary Info.plist, and returns the app directory.
func MakeApp(t *testing.T, tmp, appName, bundleID string) string {
	t.Helper()
	appDir := filepath.Join(tmp, appName+".app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := GoBuild(t, filepath.Join(tmp, "build"), appName, MainSource)
	if err := os.Rename(exe, filepath.Join(appDir, appName)); err != nil {
		t.Fatal(err)
	}
	info := plist.Dict{
		"CFBundleExecutable":         appName,
		"CFBundleIdentifier":         bundleID,
		"CFBundleName":               "TestApp",
		"CFBundleVersion":            "1.0.0",
		"CFBundleShortVersionString": "1.0.0",
		"MinimumOSVersion":           "12.0",
	}
	if err := plist.Write(filepath.Join(appDir, "Info.plist"), info); err != nil {
		t.Fatal(err)
	}
	return appDir
}

// MakeTweak builds a Mach-O binary named name.dylib in tmp and returns its
// path.
func MakeTweak(t *testing.T, tmp, name string) string {
	t.Helper()
	return GoBuild(t, filepath.Join(tmp, "tweakbuild"), name+".dylib", MainSource)
}

// MakeIPA zips an app directory into an .ipa at outPath with the correct
// Payload/ structure using the production ipa.Repack. The repack happens in a
// dedicated temp dir so the fixture IPA contains only Payload/ (sharing the
// app's temp dir would leak the go-build scratch dirs into the archive).
func MakeIPA(t *testing.T, appDir, outPath string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "payload")
	payload := filepath.Join(root, "Payload")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(payload, filepath.Base(appDir))
	if err := os.Rename(appDir, dest); err != nil {
		t.Fatal(err)
	}
	if err := ipa.Repack(root, outPath, 6); err != nil {
		t.Fatal(err)
	}
}
