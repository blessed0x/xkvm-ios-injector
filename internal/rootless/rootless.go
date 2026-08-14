// Package rootless converts rootful jailbreak packages (.deb) to rootless,
// porting the rootless-patcher (Nightwind, MIT) pipeline faithfully:
//
//  1. repack — the payload is moved under var/jb/ (DEBIAN/ stays at the top)
//  2. control — Architecture → iphoneos-arm64, Depends gains the rootless
//     runtime alternation, an Icon path is converted
//  3. Mach-O — load-command dylib paths and install names whose first path
//     component is a jailbreak bootstrap root are rewritten under /var/jb,
//     honoring the ConversionRuleset blacklist and special cases; code
//     signatures are removed first (rootless installs re-sign or skip)
//
// Scope boundary (documented in ARCHITECTURE.md): upstream also patches
// CFStrings and __data string pointers (ADRP/ADD instruction rewriting,
// chained-fixup aware). xkvm converts the load-command layer only, which is
// where a rootful tweak's dylib dependencies live; runtime dlopen strings
// compiled into __TEXT are not rewritten.
package rootless

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
)

// RuntimeDep is the dependency alternation upstream appends to Depends,
// selecting whichever rootless runtime the device runs (Procursus's
// arm64v8, or the old ABI shims).
const RuntimeDep = "cy+cpu.arm64v8 | oldabi-xina | oldabi"

// bootstrapRoots are the top-level payload paths moved under /var/jb,
// mirroring RPConversionHandler's bootstrapList.
var bootstrapRoots = []string{
	"Applications", "bin", "boot", "etc", "lib", "Library",
	"mnt", "sbin", "tmp", "User", "usr", "var",
}

// specialCases mirror RPConversionHandler's SpecialCases: replacements
// applied before the /var/jb prefix (upstream's order, quirks included —
// e.g. "private/var/" short-circuits the blacklist because it is checked
// first).
var specialCases = map[string]string{
	"private/var/": "var/",
	"var/tmp":      "tmp",
	"~":            "~",
}

