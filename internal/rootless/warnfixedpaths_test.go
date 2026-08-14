package rootless

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/testutil"
)

// TestRoothideWarnFixedPathsNonMachO pins the non-Mach-O half of the
// fixed-paths scan: upstream's `strings - "$file" | grep /var/jb` runs over
// every non-Mach-O payload file in the walked root, so a plain config file
// and even a binary asset must warn — while .png and .strings extensions are
// excluded exactly like upstream's find loop (`! [[ {png,strings} =~ ... ]]`).
// Pure Go (no Mach-O in the fixture), so it runs on every CI leg.
func TestRoothideWarnFixedPathsNonMachO(t *testing.T) {
	tmp := t.TempDir()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: warntweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: warn test\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(staging, "var", "jb", "usr", "lib"), 0o755)
	// A plain text config with a surviving /var/jb path → must warn.
	os.WriteFile(filepath.Join(staging, "var", "jb", "usr", "lib", "foo.conf"),
		[]byte("path=/var/jb/usr/bin/tool\n"), 0o644)
	// A binary asset with /var/jb embedded in printable runs → must warn
	// (upstream's strings extraction finds it).
	os.WriteFile(filepath.Join(staging, "var", "jb", "usr", "lib", "asset.bin"),
		[]byte("\x00\x01/var/jb/usr/bin/binary-tool\x00\x02"), 0o644)
	// .png / .strings extensions → excluded from the scan.
	os.WriteFile(filepath.Join(staging, "var", "jb", "usr", "lib", "Localizable.strings"),
		[]byte("root=/var/jb/usr/bin/strings-tool\n"), 0o644)
	os.WriteFile(filepath.Join(staging, "var", "jb", "usr", "lib", "icon.png"),
		[]byte("PNG\x00/var/jb/usr/bin/png-tool\x00"), 0o644)

	in := filepath.Join(tmp, "rootless-warn.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var buf bytes.Buffer
	log.SetWriters(&buf, &buf)
	defer log.SetWriters(os.Stdout, os.Stderr)

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	got := buf.String()
	// Upstream's banner shape: `=> <package-relative path>` (non-Mach-O
	// only) then the "fixed-paths-warnning" block with each surviving string
	// on its own line (patch.sh lines 341-347, spelling included). The paths
	// are package-root-relative: the roothide hoist moved the payload up, so
	// usr/lib/foo.conf (not var/jb/usr/lib/foo.conf).
	if !strings.Contains(got, "=> /usr/lib/foo.conf") || !strings.Contains(got, "=> /usr/lib/asset.bin") {
		t.Fatalf("expected the upstream `=> path` lines for both files; output:\n%s", got)
	}
	if n := strings.Count(got, "*****fixed-paths-warnning*****"); n != 2 {
		t.Errorf("expected 2 fixed-paths banners (one per file); got %d. output:\n%s", n, got)
	}
	for _, want := range []string{"/var/jb/usr/bin/tool", "/var/jb/usr/bin/binary-tool"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected the surviving string %q in a banner; output:\n%s", want, got)
		}
	}
	for _, mustNot := range []string{"Localizable.strings", "icon.png", "/var/jb/usr/bin/strings-tool", "/var/jb/usr/bin/png-tool"} {
		if strings.Contains(got, mustNot) {
			t.Errorf("fixed-paths warning must not mention %s (upstream find-loop exclusion); output:\n%s", mustNot, got)
		}
	}
}

// TestRoothideWarnFixedPathsMachOAudit pins the load-command audit for
// Mach-O: a dependency exactly "/var/jb" (no trailing slash) escapes the
// roothide rewrite — jbrootReplace only maps "/var/jb/" prefixes — and must
// still be surfaced, while the /var/jb/... dep that DID get rewritten to
// @loader_path/.jbroot must not be flagged.
func TestRoothideWarnFixedPathsMachOAudit(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	tweak := testutil.MakeTweak(t, tmp, "Survivor")
	b := macho.Bin{Path: tweak}
	if err := b.InjectWeak("/var/jb"); err != nil {
		t.Fatalf("InjectWeak(/var/jb): %v", err)
	}
	if err := b.InjectWeak("/var/jb/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate"); err != nil {
		t.Fatalf("InjectWeak(.jbroot dep): %v", err)
	}

	staging := filepath.Join(tmp, "staging")
	payloadDir := filepath.Join(staging, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, "Survivor.dylib")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: survivortweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: warn audit\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless-survivor.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	var buf bytes.Buffer
	log.SetWriters(&buf, &buf)
	defer log.SetWriters(os.Stdout, os.Stderr)

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	got := buf.String()
	// Exactly ONE warning: the surviving /var/jb dep. The rewritten
	// .jbroot dep (and the informational rewrite log line mentioning it)
	// must not appear in a warning — count the warning lines, not the log.
	// Exactly ONE banner: the surviving /var/jb dep (Mach-O files get no `=>`
	// line, matching upstream's elif). The rewritten .jbroot dep must not be
	// flagged — a second flagged string would be a second banner.
	if n := strings.Count(got, "*****fixed-paths-warnning*****"); n != 1 || !strings.Contains(got, "\n/var/jb\n") {
		t.Fatalf("expected exactly 1 fixed-paths banner for the surviving /var/jb dep; got %d. output:\n%s", n, got)
	}
	if strings.Contains(got, "\n/var/jb/Library/Frameworks") {
		t.Errorf("audit flagged the rewritten .jbroot dep — it must only flag real survivors; output:\n%s", got)
	}
}
