// Rootful conversion (rootless deb -> rootful deb), the reverse of both the
// standard rootless pipeline (rootless-patcher) and the Xina-style pipeline
// (Xinam1nePatcher — see NOTICE). It undoes what those converters did, so a
// converted deb round-trips:
//
//  1. repack — the var/jb payload is hoisted to the package root
//     (DEBIAN/ stays at the top), the reverse of repackRootless
//  2. control — Architecture iphoneos-arm64 → iphoneos-arm, the rootless
//     runtime dependency (cy+cpu.arm64v8 | oldabi-xina | oldabi) is dropped
//     from Depends, and a " (Xinafied-rootless)" Name suffix is stripped
//  3. Mach-O — load-command dependencies and install names are restored:
//     /var/jb/... prefixes stripped, @rpath/libsubstrate.dylib back to
//     /Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate, and
//     @rpath/<basename> install names resolved to the file's real package
//     path when the file ships in the package (otherwise left with a
//     warning — the original absolute path is unrecoverable); /var/jb
//     LC_RPATH entries are removed. The NUL-anchored byte-level inverse seds
//     restore string-table paths (the /var/LIY, /var/lib, /var/bin, /var/sh
//     Xina short forms and /var/jb prefixes), and __TEXT.__cstring dlopen
//     strings get the same inverse mapping (in place: the reverse mappings
//     never grow strings). Re-signed ad-hoc with the preserved entitlements.
//  4. plists — the inverse of the Xina >-root sed sequence
//  5. DEBIAN scripts — the inverse of the space-anchored sequence
//
// Lossiness is inherent to the forward seds: the Xina script collapses
// /usr/bin and /bin (and /usr/lib / /usr/share) into a single short form, so
// the reverse picks the more common rootful path (/usr/bin, /usr/lib, ...).
// A @rpath/<basename> dependency with no matching file in the package (a
// system framework the forward converter renamed) is left as-is and warned —
// the original /System path cannot be recovered.
package rootless

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
	"github.com/xscope0/xkvm-ios-injector/internal/macho"
)

// rootfulByteSeds are the inverse of the Xina byte-seds, NUL-anchored
// exactly like the forward direction (the reference's
// `sed -i 's#\x00/...#\x00/...#'`): they fire on string-table entries, never
// on load-command bytes. Every mapping is SAME-LENGTH (a byte-sed that
// shrinks would shift every offset after the match and corrupt the Mach-O),
// so only the Xina short forms are restored here; /var/jb paths are undone
// by the load-command ops (ChangeDependency/SetInstallName/RemoveRpath) and
// the __cstring rewrite (rootfulString) — the only places the standard
// rootless converter writes them.
var rootfulByteSeds = []struct{ from, to string }{
	{"\x00/var/LIY/MobileSub", "\x00/Library/MobileSub"},
	{"\x00/var/LIY/Sn", "\x00/Library/Sn"},
	{"\x00/var/LIY/Th", "\x00/Library/Th"},
	{"\x00/var/LIY/Application Support", "\x00/Library/Application Support"},
	{"\x00/var/LIY/LaunchD", "\x00/Library/LaunchD"},
	{"\x00/var/LIY/PreferenceB", "\x00/Library/PreferenceB"},
	{"\x00/var/LIY/PreferenceL", "\x00/Library/PreferenceL"},
	{"\x00/var/LIY/Frameworks", "\x00/Library/Frameworks"},
	{"\x00/var/sh", "\x00/bin/sh"},
	{"\x00/var/lib", "\x00/usr/lib"},
	{"\x00/var/bin", "\x00/usr/bin"},
}

// rootfulPlistSeds are the inverse of the Xina >-root plist sequence. The
// forward collapsed /usr/bin and /bin into /var/bin; the reverse emits
// /usr/bin (the common case), the same lossy choice for /usr/lib vs /usr/share.
var rootfulPlistSeds = []struct{ from, to string }{
	{">/var/jb/Applications/", ">/Applications/"},
	{">/var/LIY/i", ">/Library/i"},
	{">/var/share/", ">/usr/share/"},
	{">/var/bin/", ">/usr/bin/"},
	{">/var/lib/", ">/usr/lib/"},
	{">/var/sbin/", ">/usr/sbin/"},
	{">/var/libexec/", ">/usr/libexec/"},
	{">/var/jb/usr/", ">/usr/"},
	{">/var/etc/", ">/etc/"},
	{">/var/LIY/", ">/Library/"},
}

// rootfulScriptSeds are the DEBIAN-script inverse (space-anchored), mirroring
// the plist set.
var rootfulScriptSeds = []struct{ from, to string }{
	{" /var/jb/Applications/", " /Applications/"},
	{" /var/LIY/i", " /Library/i"},
	{" /var/share/", " /usr/share/"},
	{" /var/bin/", " /usr/bin/"},
	{" /var/lib/", " /usr/lib/"},
	{" /var/sbin/", " /usr/sbin/"},
	{" /var/libexec/", " /usr/libexec/"},
	{" /var/jb/usr/", " /usr/"},
	{" /var/etc/", " /etc/"},
	{" /var/LIY/", " /Library/"},
}

