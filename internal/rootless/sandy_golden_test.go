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

// fixtureSHA is the sha256 of the committed SafariPlus_FileAccess.plist,
// guarding the fixture's provenance: the test must run against exactly the
// real production profile, and any local edit is a failure.
const fixtureSHA = "eebe8db49c44901c124ddd2185b4aa59034aa68a0d36f0981e67636f23fbc8b7"

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
	fixture := filepath.Join("..", "..", "testdata", "fixtures", "plists", "SafariPlus_FileAccess.plist")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if got := sha256.Sum256(raw); encodinghex.EncodeToString(got[:]) != fixtureSHA {
		t.Fatalf("fixture sha256 changed (got %x, want %s) — update fixtureSHA when intentionally re-fetching upstream", got, fixtureSHA)
	}
	if _, err := plist.Decode(raw); err != nil {
		t.Fatalf("fixture is not a valid plist: %v", err)
	}

	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "var", "jb", "Library", "libSandy"), 0o755)
	if err := os.WriteFile(filepath.Join(staging, "var", "jb", "Library", "libSandy", "SafariPlus_FileAccess.plist"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: safariplus\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: sandy golden\nMaintainer: xkvm\n"
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
	data, err := os.ReadFile(filepath.Join(unpacked, "Library", "libSandy", "SafariPlus_FileAccess.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "<?xml") {
		t.Fatalf("converted plist not XML; first bytes: %q", data[:min(40, len(data))])
	}
	d, err := plist.Decode(data)
	if err != nil {
		t.Fatalf("converted plist unparseable: %v", err)
	}

	exts, ok := d["Extensions"].([]any)
	if !ok || len(exts) != 2 {
		t.Fatalf("Extensions = %v, want 2 entries", d["Extensions"])
	}
	pathOf := func(e any) string {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("extension entry %v is not a dict", e)
		}
		p, _ := m["path"].(string)
		return p
	}
	// The bare "/" (read-everything extension) becomes the jbroot root.
	if got := pathOf(exts[0]); got != "/rootfs/" {
		t.Errorf("extension[0].path = %q, want /rootfs/ (the >/< case)", got)
	}
	// The bare "/var" has no trailing slash — upstream's >/var/ pattern
	// requires it, so the value survives untouched (reference quirk).
	if got := pathOf(exts[1]); got != "/var" {
		t.Errorf("extension[1].path = %q, want /var unchanged (no trailing slash — upstream quirk)", got)
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