// blacklist mirrors rootless-patcher's ConversionRuleset.json Blacklist:
// strings that must never be rewritten (system paths, Apple prefs, the
// jailbreak root itself).
//
// Deviation: upstream scans __cstring/__data strings, where Apple's /usr/lib
// libraries never appear (they only exist in load commands), so its list
// omits them. xkvm converts the load-command layer, where libSystem etc.
// DO appear — the Apple /usr/lib families below are added so those deps are
// never rewritten (a corrupted libc dependency would break the whole tweak;
// a not-converted jailbreak lib merely keeps a rootful path, detectable by
// `xkvm check`).
var blacklist = []string{
	"/System",
	"/usr/lib/libSystem",
	"/usr/lib/libobjc",
	"/usr/lib/libc++",
	"/usr/lib/libstdc++",
	"/usr/lib/libcompression",
	"/usr/lib/libcups",
	"/usr/lib/libxml",
	"/usr/lib/libresolv",
	"/usr/lib/libicucore",
	"/usr/lib/libdispatch",
	"/usr/lib/libm",
	"/usr/lib/libssl",
	"/usr/lib/libcrypto",
	"/usr/lib/libnetwork",
	"/usr/lib/libxpc",
	"/usr/lib/libunwind",
	"/usr/lib/libutil",
	"/usr/lib/libexpat",
	"/usr/lib/libiconv",
	"/usr/lib/libcharset",
	"/usr/lib/libbz2",
	"/usr/lib/libpthread",
	"/usr/lib/libcommonCrypto",
	"/usr/lib/libcorecrypto",
	"/usr/lib/libmis",
	"/usr/lib/dyld",
	"/usr/lib/libMobileGestalt.dylib",
	"/usr/lib/system",
	"/usr/lib/swift",
	"/usr/lib/libswift",
	"/usr/lib/libsql",
	"/usr/lib/libz",
	"/var/jb",
	"/var/mobile/Library/Preferences/com.apple.",
	"/var/mobile/Library/Preferences/systemgroup.com.apple.",
	"/var/mobile/Library/Preferences/group.com.apple.",
	"/var/mobile/Library/Preferences/.GlobalPreferences.plist",
	"/var/mobile/Library/Preferences/.GlobalPreferences_m.plist",
	"/var/mobile/Library/Preferences/bluetoothaudiod.plist",
	"/var/mobile/Library/Preferences/NetworkInterfaces.plist",
	"/var/mobile/Library/Preferences/OSThermalStatus.plist",
	"/var/mobile/Library/Preferences/preferences.plist",
	"/var/mobile/Library/Preferences/osanalyticshelper.plist",
	"/var/mobile/Library/Preferences/UserEventAgent.plist",
	"/var/mobile/Library/Preferences/wifid.plist",
	"/var/mobile/Library/Preferences/dprivacyd.plist",
	"/var/mobile/Library/Preferences/silhouette.plist",
	"/var/mobile/Library/Preferences/nfcd.plist",
	"/var/mobile/Library/Preferences/kNPProgressTrackerDomain.plist",
	"/var/mobile/Library/Preferences/siriknowledged.plist",
	"/var/mobile/Library/Preferences/UITextInputContextIdentifiers.plist",
	"/var/mobile/Library/Preferences/mobile_storage_proxy.plist",
	"/var/mobile/Library/Preferences/splashboardd.plist",
	"/var/mobile/Library/Preferences/mobile_installation_proxy.plist",
	"/var/mobile/Library/Preferences/languageassetd.plist",
	"/var/mobile/Library/Preferences/ptpcamerad.plist",
	"/var/mobile/Library/Preferences/com.google.gmp.measurement.monitor.plist",
	"/var/mobile/Library/Preferences/com.google.gmp.measurement.plist",
	"/var/mobile/Library/SpringBoard",
	"/var/mobile/Library/WebClips",
	"/Library/Wallpaper",
	"/Library/Application Support/AggregateDictionary",
	"/var/mobile/Library/Caches/com.apple.",
	"/var/checkra1n.dmg",
	"/private/etc/apt/undecimus",
	"/var/Liy/.procursus_strapped",
	"/Library/Ringtones",
	"://",
}

// ShouldConvert reports whether a path string should be rewritten under
// /var/jb, porting RPConversionHandler's shouldConvertString: the first path
// component must be a bootstrap root, special cases and file:// URLs force
// conversion, and blacklisted substrings veto it (checked last).
func ShouldConvert(s string) bool {
	if s == "" {
		return false
	}
	first := firstPathComponent(s)
	if !contains(bootstrapRoots, first) {
		return false
	}
	for sc := range specialCases {
		if strings.Contains(s, sc) {
			return true
		}
	}
	if strings.HasPrefix(s, "file://") {
		return true
	}
	for _, b := range blacklist {
		if strings.Contains(s, b) {
			return false
		}
	}
	return true
}

// ConvertString rewrites a path string under /var/jb, porting
// RPConversionHandler's convertedStringForString: special-case replacements
// first, then file:// and absolute/relative prefixing.
func ConvertString(s string) string {
	out := s
	for sc, rep := range specialCases {
		if strings.Contains(s, sc) {
			out = strings.ReplaceAll(out, sc, rep)
		}
	}
	switch {
	case strings.HasPrefix(s, "file://"):
		return "file:///var/jb" + strings.TrimPrefix(s, "file://")
	case strings.HasPrefix(s, "/"):
		return "/var/jb" + out
	default:
		return "var/jb/" + out
	}
}