// ConvertToRootful converts a rootless deb at input to a rootful deb at
// output. Inputs already rootful (no var/jb payload) are rebuilt unchanged,
// matching the forward converters' early skip. A roothide-layout input
// (payload hoisted, rootfs/ split) is NOT detected as rootless and is skipped
// as "already rootful" — its @loader_path/.jbroot references are left as-is
// (convert it with 'xkvm roothide' first, or re-extract from the rootless
// source).
func ConvertToRootful(input, output string) error {
	tmpdir, err := os.MkdirTemp("", "xkvm-rootful-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log.Infof("unpacking %s..", input)
	if err := deb.Unpack(input, tmpdir); err != nil {
		return err
	}
	jb := filepath.Join(tmpdir, "var", "jb")
	if _, err := os.Stat(jb); err != nil {
		if _, err := os.Stat(filepath.Join(tmpdir, "rootfs")); err == nil {
			log.Warnf("input looks roothide-layout (rootfs/ present); @loader_path/.jbroot references will not be converted")
		}
		log.Infof("deb already rootful; skipping and exiting cleanly")
		return nil
	}

	if err := hoistRootlessPayload(tmpdir); err != nil {
		return err
	}
	if err := editControlRootful(tmpdir); err != nil {
		return err
	}
	if err := patchPayloadForRootful(tmpdir); err != nil {
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
	log.Infof("wrote rootful deb at %s", output)
	return nil
}

// hoistRootlessPayload moves var/jb/* up to the package root, the exact
// inverse of repackRootless, then removes the empty var/jb and var dirs.
func hoistRootlessPayload(dir string) error {
	jb := filepath.Join(dir, "var", "jb")
	entries, err := os.ReadDir(jb)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "DEBIAN" {
			continue
		}
		if err := os.Rename(filepath.Join(jb, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	_ = os.Remove(jb)
	_ = os.Remove(filepath.Join(dir, "var"))
	log.Infof("payload hoisted out of var/jb (rootful layout)")
	return nil
}

// editControlRootful reverses the forward control edits: Architecture
// iphoneos-arm64 → iphoneos-arm, the rootless runtime alternation dropped
// from Depends (removing the field entirely when it becomes empty), and the
// " (Xinafied-rootless)" Name suffix stripped.
func editControlRootful(dir string) error {
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
	kept := entries[:0]
	edits := 0
	for _, e := range entries {
		orig := e.value
		switch e.key {
		case "Architecture":
			if e.value == "iphoneos-arm64" {
				e.value = "iphoneos-arm"
			}
		case "Depends":
			var parts []string
			for _, d := range strings.Split(e.value, ",") {
				if strings.TrimSpace(d) == RuntimeDep {
					continue
				}
				parts = append(parts, strings.TrimSpace(d))
			}
			e.value = strings.Join(parts, ", ")
			if e.value == "" {
				// The runtime dep was the whole Depends: drop the field.
				edits++
				continue
			}
		case "Name":
			e.value = strings.TrimSuffix(e.value, " (Xinafied-rootless)")
		}
		if e.value != orig {
			edits++
		}
		kept = append(kept, e)
	}
	if err := os.WriteFile(path, []byte(renderControl(kept)), 0o644); err != nil {
		return err
	}
	if edits > 0 {
		log.Infof("control edited (%d field(s))", edits)
	}
	return nil
}

// patchPayloadForRootful walks the whole package (DEBIAN included) applying
// the per-file-type inverses: Mach-Os get load-command + string-table path
// restoration and are re-signed; .plist files get the >-root inverse seds;
// then DEBIAN control scripts get the space-anchored inverse seds.
func patchPayloadForRootful(dir string) error {
	resolver := buildRpathResolver(dir)
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
			return patchMachOForRootful(path, rel, resolver)
		}
		if strings.HasSuffix(strings.ToLower(filepath.Base(rel)), ".plist") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out := applySeds(data, rootfulPlistSeds)
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
			out := applySeds(data, rootfulScriptSeds)
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

// buildRpathResolver returns a function mapping a dylib basename to its
// package-absolute path ("/" + package-relative), for restoring @rpath/...
// install names and dependencies the forward converters created. Files under
// DEBIAN are excluded; the first file with a given basename wins.
func buildRpathResolver(dir string) func(string) string {
	m := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || strings.HasPrefix(rel, "DEBIAN") {
			return nil
		}
		base := filepath.Base(rel)
		if _, ok := m[base]; !ok {
			m[base] = "/" + filepath.ToSlash(rel)
		}
		return nil
	})
	return func(base string) string {
		return m[base]
	}
}

// rootfulDependency maps one load-command path back to its rootful form.
// Returns ok=false (no change) when the path needs no conversion or its
// original is unrecoverable.
func rootfulDependency(dep string, resolver func(string) string) (string, bool) {
	switch {
	case strings.HasPrefix(dep, "/var/jb/"):
		return "/" + strings.TrimPrefix(dep, "/var/jb/"), true
	case dep == "@rpath/libsubstrate.dylib":
		// The ellekit substrate shim the forward converters emit; rootful
		// links the classic CydiaSubstrate framework path.
		return "/Library/Frameworks/CydiaSubstrate.framework/CydiaSubstrate", true
	case strings.HasPrefix(dep, "@rpath/"):
		base := strings.TrimPrefix(dep, "@rpath/")
		if p := resolver(base); p != "" {
			return p, true
		}
		log.Warnf("couldn't recover rootful path for %s (no %s in package); leaving as-is", dep, base)
		return "", false
	case strings.HasPrefix(dep, "/var/LIY/"):
		return "/Library/" + strings.TrimPrefix(dep, "/var/LIY/"), true
	case strings.HasPrefix(dep, "/var/lib/"):
		return "/usr/lib/" + strings.TrimPrefix(dep, "/var/lib/"), true
	case strings.HasPrefix(dep, "/var/bin/"):
		return "/usr/bin/" + strings.TrimPrefix(dep, "/var/bin/"), true
	case strings.HasPrefix(dep, "/var/sh"):
		return "/bin/sh", true
	}
	return "", false
}

// rootfulString maps one __cstring dlopen string back to rootful form,
// returning ok=false when unchanged (used by RewriteCStrings; the mappings
// never grow a string, so rewriting stays in place).
func rootfulString(s string) (string, bool) {
	switch {
	case strings.HasPrefix(s, "/var/jb/"):
		return "/" + strings.TrimPrefix(s, "/var/jb/"), true
	case strings.HasPrefix(s, "/var/LIY/"):
		return "/Library/" + strings.TrimPrefix(s, "/var/LIY/"), true
	case strings.HasPrefix(s, "/var/lib/"):
		return "/usr/lib/" + strings.TrimPrefix(s, "/var/lib/"), true
	case strings.HasPrefix(s, "/var/bin/"):
		return "/usr/bin/" + strings.TrimPrefix(s, "/var/bin/"), true
	case strings.HasPrefix(s, "/var/sh"):
		return "/bin/sh", true
	}
	return "", false
}

// patchMachOForRootful reverses one Mach-O: load-command deps and install
// name restored via rootfulDependency, /var/jb rpaths removed, string-table
// paths restored via the NUL-anchored inverse byte-seds, __cstring paths via
// rootfulString, then re-signed with the preserved entitlements.
func patchMachOForRootful(path, rel string, resolver func(string) string) error {
	b := macho.Bin{Path: path}
	var origEnts []byte
	if ents, err := b.ExtractEntitlements(); err == nil {
		origEnts = ents
	}
	if err := b.RemoveSignature(); err != nil {
		return fmt.Errorf("removing signature from %s: %w", rel, err)
	}

	deps, err := b.AllDependencies()
	if err != nil {
		return fmt.Errorf("reading dependencies of %s: %w", rel, err)
	}
	for _, dep := range deps {
		converted, ok := rootfulDependency(dep, resolver)
		if !ok || converted == dep {
			continue
		}
		log.Infof("rewriting %s: %s -> %s", rel, dep, converted)
		if err := b.ChangeDependency(dep, converted); err != nil {
			return fmt.Errorf("rewriting dependency of %s: %w", rel, err)
		}
	}
	if id, err := b.InstallName(); err == nil && id != "" {
		if converted, ok := rootfulDependency(id, resolver); ok && converted != id {
			log.Infof("rewriting install name of %s: %s -> %s", rel, id, converted)
			if err := b.SetInstallName(converted); err != nil {
				return fmt.Errorf("rewriting install name of %s: %w", rel, err)
			}
		}
	}
	if rpaths, err := b.Rpaths(); err == nil {
		for _, rp := range rpaths {
			if !strings.HasPrefix(rp, "/var/jb/") {
				continue
			}
			log.Infof("removing rpath %s from %s", rp, rel)
			if err := b.RemoveRpath(rp); err != nil {
				return fmt.Errorf("removing rpath %s from %s: %w", rp, rel, err)
			}
		}
	}

	// String-table paths: the NUL-anchored inverse byte-seds, then the
	// __cstring rewrite (in place — reverse mappings never grow strings).
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out := applySeds(data, rootfulByteSeds)
	if !bytes.Equal(out, data) {
		if err := os.WriteFile(path, out, 0o755); err != nil {
			return err
		}
		log.Infof("applied rootful byte-seds to %s", rel)
	}
	stats, err := macho.RewriteCStrings(path, func(s string) string {
		if r, ok := rootfulString(s); ok {
			return r
		}
		return ""
	})
	if err != nil {
		return fmt.Errorf("rewriting __cstring of %s: %w", rel, err)
	}
	if stats.InPlace > 0 || stats.Relocated > 0 {
		log.Infof("rewrote %d __cstring string(s) of %s (%d in place, %d relocated)",
			stats.InPlace+stats.Relocated, rel, stats.InPlace, stats.Relocated)
	}
	if err := b.SignWithEntitlements(origEnts); err != nil {
		return fmt.Errorf("signing %s: %w", rel, err)
	}
	log.Infof("patched Mach-O %s (rootful)", rel)
	return nil
}
