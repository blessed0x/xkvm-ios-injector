package rootless

import (
	"crypto/sha256"
	encodinghex "encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/plist"
)

// readPinnedFixture reads a committed fixture and asserts its sha256,
// guarding provenance: the golden must run against exactly the real
// upstream/package file, and any local edit is a failure.
func readPinnedFixture(t *testing.T, rel, sha string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "fixtures", "plists", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", rel, err)
	}
	if got := sha256.Sum256(raw); encodinghex.EncodeToString(got[:]) != sha {
		t.Fatalf("fixture %s sha256 changed (got %x, want %s) — update the pin when intentionally re-fetching", rel, got, sha)
	}
	return raw
}

// buildSandyFixture stages a rootless deb carrying the given profile under
// var/jb/Library/libSandy/ and converts it through ConvertToRoothide,
// returning the converted profile bytes.
func convertSandyProfile(t *testing.T, tmp string, profile []byte) []byte {
	t.Helper()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "var", "jb", "Library", "libSandy"), 0o755)
	if err := os.WriteFile(filepath.Join(staging, "var", "jb", "Library", "libSandy", "Profile.plist"), profile, 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: sandytweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: sandy golden\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless-sandy.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(unpacked, "Library", "libSandy", "Profile.plist"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func extPaths(t *testing.T, d plist.Dict) []string {
	t.Helper()
	exts, ok := d["Extensions"].([]any)
	if !ok {
		t.Fatalf("Extensions = %v, want an array", d["Extensions"])
	}
	var out []string
	for _, e := range exts {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("extension entry %v is not a dict", e)
		}
		p, _ := m["path"].(string)
		out = append(out, p)
	}
	return out
}

// fixtureSHASafariPlus pins the real production profile from
// opa334/SafariPlus (MIT, master) — the >/rootfs/ side of the dance.
const fixtureSHASafariPlus = "eebe8db49c44901c124ddd2185b4aa59034aa68a0d36f0981e67636f23fbc8b7"

// TestSandyGoldenRealProfile pins the libSandy plist rewrite against a REAL
// production profile: opa334/SafariPlus's SafariPlus_FileAccess.plist (MIT,
// committed at testdata/fixtures/plists/). It exercises the exact patterns
// the roothide sandy dance exists for, on real values:
//
//   - a bare "/" path — the >/< → >/rootfs/< case
//   - a /var/mobile/... path — the >/var/ → >/rootfs/var/ case
//   - a bare "/var" path (no trailing slash) — NOT rewritten, because
//     upstream's >/var/ pattern requires the slash; pinned as the
//     reference's quirk on real data
//
// Pure Go (the fixture is a plist, no Mach-O), so it runs on every CI leg.
func TestSandyGoldenRealProfile(t *testing.T) {
	raw := readPinnedFixture(t, "SafariPlus_FileAccess.plist", fixtureSHASafariPlus)
	if _, err := plist.Decode(raw); err != nil {
		t.Fatalf("fixture is not a valid plist: %v", err)
	}

	data := convertSandyProfile(t, t.TempDir(), raw)
	if !strings.HasPrefix(string(data), "<?xml") {
		t.Fatalf("converted plist not XML; first bytes: %q", data[:min(40, len(data))])
	}
	d, err := plist.Decode(data)
	if err != nil {
		t.Fatalf("converted plist unparseable: %v", err)
	}

	paths := extPaths(t, d)
	if len(paths) != 2 {
		t.Fatalf("Extensions = %v, want 2 entries", paths)
	}
	// The bare "/" (read-everything extension) becomes the jbroot root.
	if paths[0] != "/rootfs/" {
		t.Errorf("extension[0].path = %q, want /rootfs/ (the >/< case)", paths[0])
	}
	// The bare "/var" has no trailing slash — upstream's >/var/ pattern
	// requires it, so the value survives untouched (reference quirk).
	if paths[1] != "/var" {
		t.Errorf("extension[1].path = %q, want /var unchanged (no trailing slash — upstream quirk)", paths[1])
	}

	// The /var/mobile/... condition path is user data on the real root,
	// which roothide exposes at /rootfs.
	conds, ok := d["Conditions"].([]any)
	if !ok || len(conds) != 1 {
		t.Fatalf("Conditions = %v, want 1 entry", d["Conditions"])
	}
	fp, _ := conds[0].(map[string]any)["FilePath"].(string)
	if fp != "/rootfs/var/mobile/Library/Preferences/com.opa334.safariplusprefs.force_sandbox" {
		t.Errorf("Conditions[0].FilePath = %q, want /rootfs/var/mobile/... (the >/var/ case)", fp)
	}
	if !strings.Contains(string(data), "/rootfs/var/mobile/") {
		t.Errorf("raw converted plist missing the /rootfs/var/ rewrite:\n%s", data)
	}
}

// fixtureSHAAlbumManager pins the real rootless-era profile shipped in the
// AlbumManager 1.0.2 iphoneos-arm64 deb (com.noisyflake.albummanager) on
// the Chariz repo — a BINARY plist whose extensions carry BOTH a rootful
// user-data path and its /var/jb twin, the exact input the protect/strip
// half of the sandy dance exists for.
const fixtureSHAAlbumManager = "b0f869fb92b7fdab6058e6b3db56a88474b4ddef9cd21800b12fdfcb25f6190f"

// TestSandyGoldenRootlessProfile pins the jbroot-strip side of the sandy
// rewrite against a real BINARY rootless profile: AlbumManager's
// AlbumManager_FileAccess.plist. Its two read-write extensions are sibling
// paths that must diverge:
//
//   - /var/mobile/Library/Preferences/  → /rootfs/var/mobile/... (user data
//     on the real root, exposed at /rootfs on roothide)
//   - /var/jb/var/mobile/Library/Preferences/ → /var/mobile/... (the
//     /var/jb jbroot prefix STRIPPED to the root — the protect/unprotect
//     dance keeps it away from the rootfs rewrites, then strips it)
//
// Without the protect step, the second path would be rootfs-prefixed into
// the broken /rootfs/var/jb/... — the exact failure this test catches.
func TestSandyGoldenRootlessProfile(t *testing.T) {
	raw := readPinnedFixture(t, "AlbumManager_FileAccess.plist", fixtureSHAAlbumManager)
	// Provenance sanity: the fixture is a binary plist as shipped.
	if _, err := plist.Decode(raw); err != nil {
		t.Fatalf("fixture is not a valid plist: %v", err)
	}

	data := convertSandyProfile(t, t.TempDir(), raw)
	if !strings.HasPrefix(string(data), "<?xml") {
		t.Fatalf("converted plist not XML (binary → xml1 conversion failed); first bytes: %q", data[:min(40, len(data))])
	}
	d, err := plist.Decode(data)
	if err != nil {
		t.Fatalf("converted plist unparseable: %v", err)
	}

	paths := extPaths(t, d)
	if len(paths) != 2 {
		t.Fatalf("Extensions = %v, want 2 entries", paths)
	}
	if paths[0] != "/rootfs/var/mobile/Library/Preferences/" {
		t.Errorf("extension[0].path = %q, want /rootfs/var/mobile/... (rootfs rewrite)", paths[0])
	}
	if paths[1] != "/var/mobile/Library/Preferences/" {
		t.Errorf("extension[1].path = %q, want /var/mobile/... (jbroot prefix stripped)", paths[1])
	}
	if strings.Contains(string(data), "/rootfs/var/jb/") {
		t.Errorf("converted plist contains the broken /rootfs/var/jb/ path — the protect step must keep /var/jb away from the rootfs rewrites:\n%s", data)
	}

	// The consumer whitelist survives untouched.
	procs, ok := d["AllowedProcesses"].([]any)
	if !ok || len(procs) != 1 || procs[0] != "com.apple.mobileslideshow" {
		t.Errorf("AllowedProcesses = %v, want [com.apple.mobileslideshow]", d["AllowedProcesses"])
	}
}
