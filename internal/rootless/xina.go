// Xina-style rootless conversion (rootful deb -> rootless deb), a faithful
// semantics port of the Xinam1nePatcher script shipped in the Xinam1ne AIO
// package (CyPwn/Paisseon; reference only — see NOTICE). The Xina jailbreak
// exposes short bootstrap paths as symlinks created at jailbreak time
// (Bootstrapper.swift), and the patcher rewrites rootful paths in binaries
// to those short forms so rootful tweaks resolve through the symlinks:
//
//	/var/jb/usr/lib     <- /var/lib           /var/jb/usr/sbin   <- /var/sbin
//	/var/jb/usr/lib     <- /var/Lib           /var/jb/usr/cache  <- /var/cache
//	/var/jb/usr/libexec <- /var/libexec       /var/jb/usr/share  <- /var/share
//	/var/jb/usr/bin     <- /var/bin           /var/jb/System     <- /var/sy
//	/var/jb/Library     <- /var/LIY           /var/jb/bin/bash   <- /var/bash
//	/var/jb/Library/dpkg <- /var/lib/dpkg     /var/jb/usr/local  <- /var/local
//
// So a rootful binary whose string table says "/Library/Frameworks/…"
// becomes "/var/LIY/Frameworks/…", "/usr/lib/…" becomes "/var/lib/…", and
// "/usr/bin/…" becomes "/var/bin/…" — each resolving via one of the
// jailbreak-side symlinks above. Apple system libs are exempted (the
// revert-exception seds keep libobjc/libc++/libSystem/libstdc++/
// libMobileGestalt at /usr/lib).
//
// The pipeline (mirroring the reference script's order):
//  1. repack — payload under var/jb (standard rootless repack)
//  2. control — Architecture → iphoneos-arm64, Name gains " (Xinafied-rootless)"
//  3. Mach-O — install name → @rpath/<basename>; CydiaSubstrate dep →
//     @rpath/libsubstrate.dylib; /System/Library dylib deps → @rpath/<basename>;
//     add the /var/jb/Library/Frameworks + /var/jb/usr/lib rpaths; then the
//     byte-level Xina seds (whole-file NUL-anchored replacements) with the
//     revert exceptions last; re-signed ad-hoc like the reference's `ldid -S`
//  4. plists — the >-root sed sequence (any .plist in the payload, DEBIAN
//     included, exactly like the reference's find loop)
//  5. DEBIAN scripts — the space-anchored sed sequence on
//     preinst/postinst/prerm/postrm (the reference's second find loop)
package rootless

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/log"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
)

// xinaSymlinks documents the jailbreak-side symlink set the byte-seds
// target (Bootstrapper.swift symlinks array): dest <- target, i.e. a
// symlink AT dest pointing INTO /var/jb. Purely documentary — the jailbreak
// creates these at boot, never the deb converter.
var xinaSymlinks = []struct{ dest, target string }{
	{"/var/lib", "/var/jb/usr/lib"},
	{"/var/lib/dpkg", "/var/jb/Library/dpkg"},
	{"/var/Lib", "/var/jb/usr/lib"},
	{"/var/libexec", "/var/jb/usr/libexec"},
	{"/var/bin", "/var/jb/usr/bin"},
	{"/var/sy", "/var/jb/System"},
	{"/var/LIY", "/var/jb/Library"},
	{"/var/sbin", "/var/jb/usr/sbin"},
	{"/var/cache", "/var/jb/usr/cache"},
	{"/var/share", "/var/jb/usr/share"},
	{"/var/dpkg", "/var/jb/etc/dpkg"},
	{"/var/alternatives", "/var/jb/etc/alternatives"},
	{"/var/jb/Xapps", "/var/jb/Applications"},
	{"/var/jb/UsrLb", "/var/jb/User/Library"},
	{"/var/jb/vmo", "/var/jb/var/mobile"},
	{"/var/bash", "/var/jb/bin/bash"},
	{"/var/local", "/var/jb/usr/local"},
}

// xinaByteSeds are the reference script's binary-level replacements, in
// order, with the NUL anchor baked into each pattern exactly like the
// reference's `sed -i 's#\x00/...#\x00/...#'`. NUL-anchored, so they fire on
// string-table entries (each string is NUL-terminated, so the next string
// begins after a NUL) and never on load-command bytes (whose name strings
// are preceded by cmd fields, not NULs).
var xinaByteSeds = []struct{ from, to string }{
	{"\x00/Library/MobileSub", "\x00/var/LIY/MobileSub"},
	{"\x00/Library/Sn", "\x00/var/LIY/Sn"},
	{"\x00/Library/Th", "\x00/var/LIY/Th"},
	{"\x00/Library/Application Support", "\x00/var/LIY/Application Support"},
	{"\x00/Library/LaunchD", "\x00/var/LIY/LaunchD"},
	{"\x00/Library/PreferenceB", "\x00/var/LIY/PreferenceB"},
	{"\x00/Library/PreferenceL", "\x00/var/LIY/PreferenceL"},
	{"\x00/Library/Frameworks", "\x00/var/LIY/Frameworks"},
	{"\x00/bin/sh", "\x00/var/sh"},
	{"\x00/usr/lib", "\x00/var/lib"},
	{"\x00/usr/bin", "\x00/var/bin"},
}

