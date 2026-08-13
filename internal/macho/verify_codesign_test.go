package macho

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blacktop/go-macho"
	ctypes "github.com/blacktop/go-macho/pkg/codesign/types"
)

// TestVerifyCodesignAcceptsNativeSign builds a real binary, signs it with
// entitlements through the native backend, and asks Apple's codesign to
// verify the result. This is the ground-truth check that the M3 writer +
// signer produce a structurally valid signature.
func TestVerifyCodesignAcceptsNativeSign(t *testing.T) {
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign not available")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(out string) {
		if b, err := exec.Command("go", "build", "-o", out, src).CombinedOutput(); err != nil {
			t.Fatalf("go build: %v: %s", err, b)
		}
	}
	app := filepath.Join(tmp, "app")
	build(app)

	entsXML := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>get-task-allow</key><true/>
<key>platform-application</key><true/>
</dict></plist>`
	eb := []byte(entsXML)

	// Reference: the same binary signed by Apple codesign, for CD comparison.
	apple := filepath.Join(tmp, "apple")
	build(apple)
	if out, err := exec.Command("codesign", "-s", "-", apple).CombinedOutput(); err != nil {
		t.Logf("apple ref sign failed: %v: %s", err, out)
	}

	b := Bin{Path: app}
	if err := b.SignWithEntitlements(eb); err != nil {
		t.Fatalf("SignWithEntitlements: %v", err)
	}

	// CD comparison via go-macho's own parser (correct for every CD version)
	// plus Apple's view of the same binaries.
	for _, p := range []struct{ name, path string }{{"native", app}, {"apple", apple}} {
		if d := cdOf(t, p.path); d != nil {
			h := d.Header
			t.Logf("CD %-8s ver=%#x flags=%#x nSpec=%d nCode=%d codeLimit=%d hashType=%d hashSize=%d pageBits=%d id=%q team=%q runtime=%d cdHash=%s",
				p.name, h.Version, h.Flags, h.NSpecialSlots, h.NCodeSlots, h.CodeLimit,
				h.HashType, h.HashSize, h.PageSize, d.ID, d.TeamID, h.Runtime, d.CDHash)
			for _, s := range d.SpecialSlots {
				t.Logf("CD %-8s   special slot %d hash=%x", p.name, s.Index, s.Hash)
			}
		}
	}

	out, err := exec.Command("codesign", "-dvvv", app).CombinedOutput()
	t.Logf("--- codesign -dvvv native (err=%v) ---\n%s", err, out)
	ver, err := exec.Command("codesign", "--verify", "--verbose=2", app).CombinedOutput()
	t.Logf("--- codesign --verify native: %v: %s", err, ver)

	verify := func(what string) {
		out, err := exec.Command("codesign", "--verify", "--verbose=2", app).CombinedOutput()
		if err != nil {
			t.Fatalf("codesign --verify after %s: %v: %s", what, err, out)
		}
	}
	verify("sign-with-entitlements")
	got, err := b.ExtractEntitlements()
	if err != nil {
		t.Fatalf("ExtractEntitlements: %v", err)
	}
	for _, want := range []string{"get-task-allow", "platform-application"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("entitlements missing %q after round-trip", want)
		}
	}
	if err := b.Fakesign(); err != nil {
		t.Fatalf("Fakesign: %v", err)
	}
	verify("fakesign")
	got2, err := b.ExtractEntitlements()
	if err != nil {
		t.Fatalf("ExtractEntitlements after fakesign: %v", err)
	}
	if !strings.Contains(string(got2), "get-task-allow") {
		t.Errorf("fakesign dropped existing entitlements: %q", got2)
	}
}

// cdOf returns the primary CodeDirectory parsed by go-macho, or nil when
// path has no readable signature.
func cdOf(t *testing.T, path string) *ctypes.CodeDirectory {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	cs := f.CodeSignature()
	if cs == nil || len(cs.CodeDirectories) == 0 {
		return nil
	}
	return &cs.CodeDirectories[0]
}
