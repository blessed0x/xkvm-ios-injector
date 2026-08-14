package rootless

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	howett "howett.net/plist"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/plist"
)

// buildPlistFixture builds a rootless deb (payload under var/jb) whose
// payload is plists + a control script only — no Mach-O, so this test needs
// no native toolchain and runs on every CI leg (ubuntu included):
//
//   - Library/LaunchDaemons/com.xkvm.daemon.plist — a BINARY plist whose
//     ProgramArguments points at /var/jb/usr/libexec/xkvm-daemon
//   - Library/libSandy/Sandy_foo/Info.plist — a BINARY plist carrying a
//     /Library/... path (rootfs-bound), a /var/jb/... path (jbroot prefix
//     stripped to the root by the dance) and a bare "/" (the >/< rootfs case)
//   - Library/Preferences/com.xkvm.plist — a binary plist with no jb paths
//   - DEBIAN/preinst — a control script with rootful paths
func buildPlistFixture(t *testing.T, tmp string) string {
	t.Helper()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: plisttweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: plist test\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	preinst := "#!/bin/sh\nmkdir -p /Library/MobileSubstrate/DynamicLibraries\ndpkg -i /var/jb/usr/lib/xkvm.deb\ncd /var/jb && true\n[ \"$(uname)\" = \"iphoneos-arm64\" ] && true\nexit 0\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "preinst"), []byte(preinst), 0o755); err != nil {
		t.Fatal(err)
	}

	bin := func(d map[string]any) []byte {
		data, err := howett.Marshal(d, howett.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	daemon := filepath.Join(staging, "var", "jb", "Library", "LaunchDaemons")
	os.MkdirAll(daemon, 0o755)
	os.WriteFile(filepath.Join(daemon, "com.xkvm.daemon.plist"), bin(map[string]any{
		"Label":            "com.xkvm.daemon",
		"ProgramArguments": []any{"/var/jb/usr/libexec/xkvm-daemon"},
		"RunAtLoad":        true,
	}), 0o644)

	sandy := filepath.Join(staging, "var", "jb", "Library", "libSandy", "Sandy_foo")
	os.MkdirAll(sandy, 0o755)
	os.WriteFile(filepath.Join(sandy, "Info.plist"), bin(map[string]any{
		"Filter": map[string]any{
			"Executables": []any{
				"/Library/MobileSubstrate/DynamicLibraries/foo.dylib",
				"/var/jb/usr/lib/foo.dylib",
				"/",
			},
		},
	}), 0o644)

	prefs := filepath.Join(staging, "var", "jb", "Library", "Preferences")
	os.MkdirAll(prefs, 0o755)
	os.WriteFile(filepath.Join(prefs, "com.xkvm.plist"), bin(map[string]any{
		"Enabled": true,
		"Name":    "xkvm",
	}), 0o644)

	in := filepath.Join(tmp, "rootless-plist.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return in
}

func head(b []byte) string {
	if len(b) > 40 {
		b = b[:40]
	}
	return fmt.Sprintf("%q", b)
}

// TestRoothideScriptsAndPlistPatches pins the plist + script layer of the
// conversion against upstream patch.sh's walk:
//
//  1. every payload .plist is plutil-converted to XML1 (so the XML-syntax
//     sed patterns match BINARY plists too — a naive raw-byte replace would
//     corrupt the length prefixes)
//  2. LaunchDaemons plists get /var/jb/ → / (they live at the real /Library)
//  3. libSandy plists get the >-root /rootfs/ dance with /var/jb paths
//     protected and restored
//  4. DEBIAN/ is walked: control scripts get the sed dance, and the control
//     file itself is left alone
func TestRoothideScriptsAndPlistPatches(t *testing.T) {
	tmp := t.TempDir()
	in := buildPlistFixture(t, tmp)
	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}

	// --- LaunchDaemons: XML + /var/jb/ stripped.
	daemon := filepath.Join(unpacked, "Library", "LaunchDaemons", "com.xkvm.daemon.plist")
	raw, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("<?xml")) {
		t.Fatalf("LaunchDaemons plist not XML after conversion; first bytes: %s", head(raw))
	}
	d, err := plist.Decode(raw)
	if err != nil {
		t.Fatalf("LaunchDaemons plist unparseable: %v", err)
	}
	if args, ok := d["ProgramArguments"].([]any); !ok || len(args) != 1 || args[0] != "/usr/libexec/xkvm-daemon" {
		t.Errorf("LaunchDaemons ProgramArguments = %v, want [/usr/libexec/xkvm-daemon]", d["ProgramArguments"])
	}

	// --- libSandy: /Library/ → /rootfs/Library/, /var/jb protected+restored,
	// bare "/" → /rootfs/ (the >/< pattern).
	sandyPath := filepath.Join(unpacked, "Library", "libSandy", "Sandy_foo", "Info.plist")
	sraw, err := os.ReadFile(sandyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sraw, []byte("<?xml")) {
		t.Fatalf("Sandy plist not XML; first bytes: %s", head(sraw))
	}
	sd, err := plist.Decode(sraw)
	if err != nil {
		t.Fatalf("Sandy plist unparseable: %v", err)
	}
	exes, ok := sd["Filter"].(map[string]any)["Executables"].([]any)
	if !ok || len(exes) != 3 {
		t.Fatalf("Sandy Executables = %v, want 3 entries", sd["Filter"])
	}
	if exes[0] != "/rootfs/Library/MobileSubstrate/DynamicLibraries/foo.dylib" {
		t.Errorf("Sandy exes[0] = %v, want /rootfs/Library/... (rootfs rewrite)", exes[0])
	}
	if exes[1] != "/usr/lib/foo.dylib" {
		t.Errorf("Sandy exes[1] = %v, want /usr/lib/foo.dylib (jbroot prefix stripped to the root)", exes[1])
	}
	if exes[2] != "/rootfs/" {
		t.Errorf("Sandy exes[2] = %v, want /rootfs/ (the >/< case)", exes[2])
	}

	// --- Plain plist: converted to XML unconditionally, values preserved.
	plain := filepath.Join(unpacked, "Library", "Preferences", "com.xkvm.plist")
	praw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(praw, []byte("<?xml")) {
		t.Fatalf("plain plist not XML after unconditional conversion; first bytes: %s", head(praw))
	}
	pd, err := plist.Decode(praw)
	if err != nil {
		t.Fatalf("plain plist unparseable: %v", err)
	}
	if pd["Name"] != "xkvm" {
		t.Errorf("plain plist Name = %v, want xkvm", pd["Name"])
	}

	// --- DEBIAN/preinst: rootful dirs → /rootfs/, the /var/jb jbroot prefix
	// is STRIPPED to the root (on roothide the jbroot IS "/"), a bare
	// /var/jb is kept (upstream's second restore), and the arch dance runs.
	// Pinned against the real upstream sed sequence.
	preinst, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "preinst"))
	if err != nil {
		t.Fatal(err)
	}
	ps := string(preinst)
	for _, want := range []string{
		"mkdir -p /rootfs/Library/MobileSubstrate/DynamicLibraries",
		"dpkg -i /usr/lib/xkvm.deb",
		"cd /var/jb && true",
		"iphoneos-arm64e",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("preinst missing %q; got:\n%s", want, ps)
		}
	}
	if strings.Contains(ps, "mkdir -p /Library/") {
		t.Errorf("preinst still creates a bare /Library/ dir; got:\n%s", ps)
	}

	// --- DEBIAN/control is not mangled by the script walk.
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ctl), "/rootfs/") {
		t.Errorf("control got mangled by the script walk; got:\n%s", ctl)
	}
}
