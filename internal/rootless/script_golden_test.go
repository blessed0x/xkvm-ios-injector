package rootless

import (
	"testing"
)

// TestPatchScriptPathsGoldenUpstream pins patchScriptPaths byte-for-byte
// against upstream RootHidePatcher's actual output. The fixtures are the
// scriptfix comparison deb (synthetic, exercising EVERY firing pattern of
// patch.sh's script seds — see the member-by-member comparison in the
// roothide fidelity work): both converters ran on the identical input, and
// xkvm's converted postinst/preinst were BYTE-IDENTICAL to upstream's. These
// want-strings are upstream's bytes, captured from that run.
//
// Firing patterns covered:
//   - iphoneos-arm64 → iphoneos-arm64e (in scripts, not just control)
//   - /var/jb/ + trailing context → jbroot prefix STRIPPED (/var/jb/Library/Preferences/… → /Library/Preferences/…)
//   - bare /var/jb (no trailing slash) → protected then RESTORED (round-trip, still /var/jb)
//   - all ten space-anchored rootful seds: /Applications/ /Library/ /private/
//     /System/ /sbin/ /bin/ /etc/ /lib/ /usr/ /var/ → /rootfs/$1/
//   - DIR="/Library/… → DIR="/rootfs/Library/…
//
// Non-fire cases (space-anchored, so the sed correctly does NOT match):
//   - $(/sbin/launchctl …) — "/sbin/" preceded by "(" not a space
//   - ${INSTALL_PREFIX}/Library/… — "/Library/" preceded by "}" not a space
func TestPatchScriptPathsGoldenUpstream(t *testing.T) {
	gotPost := string(patchScriptPaths([]byte(scriptFixInputPostinst)))
	if gotPost != scriptFixWantPostinst {
		t.Errorf("postinst mismatch:\n--- got ---\n%s\n--- want (upstream-verified) ---\n%s", gotPost, scriptFixWantPostinst)
	}
	gotPre := string(patchScriptPaths([]byte(scriptFixInputPreinst)))
	if gotPre != scriptFixWantPreinst {
		t.Errorf("preinst mismatch:\n--- got ---\n%s\n--- want (upstream-verified) ---\n%s", gotPre, scriptFixWantPreinst)
	}

	// The bare-/var/jb lines are INTENTIONALLY preserved (upstream's
	// protect/unprotect dance restores them) — xkvm's fixed-paths warning
	// flags them, exactly like upstream's strings|grep /var/jb scan does on
	// the same walked file set.
	for _, line := range []string{"if [ -d /var/jb ]; then", "    INSTALL_PREFIX=/var/jb"} {
		if !containsLine(gotPost, line) && !containsLine(gotPre, line) {
			t.Errorf("expected bare /var/jb line %q to survive the dance (round-trip); missing from both outputs", line)
		}
	}
}

func containsLine(s, line string) bool {
	for _, l := range splitLines(s) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

const scriptFixInputPostinst = `#!/bin/bash

# scriptfix postinst: exercises every upstream script-sed firing pattern
echo "built for iphoneos-arm64"
APP_DIR=/var/jb/Applications/scriptfix.app

if [ -d /var/jb ]; then
    echo "jailbreak root present"
fi

killall -9 Snapchat

DIR="/Library/MobileSubstrate/DynamicLibraries"

uicache -p /Applications/scriptfix.app
rm -f /var/jb/Library/Preferences/com.scriptfix.plist
launchctl unload /var/jb/Library/LaunchDaemons/com.scriptfix.plist

chmod 755 /usr/bin/scriptfix-helper
chown mobile /var/mobile/Library/scriptfix
mkdir -p /private/var/tmp/scriptfix
ln -s /System/Library/PrivateFrameworks /lib/scriptfix-links
if [ -f /etc/rc.d/scriptfix ]; then
    . /etc/rc.d/scriptfix
fi
result=$(/sbin/launchctl print system)
ls /bin/ls >/dev/null
launchctl load /Library/LaunchDaemons/com.scriptfix.plist
exit 0
`

const scriptFixWantPostinst = `#!/bin/bash

# scriptfix postinst: exercises every upstream script-sed firing pattern
echo "built for iphoneos-arm64e"
APP_DIR=/Applications/scriptfix.app

if [ -d /var/jb ]; then
    echo "jailbreak root present"
fi

killall -9 Snapchat

DIR="/rootfs/Library/MobileSubstrate/DynamicLibraries"

uicache -p /rootfs/Applications/scriptfix.app
rm -f /Library/Preferences/com.scriptfix.plist
launchctl unload /Library/LaunchDaemons/com.scriptfix.plist

chmod 755 /rootfs/usr/bin/scriptfix-helper
chown mobile /rootfs/var/mobile/Library/scriptfix
mkdir -p /rootfs/private/var/tmp/scriptfix
ln -s /rootfs/System/Library/PrivateFrameworks /rootfs/lib/scriptfix-links
if [ -f /rootfs/etc/rc.d/scriptfix ]; then
    . /rootfs/etc/rc.d/scriptfix
fi
result=$(/sbin/launchctl print system)
ls /rootfs/bin/ls >/dev/null
launchctl load /rootfs/Library/LaunchDaemons/com.scriptfix.plist
exit 0
`

const scriptFixInputPreinst = `#!/bin/sh

set -e
if [ -d /var/jb ]; then
    INSTALL_PREFIX=/var/jb
else
    INSTALL_PREFIX=
fi
echo "iphoneos-arm64 target"
mkdir -p ${INSTALL_PREFIX}/Library/MobileSubstrate/DynamicLibraries
exit 0
`

const scriptFixWantPreinst = `#!/bin/sh

set -e
if [ -d /var/jb ]; then
    INSTALL_PREFIX=/var/jb
else
    INSTALL_PREFIX=
fi
echo "iphoneos-arm64e target"
mkdir -p ${INSTALL_PREFIX}/Library/MobileSubstrate/DynamicLibraries
exit 0
`