// xinaRevertExceptions restore Apple system libs to /usr/lib after the base
// seds (the reference's second sed batch; order matters — /usr/lib/... would
// otherwise have been rewritten to /var/lib/...).
var xinaRevertExceptions = []struct{ from, to string }{
	{"/var/lib/libobjc.A.dylib", "/usr/lib/libobjc.A.dylib"},
	{"/var/lib/libc++.1.dylib", "/usr/lib/libc++.1.dylib"},
	{"/var/lib/libSystem.B.dylib", "/usr/lib/libSystem.B.dylib"},
	{"/var/lib/libstdc++.6.dylib", "/usr/lib/libstdc++.6.dylib"},
	{"/var/lib/libMobileGestalt.dylib", "/usr/lib/libMobileGestalt.dylib"},
}

// xinaPlistSeds are the reference's >-root plist sequence, order-sensitive
// (the /usr/lib and /Library/i entries must run before the /usr and
// /Library catch-alls).
var xinaPlistSeds = []struct{ from, to string }{
	{">/Applications/", ">/var/jb/Applications/"},
	{">/Library/i", ">/var/LIY/i"},
	{">/usr/share/", ">/var/share/"},
	{">/usr/bin/", ">/var/bin/"},
	{">/usr/lib/", ">/var/lib/"},
	{">/usr/sbin/", ">/var/sbin/"},
	{">/usr/libexec/", ">/var/libexec/"},
	{">/usr/", ">/var/jb/usr/"},
	{">/etc/", ">/var/etc/"},
	{">/bin/", ">/var/bin/"},
	{">/Library/", ">/var/LIY/"},
}

// xinaScriptSeds are the reference's DEBIAN-script sequence (space-anchored
// instead of >-anchored), same order-sensitivity.
var xinaScriptSeds = []struct{ from, to string }{
	{" /Applications/", " /var/jb/Applications/"},
	{" /Library/i", " /var/LIY/i"},
	{" /usr/share/", " /var/share/"},
	{" /usr/bin/", " /var/bin/"},
	{" /usr/lib/", " /var/lib/"},
	{" /usr/sbin/", " /var/sbin/"},
	{" /usr/libexec/", " /var/libexec/"},
	{" /usr/", " /var/jb/usr/"},
	{" /etc/", " /var/etc/"},
	{" /bin/", " /var/bin/"},
	{" /Library/", " /var/LIY/"},
}

// xinaControlScriptNames is the reference's second find loop basename match.
var xinaControlScriptNames = []string{"preinst", "postinst", "prerm", "postrm"}

// ConvertToXina converts a rootful deb at input to a Xina-style rootless deb
// at output (the Xinam1nePatcher semantics). An already-rootless input is
// rebuilt unchanged, matching the reference's early skip (`[ -d "$OLD/var/jb"
// ]` -> "Deb already rootless. Skipping and exiting cleanly.").
func ConvertToXina(input, output string) error {
	tmpdir, err := os.MkdirTemp("", "xkvm-xina-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log.Infof("unpacking %s..", input)
	if err := deb.Unpack(input, tmpdir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(tmpdir, "var", "jb")); err == nil {
		log.Infof("deb already rootless; skipping and exiting cleanly")
		return nil
	}

	if err := repackRootless(tmpdir); err != nil {
		return err
	}
	if err := editControlXina(tmpdir); err != nil {
		return err
	}
	if err := patchPayloadForXina(tmpdir); err != nil {
		return err
	}

	if dir := filepath.Dir(output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := deb.Build(tmpdir, output); err != nil {
		return fmt.Errorf("repacking %s: %w", output, err)
	}
	log.Infof("wrote Xina-style rootless deb at %s", output)
	return nil
}

// editControlXina applies the reference's control edits: Architecture →
// iphoneos-arm64 (the whole-file sed `s|iphoneos-arm|iphoneos-arm64|` in the
// reference rewrites the Architecture field; xkvm scopes it to the field to
// avoid corrupting Description text that mentions the arch) and Name gains
// " (Xinafied-rootless)".
func editControlXina(dir string) error {
	path := filepath.Join(dir, "DEBIAN", "control")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warnf("no DEBIAN/control in package; skipping control edits")
			return nil
		}
		return err
	}
	entries := parseControl(string(data))
	edits := 0
	for i := range entries {
		switch entries[i].key {
		case "Architecture":
			if entries[i].value != "iphoneos-arm64" {
				entries[i].value = "iphoneos-arm64"
				edits++
			}
		case "Name":
			if !strings.HasSuffix(entries[i].value, "(Xinafied-rootless)") {
				entries[i].value += " (Xinafied-rootless)"
				edits++
			}
		}
	}
	if err := os.WriteFile(path, []byte(renderControl(entries)), 0o644); err != nil {
		return err
	}
	if edits > 0 {
		log.Infof("control edited (%d field(s))", edits)
	}
	return nil
}