// firstPathComponent returns the first non-empty path component of s after
// any "file://" prefix, mirroring the pathComponents walk upstream (leading
// "/" components are skipped). Returns "" for single-component strings,
// matching upstream's [pathComponents count] == 1 rejection (a bare "usr" or
// "var" without a slash never converts).
func firstPathComponent(s string) string {
	s = strings.TrimPrefix(s, "file://")
	if !strings.Contains(s, "/") {
		return "" // single component (no slash): upstream requires >= 2
	}
	s = strings.TrimPrefix(s, "/")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Convert converts a rootful deb at input to a rootless deb at output. thin
// additionally thins every Mach-O to arm64 (best-effort: binaries that
// cannot thin are skipped with a warning rather than aborting the package).
// Inputs already rootless (payload under var/jb) are rebuilt unchanged.
func Convert(input, output string, thin bool) error {
	tmpdir, err := os.MkdirTemp("", "xkvm-rootless-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log.Infof("unpacking %s..", input)
	if err := deb.Unpack(input, tmpdir); err != nil {
		return err
	}

	if err := repackRootless(tmpdir); err != nil {
		return err
	}
	if err := editControl(tmpdir); err != nil {
		return err
	}
	converted, err := convertMachOs(filepath.Join(tmpdir, "var", "jb"), thin)
	if err != nil {
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
	log.Infof("rootless conversion: %d Mach-O load-command path(s) rewritten", converted)
	log.Infof("wrote rootless deb at %s", output)
	return nil
}

// repackRootless moves the payload under var/jb (DEBIAN/ stays at the top),
// mirroring RPRepackHandler. A payload already under var/jb is left as-is —
// upstream's "already rootless, skipping" case.
func repackRootless(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "var", "jb")); err == nil {
		log.Infof("package is already rootless; skipping repack")
		return nil
	}
	// Read the listing BEFORE creating var/jb: the fresh directory must not
	// be reprocessed by the move loop (it would try to move var/jb into
	// itself).
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	jb := filepath.Join(dir, "var", "jb")
	if err := os.MkdirAll(jb, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "DEBIAN" {
			continue
		}
		src := filepath.Join(dir, e.Name())
		if e.Name() == "var" {
			// A rootful package shipping /var content: move its children
			// into var/jb (upstream's naive whole-dir move would fail by
			// moving var into its own subdirectory).
			children, err := os.ReadDir(src)
			if err != nil {
				return err
			}
			for _, c := range children {
				if err := os.Rename(filepath.Join(src, c.Name()), filepath.Join(jb, c.Name())); err != nil {
					return err
				}
			}
			continue
		}
		if err := os.Rename(src, filepath.Join(jb, e.Name())); err != nil {
			return err
		}
	}
	log.Infof("payload repacked under var/jb")
	return nil
}

// editControl applies the rootless control edits: Architecture →
// iphoneos-arm64, Depends gains RuntimeDep (when not already present), and
// an Icon path is converted under /var/jb. Faithful to RPControlHandler
// except upstream's Depends append is buggy (it crashes on a plain-string
// Depends and writes garbage arrays) — xkvm appends the alternation
// correctly in all cases.
func editControl(dir string) error {
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
	found := map[string]bool{}
	for i := range entries {
		found[entries[i].key] = true
	}
	edits := 0
	for i := range entries {
		switch entries[i].key {
		case "Architecture":
			if entries[i].value != "iphoneos-arm64" {
				entries[i].value = "iphoneos-arm64"
				edits++
			}
		case "Depends":
			if !containsDep(entries[i].value, RuntimeDep) {
				if entries[i].value == "" {
					entries[i].value = RuntimeDep
				} else {
					entries[i].value += ", " + RuntimeDep
				}
				edits++
			}
		case "Icon":
			if ShouldConvert(entries[i].value) {
				converted := ConvertString(entries[i].value)
				log.Infof("converting icon path %s -> %s", entries[i].value, converted)
				entries[i].value = converted
				edits++
			}
		}
	}
	if !found["Architecture"] {
		entries = append(entries, controlEntry{key: "Architecture", value: "iphoneos-arm64"})
		edits++
	}
	if !found["Depends"] {
		entries = append(entries, controlEntry{key: "Depends", value: RuntimeDep})
		edits++
	}
	if err := os.WriteFile(path, []byte(renderControl(entries)), 0o644); err != nil {
		return err
	}
	if edits > 0 {
		log.Infof("control edited (%d field(s))", edits)
	}
	return nil
}

func containsDep(depends, want string) bool {
	for _, d := range strings.Split(depends, ",") {
		if strings.TrimSpace(d) == want {
			return true
		}
	}
	return false
}

type controlEntry struct {
	key   string
	value string
}

// parseControl parses dpkg control syntax: "Key: value" lines, with
// continuation lines (leading space/tab) folded into the previous value.
func parseControl(data string) []controlEntry {
	var entries []controlEntry
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(entries) > 0 {
				entries[len(entries)-1].value += "\n" + line
			}
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		entries = append(entries, controlEntry{key: key, value: value})
	}
	return entries
}

