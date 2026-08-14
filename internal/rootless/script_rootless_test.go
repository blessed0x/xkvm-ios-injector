package rootless

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPatchScriptRootlessGoldenUpstream pins patchScriptForRootless byte-for-byte
// against upstream rootless-patcher's actual output (the scriptfix2 comparison
// deb, synthetic, exercising every token pattern of RPScriptHandler — see the
// member-by-member comparison in the rootless-direction fidelity work): both
// converters ran on the identical input, and xkvm's converted postinst,
// preinst, and payload script were BYTE-IDENTICAL to upstream's. These
// want-strings are upstream's bytes, captured from that run — with ONE
// deliberate divergence: the "/usr/lib/libSystem.B.dylib" token stays rootful
// in xkvm (the documented blacklist superset added for the load-command
// layer), where upstream converts it to a dangling /var/jb/usr/lib path.
//
// Firing patterns covered (token-based — unlike patch.sh's sed dance):
//   - every rootful bootstrap token: /Library /usr /var/mobile /Applications
//     /sbin /bin /lib /var/tmp → /var/jb-prefixed (via ConvertString)
//   - "$(/sbin/launchctl …)" — fires here, where a space-anchored sed would not
//   - "${INSTALL_PREFIX}/Library/…" — the {/} split makes /Library/… a clean
//     token, so it converts (the roothide-direction sed skipped this)
//   - "/var/jb" and "/System/Library" tokens — blacklisted, non-fire
//   - the double-conversion guard: a "/Library/MobileSubstrate" token whose
//     converted form "/var/jb/Library/MobileSubstrate" is already present in
//     the script is NOT rewritten
//   - '\'-continuation lines — the joined "\ /usr/bin/…" token is not
//     convertible (upstream's behavior), the split paths convert independently
//   - quoted tokens (mkdir -p "/Library/…")
//   - the shebang line itself — never converted
func TestPatchScriptRootlessGoldenUpstream(t *testing.T) {
	gotPost := string(patchScriptForRootless([]byte(scriptRootlessInputPostinst)))
	if gotPost != scriptRootlessWantPostinst {
		t.Errorf("postinst mismatch:\n--- got ---\n%s\n--- want (upstream-verified) ---\n%s", gotPost, scriptRootlessWantPostinst)
	}
	gotPre := string(patchScriptForRootless([]byte(scriptRootlessInputPreinst)))
	if gotPre != scriptRootlessWantPreinst {
		t.Errorf("preinst mismatch:\n--- got ---\n%s\n--- want (upstream-verified) ---\n%s", gotPre, scriptRootlessWantPreinst)
	}
	gotPayload := string(patchScriptForRootless([]byte(scriptRootlessInputPayload)))
	if gotPayload != scriptRootlessWantPayload {
		t.Errorf("payload script mismatch:\n--- got ---\n%s\n--- want (upstream-verified) ---\n%s", gotPayload, scriptRootlessWantPayload)
	}

	// The deliberate deviation, pinned: xkvm's blacklist superset (added for
	// the load-command layer) also protects Apple system paths in script
	// tokens — upstream converts this to a dangling /var/jb/usr/lib path.
	if !containsLine(gotPost, "cp /usr/lib/libSystem.B.dylib /var/jb/tmp/scriptfix2-copy") {
		t.Errorf("expected the blacklist-superset deviation line to survive; postinst output:\n%s", gotPost)
	}
}

// TestConvertScriptsWiring proves convertScripts picks the right files and
// preserves modes end to end (pure Go, runs on every CI leg): a DEBIAN
// postinst with NO shebang (control-script branch), a preinst with a shebang,
// a payload shebang script, a non-ASCII shebang file (skipped — upstream's
// NSASCIIStringEncoding read fails on any non-ASCII byte), and a non-script
// payload file (untouched).
func TestConvertScriptsWiring(t *testing.T) {
	dir := t.TempDir()
	debian := filepath.Join(dir, "DEBIAN")
	if err := os.MkdirAll(debian, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "var", "jb", "Library", "LaunchDaemons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "var", "jb", "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	postinst := filepath.Join(debian, "postinst")
	preinst := filepath.Join(debian, "preinst")
	payload := filepath.Join(dir, "var", "jb", "Library", "LaunchDaemons", "com.xkvm.sh")
	// Non-ASCII byte in a PAYLOAD shebang file: the shebang branch reads with
	// NSASCIIStringEncoding, which fails on any non-ASCII byte, so the file is
	// NOT a script. (The DEBIAN control-script branch has no such gate — it
	// reads binary and only excludes Mach-Os, like upstream.)
	nonASCII := filepath.Join(dir, "var", "jb", "usr", "bin", "caf.sh")
	plain := filepath.Join(dir, "var", "jb", "Library", "README.txt")

	write := func(p, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// No shebang — picked up by the control-script branch.
	write(postinst, "mkdir -p /Library/MobileSubstrate/DynamicLibraries\n", 0o755)
	// Shebang — picked up by the shebang branch.
	write(preinst, "#!/bin/sh\nmkdir -p /usr/lib/scriptfix2\n", 0o755)
	// Payload shebang script.
	write(payload, "#!/bin/sh\nlaunchctl load /Library/LaunchDaemons/com.xkvm.plist\n", 0o755)
	// Non-ASCII byte in the shebang file: upstream's ASCII read fails, so it
	// is NOT a script and stays rootful.
	write(nonASCII, "#!/bin/sh\n# caf\xc3\xa9\nmkdir -p /usr/bin/foo\n", 0o755)
	// Not a script (no shebang, not a control-script name) — untouched.
	write(plain, "some text /Library/MobileSubstrate\n", 0o644)

	if err := convertScripts(dir); err != nil {
		t.Fatalf("convertScripts: %v", err)
	}

	gotPost, _ := os.ReadFile(postinst)
	if string(gotPost) != "mkdir -p /var/jb/Library/MobileSubstrate/DynamicLibraries\n" {
		t.Errorf("postinst not converted:\n%s", gotPost)
	}
	gotPre, _ := os.ReadFile(preinst)
	if string(gotPre) != "#!/bin/sh\nmkdir -p /var/jb/usr/lib/scriptfix2\n" {
		t.Errorf("preinst not converted:\n%s", gotPre)
	}
	gotPayload, _ := os.ReadFile(payload)
	if string(gotPayload) != "#!/bin/sh\nlaunchctl load /var/jb/Library/LaunchDaemons/com.xkvm.plist\n" {
		t.Errorf("payload script not converted:\n%s", gotPayload)
	}
	gotNonASCII, _ := os.ReadFile(nonASCII)
	if string(gotNonASCII) != "#!/bin/sh\n# caf\xc3\xa9\nmkdir -p /usr/bin/foo\n" {
		t.Errorf("non-ASCII shebang file must be skipped (upstream ASCII read fails); got:\n%s", gotNonASCII)
	}
	gotPlain, _ := os.ReadFile(plain)
	if string(gotPlain) != "some text /Library/MobileSubstrate\n" {
		t.Errorf("non-script file must be untouched; got:\n%s", gotPlain)
	}

	// Modes preserved (upstream captures attributes before rewriting).
	for _, p := range []string{postinst, preinst, payload} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %o, want 755", p, info.Mode().Perm())
		}
	}
}