// patchPayloadForXina walks the whole package (DEBIAN included, like the
// reference's find loop) and applies the per-file-type transformations:
// Mach-Os get the install_name/rpath edits plus the byte-level seds and are
// re-signed; .plist files get the >-root seds; then DEBIAN control scripts
// get the space-anchored seds.
func patchPayloadForXina(dir string) error {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		is, err := macho.IsMachO(path)
		if err != nil {
			return err
		}
		if is {
			return patchMachOForXina(path, rel)
		}
		if strings.HasSuffix(strings.ToLower(filepath.Base(rel)), ".plist") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out := applySeds(data, xinaPlistSeds)
			if !bytes.Equal(out, data) {
				if err := os.WriteFile(path, out, 0o644); err != nil {
					return err
				}
				log.Infof("patched plist %s", rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The reference's second find loop: DEBIAN control scripts.
	return filepath.WalkDir(filepath.Join(dir, "DEBIAN"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		for _, name := range xinaControlScriptNames {
			if d.Name() != name {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out := applySeds(data, xinaScriptSeds)
			if !bytes.Equal(out, data) {
				if err := os.WriteFile(path, out, 0o755); err != nil {
					return err
				}
				log.Infof("patched DEBIAN script %s", name)
			}
		}
		return nil
	})
}

// applySeds applies an ordered list of byte replacements.
func applySeds(data []byte, seds []struct{ from, to string }) []byte {
	out := data
	for _, s := range seds {
		out = bytes.ReplaceAll(out, []byte(s.from), []byte(s.to))
	}
	return out
}

// systemDylibForRpath mirrors the reference's lib_cache grep
// (`grep -e /System | grep /Library/'[^/]'\*.dylib`): a /System dependency
// that is a DIRECT child of a /Library/ directory, ending in .dylib.
// Framework deps (Foundation.framework/Foundation, ...) never match — the
// reference leaves them rootful, and so does the port: they resolve from the
// real /System on-device, while an @rpath/<basename> form would dangle
// (nothing ships the framework under the added /var/jb rpaths).
func systemDylibForRpath(dep string) bool {
	if !strings.HasPrefix(dep, "/System/") {
		return false
	}
	i := strings.LastIndex(dep, "/Library/")
	if i < 0 {
		return false
	}
	tail := dep[i+len("/Library/"):]
	base := strings.TrimSuffix(tail, ".dylib")
	return base != tail && !strings.Contains(base, "/")
}

// patchMachOForXina ports the reference's Mach-O block: install name →
// @rpath/<basename>; CydiaSubstrate dep → @rpath/libsubstrate.dylib;
// /System/Library dylib deps (the narrow lib_cache grep) →
// @rpath/<basename>; the two Xina rpaths; then the byte-level seds with
// revert exceptions; re-signed ad-hoc (the reference's `ldid -S` —
// entitlements preserved).
func patchMachOForXina(path, rel string) error {
	b := macho.Bin{Path: path}
	var origEnts []byte
	if ents, err := b.ExtractEntitlements(); err == nil {
		origEnts = ents
	}
	if err := b.RemoveSignature(); err != nil {
		return fmt.Errorf("removing signature from %s: %w", rel, err)
	}

	if id, err := b.InstallName(); err == nil && id != "" {
		converted := "@rpath/" + filepath.Base(id)
		log.Infof("rewriting install name of %s: %s -> %s", rel, id, converted)
		if err := b.SetInstallName(converted); err != nil {
			return fmt.Errorf("rewriting install name of %s: %w", rel, err)
		}
	}

	deps, err := b.AllDependencies()
	if err != nil {
		return fmt.Errorf("reading dependencies of %s: %w", rel, err)
	}
	for _, dep := range deps {
		var converted string
		switch {
		case strings.Contains(dep, "CydiaSubstrate.framework"):
			converted = "@rpath/libsubstrate.dylib"
		case systemDylibForRpath(dep):
			converted = "@rpath/" + filepath.Base(dep)
		default:
			continue
		}
		log.Infof("rewriting %s: %s -> %s", rel, dep, converted)
		if err := b.ChangeDependency(dep, converted); err != nil {
			return fmt.Errorf("rewriting dependency of %s: %w", rel, err)
		}
	}
	if err := b.AddRpath("/var/jb/Library/Frameworks"); err != nil {
		return fmt.Errorf("adding Xina rpath to %s: %w", rel, err)
	}
	if err := b.AddRpath("/var/jb/usr/lib"); err != nil {
		return fmt.Errorf("adding Xina rpath to %s: %w", rel, err)
	}

	// Byte-level seds on the whole file (the reference runs sed -i on the
	// binary; NUL-anchored patterns target string tables), then the revert
	// exceptions, then re-sign.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out := applySeds(data, xinaByteSeds)
	out = applySeds(out, xinaRevertExceptions)
	if !bytes.Equal(out, data) {
		if err := os.WriteFile(path, out, 0o755); err != nil {
			return err
		}
		log.Infof("applied Xina byte-seds to %s", rel)
	}
	if err := b.SignWithEntitlements(origEnts); err != nil {
		return fmt.Errorf("signing %s: %w", rel, err)
	}
	log.Infof("patched Mach-O %s (Xina)", rel)
	return nil
}