func renderControl(entries []controlEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s: %s\n", e.key, e.value)
	}
	return b.String()
}

// convertMachOs walks payload (var/jb) and rewrites every Mach-O's
// load-command dylib paths and install name under /var/jb per ShouldConvert.
// Signatures are removed before editing (upstream RPCodesignHandler) — the
// jailbreak's install path re-signs or tolerates unsigned binaries. Returns
// the number of rewritten paths.
func convertMachOs(payload string, thin bool) (int, error) {
	converted := 0
	err := filepath.WalkDir(payload, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		is, err := macho.IsMachO(path)
		if err != nil || !is {
			return nil
		}
		rel, _ := filepath.Rel(payload, path)
		b := macho.Bin{Path: path}

		if thin {
			if err := b.ThintoArm64(); err != nil {
				// Best-effort: a deb with a non-arm64 slice (rare for
				// jailbreak tweaks) must not abort the whole conversion.
				log.Warnf("couldn't thin %s: %v", rel, err)
			}
		}

		if err := b.RemoveSignature(); err != nil {
			return fmt.Errorf("removing signature from %s: %w", rel, err)
		}
		deps, err := b.Dependencies()
		if err != nil {
			return fmt.Errorf("reading dependencies of %s: %w", rel, err)
		}
		for _, dep := range deps {
			if !ShouldConvert(dep) {
				continue
			}
			convertedPath := ConvertString(dep)
			log.Infof("rewriting %s: %s -> %s", rel, dep, convertedPath)
			if err := b.ChangeDependency(dep, convertedPath); err != nil {
				return fmt.Errorf("rewriting dependency of %s: %w", rel, err)
			}
			converted++
		}
		if id, err := b.InstallName(); err == nil && id != "" && ShouldConvert(id) {
			convertedID := ConvertString(id)
			log.Infof("rewriting install name of %s: %s -> %s", rel, id, convertedID)
			if err := b.SetInstallName(convertedID); err != nil {
				return fmt.Errorf("rewriting install name of %s: %w", rel, err)
			}
			converted++
		}

		// __TEXT.__cstring dlopen strings: runtime paths compiled into the
		// binary (the old documented boundary). Strings that grow under
		// /var/jb are relocated into a __PATCH_ROOTLESS,__cstring segment
		// with references retargeted (see internal/macho/cstring.go).
		stats, err := macho.RewriteCStrings(path, func(s string) string {
			if ShouldConvert(s) {
				return ConvertString(s)
			}
			return ""
		})
		if err != nil {
			return fmt.Errorf("rewriting __cstring of %s: %w", rel, err)
		}
		if stats.InPlace > 0 || stats.Relocated > 0 {
			log.Infof("rewrote %d __cstring string(s) of %s (%d in place, %d relocated)",
				stats.InPlace+stats.Relocated, rel, stats.InPlace, stats.Relocated)
			converted += stats.InPlace + stats.Relocated
		}
		return nil
	})
	return converted, err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