const scriptRootlessInputPostinst = `# postinst for scriptfix2 -- NO shebang, converted via the control-script branch
echo "postinst running"
mkdir -p /Library/MobileSubstrate/DynamicLibraries
chmod 755 /usr/bin/scriptfix2-helper
chown mobile /var/mobile/Library/scriptfix2
uicache -p /Applications/scriptfix2.app
launchctl load /Library/LaunchDaemons/com.scriptfix2.plist
if [ -d /var/jb ]; then
    INSTALL_PREFIX=/var/jb
fi
rm -f ${INSTALL_PREFIX}/Library/Preferences/com.scriptfix2.plist
result=$(/sbin/launchctl print system)
ls /bin/ls >/dev/null
ln -s /System/Library/PrivateFrameworks /lib/scriptfix2-links
cp /usr/lib/libSystem.B.dylib /var/tmp/scriptfix2-copy
mkdir -p /var/jb/Library/MobileSubstrate
mkdir -p /Library/MobileSubstrate
cp /usr/lib/scriptfix2-helper \
   /usr/bin/scriptfix2-helper
killall -9 uicache
echo "done"
`

const scriptRootlessWantPostinst = `# postinst for scriptfix2 -- NO shebang, converted via the control-script branch
echo "postinst running"
mkdir -p /var/jb/Library/MobileSubstrate/DynamicLibraries
chmod 755 /var/jb/usr/bin/scriptfix2-helper
chown mobile /var/jb/var/mobile/Library/scriptfix2
uicache -p /var/jb/Applications/scriptfix2.app
launchctl load /var/jb/Library/LaunchDaemons/com.scriptfix2.plist
if [ -d /var/jb ]; then
    INSTALL_PREFIX=/var/jb
fi
rm -f ${INSTALL_PREFIX}/var/jb/Library/Preferences/com.scriptfix2.plist
result=$(/sbin/launchctl print system)
ls /var/jb/bin/ls >/dev/null
ln -s /System/Library/PrivateFrameworks /var/jb/lib/scriptfix2-links
cp /usr/lib/libSystem.B.dylib /var/jb/tmp/scriptfix2-copy
mkdir -p /var/jb/Library/MobileSubstrate
mkdir -p /Library/MobileSubstrate
cp /var/jb/usr/lib/scriptfix2-helper \
   /var/jb/usr/bin/scriptfix2-helper
killall -9 uicache
echo "done"
`

const scriptRootlessInputPreinst = `#!/bin/sh
# preinst for scriptfix2 -- with shebang; exercises the shebang branch + quoted tokens
set -e
if [ -d /var/jb ]; then
    echo "already rootless"
else
    mkdir -p "/Library/MobileSubstrate/DynamicLibraries"
fi
mkdir -p /usr/lib/scriptfix2
exit 0
`

const scriptRootlessWantPreinst = `#!/bin/sh
# preinst for scriptfix2 -- with shebang; exercises the shebang branch + quoted tokens
set -e
if [ -d /var/jb ]; then
    echo "already rootless"
else
    mkdir -p "/var/jb/Library/MobileSubstrate/DynamicLibraries"
fi
mkdir -p /var/jb/usr/lib/scriptfix2
exit 0
`

const scriptRootlessInputPayload = `#!/bin/sh
# payload script for scriptfix2 -- shebang branch in the payload (not DEBIAN)
launchctl unload /Library/LaunchDaemons/com.scriptfix2.plist 2>/dev/null || true
killall -9 tccd 2>/dev/null || true
mkdir -p /private/var/mobile/Library/scriptfix2
mkdir -p /usr/lib/scriptfix2/data
exit 0
`

const scriptRootlessWantPayload = `#!/bin/sh
# payload script for scriptfix2 -- shebang branch in the payload (not DEBIAN)
launchctl unload /var/jb/Library/LaunchDaemons/com.scriptfix2.plist 2>/dev/null || true
killall -9 tccd 2>/dev/null || true
mkdir -p /private/var/mobile/Library/scriptfix2
mkdir -p /var/jb/usr/lib/scriptfix2/data
exit 0
`
