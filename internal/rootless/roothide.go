package rootless

// Roothide conversion (rootless deb -> roothide-jailbreak deb), a faithful
// semantics port of roothide/RootHidePatcher's patch.sh main path (GPL-3.0 —
// format/semantics reference only, no code copied; see NOTICE):
//
//  1. hoist — var/jb/* is moved to the package root; anything else in the
//     payload goes under rootfs/
//  2. control — Architecture → iphoneos-arm64e, Conflicts mangles
//     "roothide", and Pre-Depends/version-suffix edits per mode
//  3. Mach-O — every /var/jb/... load-command dependency and LC_RPATH is
//     rewritten to @loader_path/.jbroot/... (the roothide bootstrap lives
//     inside each app's container at .jbroot, so the jailbreak is invisible
//     to the app); signatures are removed (documented deviation — upstream
//     ldid-signs; the roothide install path signs or tolerates unsigned,
//     matching xkvm's rootless converter contract). In --mode auto, every
//     patched Mach-O also gains a sibling .roothidepatch symlink to
//     /usr/lib/DynamicPatches/AutoPatches.dylib (upstream's AutoPatches
//     mechanism); --mode dynamic creates none.
//  4. scripts + plists — the same sed path translations upstream applies
//     (preinst/prerm/postinst/postrm/extrainst_, LaunchDaemons and libSandy
//     plists)
//  5. fixed-paths-warning — remaining /var/jb strings in converted Mach-O
//     __cstring sections are reported, exactly like upstream's scan
//
// --pkgmirror mirrors the package to var/mobile/Library/pkgmirror with the
// control dir renamed DEBIAN.<pkg> for roothide's package manager (plus any
// DEBIAN/*.roothidepatch files the input ships, copied best-effort like
// upstream's `cp ... || true`; the mirror never contains payload
// .roothidepatch symlinks — it is a pre-patch snapshot). mode ("", "auto",
// "dynamic") controls the Pre-Depends/version edits: "" adds none
// (upstream's default without a mode argument), "auto" adds
// rootless-compat(>= 0.9) plus the .roothidepatch symlinks above, "dynamic"
// adds a ~roothide version suffix and a patches-<pkg>(= <ver>~roothide)
// Pre-Depends.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
)

// RoothideOutputArch is the architecture roothide packages are re-arch'd to.
const RoothideOutputArch = "iphoneos-arm64e"

// autoPatchesDylib is the DynamicPatches loader that roothide's AutoPatches
// mechanism points at: in --mode auto every patched payload Mach-O gains a
// sibling <file>.roothidepatch symlink to it, and the roothide runtime
// applies the auto-patch treatment to any binary carrying such a sibling
// (upstream patch.sh line 289; target verified against the reference).
const autoPatchesDylib = "/usr/lib/DynamicPatches/AutoPatches.dylib"

// ConvertToRoothide converts a rootless deb at input to a roothide deb at
// output.
func ConvertToRoothide(input, output string, pkgmirror bool, mode string) error {
	tmpdir, err := os.MkdirTemp("", "xkvm-roothide-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log.Infof("unpacking %s..", input)
	if err := deb.Unpack(input, tmpdir); err != nil {
		return err
	}

	if err := hoistRootless(tmpdir); err != nil {
		return err
	}
	// The pkgmirror is a snapshot of the hoisted-but-unmodified package,
	// taken BEFORE the control edits and the Mach-O patching — exactly
	// where upstream's patch.sh copies it ($3 block, before the control
	// seds). The mirror's DEBIAN.<pkg>/control therefore keeps the input
	// package's original fields, and the mirrored payload keeps its
	// original /var/jb load commands (patchPayloadForRoothide skips the
	// mirror). This is the reference tool's observable output; the
	// installable artifact is the real package, not the mirror.
	if pkgmirror {
		if err := makePkgMirror(tmpdir); err != nil {
			return err
		}
	}
	if err := editControlRoothide(tmpdir, mode); err != nil {
		return err
	}
	if err := patchPayloadForRoothide(tmpdir, mode); err != nil {
		return err
	}
	if err := warnRoothideFixedPaths(tmpdir); err != nil {
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
	log.Infof("wrote roothide deb at %s", output)
	return nil
}

// hoistRootless moves var/jb/* to the package root and sends anything else
// under rootfs/, mirroring upstream's layout handling: on the roothide
// jailbreak the real system root is exposed at /rootfs and the jbroot
// content is merged at the top level. The loose set (top-level entries other
// than DEBIAN and var/) is captured BEFORE the hoist so the hoisted jbroot
// content is not mistaken for loose system files.
//
// var/ is special: it may hold the jbroot (var/jb) AND system content at the
// same time — upstream's comment notes packages with both /var/jb/var/xxx
// and /var/xxx. Upstream hoists into an EMPTY staging root (mv into the
// fresh NEW dir), so the jbroot copy wins at the package root while the
// system copy joins rootfs/. The single-dir equivalent: move the system
// var children to rootfs/var/ first (freeing the root var shell), then
// scratch-rename the jbroot out of the shell and hoist it into the now-empty
// root — no rename can collide. A var/ without jb (pure system content)
// joins rootfs/, and an empty var/ is dropped (upstream `rmdir ... || true`).
func hoistRootless(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var loose []string
	for _, e := range entries {
		if e.Name() == "DEBIAN" || e.Name() == "var" {
			continue
		}
		loose = append(loose, e.Name())
	}

	jb := filepath.Join(dir, "var", "jb")
	if _, err := os.Stat(jb); err == nil {
		// 1. System var children (anything in var/ other than jb) move to
		//    rootfs/var/ first — they are system content, and leaving them in
		//    place would collide with the hoisted jbroot's own var/.
		varEntries, err := os.ReadDir(filepath.Join(dir, "var"))
		if err != nil {
			return err
		}
		var sysVar []string
		for _, e := range varEntries {
			if e.Name() != "jb" {
				sysVar = append(sysVar, e.Name())
			}
		}
		if len(sysVar) > 0 {
			rootfsVar := filepath.Join(dir, "rootfs", "var")
			if err := os.MkdirAll(rootfsVar, 0o755); err != nil {
				return err
			}
			for _, n := range sysVar {
				if err := os.Rename(filepath.Join(dir, "var", n), filepath.Join(rootfsVar, n)); err != nil {
					return err
				}
			}
		}
		// 2. Scratch-rename the jbroot out of the shell, then drop the shell
		//    (empty now). If it somehow isn't empty, it's system content and
		//    joins the rootfs move below (upstream `rmdir ... || true`).
		tmp := filepath.Join(dir, ".xkvm-jbroot-tmp")
		if err := os.Rename(jb, tmp); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, "var")); err != nil {
			loose = append(loose, "var")
		}
		// 3. Hoist into the now-empty root (only DEBIAN + rootfs/ present).
		tmpEntries, err := os.ReadDir(tmp)
		if err != nil {
			return err
		}
		for _, e := range tmpEntries {
			if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
		log.Infof("hoisted var/jb payload to package root")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	} else {
		// No var/jb: an empty var/ is dropped; a var/ with system content
		// joins the rootfs move (upstream rmdir fails -> rootfs).
		if err := os.Remove(filepath.Join(dir, "var")); err != nil {
			loose = append(loose, "var")
		}
	}

	if len(loose) == 0 {
		return nil
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return err
	}
	for _, name := range loose {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(rootfs, name)); err != nil {
			return err
		}
	}
	log.Infof("moved %d remaining payload entr(ies) under rootfs/", len(loose))
	return nil
}

// editControlRoothide applies the roothide control edits: Architecture →
// iphoneos-arm64e, the Conflicts "roothide" mangle, and the mode-dependent
// Pre-Depends/version edits.
func editControlRoothide(dir, mode string) error {
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
	pkg := ""
	for _, e := range entries {
		if e.key == "Package" {
			pkg = e.value
			break
		}
	}
	edits := 0
	preDepends := ""
	for i := range entries {
		switch entries[i].key {
		case "Architecture":
			if entries[i].value != RoothideOutputArch {
				entries[i].value = RoothideOutputArch
				edits++
			}
		case "Version":
			if mode == "dynamic" && !strings.HasSuffix(entries[i].value, "~roothide") {
				entries[i].value += "~roothide"
				edits++
			}
		case "Conflicts":
			if strings.Contains(entries[i].value, "roothide") {
				entries[i].value = strings.ReplaceAll(entries[i].value, "roothide", "r-o-o-t-l-e-s-s-")
				edits++
			}
		case "Pre-Depends":
			preDepends = entries[i].value
		}
	}
	switch mode {
	case "auto":
		preDepends = joinPreDepends(preDepends, "rootless-compat(>= 0.9)")
	case "dynamic":
		preDepends = joinPreDepends(preDepends, fmt.Sprintf("patches-%s(= %s)", pkg, versionValue(entries, "~roothide")))
	}
	if preDepends != "" && mode != "" {
		found := false
		for i := range entries {
			if entries[i].key == "Pre-Depends" {
				entries[i].value = preDepends
				found = true
			}
		}
		if !found {
			entries = append(entries, controlEntry{key: "Pre-Depends", value: preDepends})
		}
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

func joinPreDepends(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + ", " + add
}

func versionValue(entries []controlEntry, suffix string) string {
	for _, e := range entries {
		if e.key == "Version" {
			return e.value
		}
	}
	return "" + suffix
}

// makePkgMirror copies the (post-hoist) package to
// var/mobile/Library/pkgmirror with the control dir renamed
// DEBIAN.<pkg>, for roothide's package-manager integration.
func makePkgMirror(dir string) error {
	pkg := ""
	if data, err := os.ReadFile(filepath.Join(dir, "DEBIAN", "control")); err == nil {
		for _, e := range parseControl(string(data)) {
			if e.key == "Package" {
				pkg = e.value
				break
			}
		}
	}
	if pkg == "" {
		return fmt.Errorf("cannot build pkgmirror: no Package field in control")
	}
	mirror := filepath.Join(dir, "var", "mobile", "Library", "pkgmirror")
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "var" {
			continue // the mirror destination itself
		}
		if err := copyTree(filepath.Join(dir, e.Name()), filepath.Join(mirror, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(mirror, "DEBIAN"), filepath.Join(mirror, "DEBIAN."+pkg)); err != nil {
		return err
	}
	// Upstream copies any DEBIAN/*.roothidepatch files the input package
	// ships into the mirror's control dir (patch.sh line 352, `|| true` —
	// best-effort; usually empty).
	if patches, err := filepath.Glob(filepath.Join(dir, "DEBIAN", "*.roothidepatch")); err == nil {
		for _, p := range patches {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(mirror, "DEBIAN."+pkg, filepath.Base(p)), data, 0o755); err != nil {
				return err
			}
			log.Infof("copied %s into pkgmirror", filepath.Base(p))
		}
	}
	// Upstream chmods the whole mirror 0755 (mobile-owned, readable and
	// writable by the package manager); ownership is zeroed by the deb
	// builder, so world-readable 0755 is the faithful equivalent.
	if err := filepath.WalkDir(mirror, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, 0o755)
	}); err != nil {
		return err
	}
	log.Infof("mirrored package to var/mobile/Library/pkgmirror (DEBIAN.%s)", pkg)
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// patchPayloadForRoothide walks the package and applies the Mach-O /var/jb →
// @loader_path/.jbroot rewrites plus the script/plist path translations.
// DEBIAN and the pkgmirror copy are skipped (mirrored, not installed). In
// --mode auto, every patched Mach-O also gains a sibling <file>.roothidepatch
// symlink to /usr/lib/DynamicPatches/AutoPatches.dylib — upstream's
// AutoPatches mechanism (unconditional per Mach-O, exactly like the
// reference's ln -s).
func patchPayloadForRoothide(dir, mode string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip dirs and symlinks: the .roothidepatch siblings point at a
		// device-only path (/usr/lib/DynamicPatches/AutoPatches.dylib) that
		// must never be followed during conversion.
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if strings.HasPrefix(rel, "DEBIAN") || strings.Contains(rel, "var/mobile/Library/pkgmirror") {
			return nil
		}
		is, err := macho.IsMachO(path)
		if err != nil {
			return err
		}
		if is {
			if err := patchMachOForRoothide(path, rel); err != nil {
				return err
			}
			if mode == "auto" {
				link := path + ".roothidepatch"
				if err := os.Symlink(autoPatchesDylib, link); err != nil {
					return fmt.Errorf("creating .roothidepatch symlink for %s: %w", rel, err)
				}
				log.Infof("added .roothidepatch symlink for %s", rel)
			}
			return nil
		}
		switch {
		case isScriptName(filepath.Base(rel)):
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out := patchScriptPaths(data)
			if string(out) != string(data) {
				if err := os.WriteFile(path, out, 0o755); err != nil {
					return err
				}
				log.Infof("patched script %s", rel)
			}
		case strings.HasSuffix(rel, ".plist"):
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			switch dirn := filepath.Dir(rel); {
			case strings.Contains(dirn, "/Library/LaunchDaemons"):
				out := strings.ReplaceAll(string(data), "/var/jb/", "/")
				if out != string(data) {
					if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
						return err
					}
					log.Infof("patched LaunchDaemons plist %s", rel)
				}
			case strings.Contains(dirn, "/Library/libSandy"):
				out := patchSandyPlist(data)
				if string(out) != string(data) {
					if err := os.WriteFile(path, out, 0o644); err != nil {
						return err
					}
					log.Infof("patched libSandy plist %s", rel)
				}
			}
		}
		return nil
	})
}

func isScriptName(base string) bool {
	switch base {
	case "preinst", "prerm", "postinst", "postrm", "extrainst_":
		return true
	}
	return false
}

// patchMachOForRoothide rewrites /var/jb/... dependencies and rpaths to
// @loader_path/.jbroot/... and removes the signature.
func patchMachOForRoothide(path, rel string) error {
	b := macho.Bin{Path: path}
	deps, err := b.AllDependencies()
	if err != nil {
		return fmt.Errorf("reading dependencies of %s: %w", rel, err)
	}
	patched := 0
	for _, dep := range deps {
		repl, ok := jbrootReplace(dep)
		if !ok {
			continue
		}
		log.Infof("rewriting %s: %s -> %s", rel, dep, repl)
		if err := b.ChangeDependency(dep, repl); err != nil {
			return fmt.Errorf("rewriting dependency of %s: %w", rel, err)
		}
		patched++
	}
	rpaths, err := b.Rpaths()
	if err != nil {
		return fmt.Errorf("reading rpaths of %s: %w", rel, err)
	}
	for _, rp := range rpaths {
		repl, ok := jbrootReplace(rp)
		if !ok {
			continue
		}
		log.Infof("rewriting rpath of %s: %s -> %s", rel, rp, repl)
		if err := b.ReplaceRpath(rp, repl); err != nil {
			return fmt.Errorf("rewriting rpath of %s: %w", rel, err)
		}
		patched++
	}
	if err := b.RemoveSignature(); err != nil {
		return fmt.Errorf("removing signature from %s: %w", rel, err)
	}
	log.Infof("patched Mach-O %s (%d path(s))", rel, patched)
	return nil
}

// jbrootReplace maps a /var/jb/... path onto @loader_path/.jbroot/...,
// reporting whether the input was a /var/jb path at all.
func jbrootReplace(s string) (string, bool) {
	if !strings.HasPrefix(s, "/var/jb/") {
		return "", false
	}
	return "@loader_path/.jbroot/" + strings.TrimPrefix(s, "/var/jb/"), true
}

// scriptPathsRe matches the leading-space absolute-root patterns upstream
// rewrites to /rootfs/ in scripts (applied only to unprotected paths).
var scriptPathsRe = regexp.MustCompile(` (Applications|Library|private|System|sbin|bin|etc|lib|usr|var)/`)

var shebangRe = regexp.MustCompile(`(?m)^#!\s*/rootfs/`)

// patchScriptPaths ports the exact sed sequence upstream applies to
// preinst/prerm/postinst/postrm/extrainst_ files (order preserved — the
// /var/jb protect/unprotect dance keeps jailbreak paths away from the
// /rootfs/ rewrites, then restores them).
func patchScriptPaths(data []byte) []byte {
	s := string(data)
	s = strings.ReplaceAll(s, "iphoneos-arm64", "iphoneos-arm64e")
	s = strings.ReplaceAll(s, "/var/jb/", "/-var/jb-/")
	s = strings.ReplaceAll(s, "/var/jb", "/-var/jb-")
	s = scriptPathsRe.ReplaceAllString(s, " /rootfs/$1/")
	s = strings.ReplaceAll(s, `DIR="/Library/`, `DIR="/rootfs/Library/`)
	s = shebangRe.ReplaceAllString(s, "#! /")
	s = strings.ReplaceAll(s, "/-var/jb-/", "/")
	s = strings.ReplaceAll(s, "/-var/jb-", "/var/jb")
	return []byte(s)
}

var sandyPathsRe = regexp.MustCompile(`>(Applications|Library|private|System|sbin|bin|etc|lib|usr|var)/`)

// patchSandyPlist ports upstream's libSandy plist sed sequence (the >-root
// variant of the script dance).
func patchSandyPlist(data []byte) []byte {
	s := string(data)
	s = strings.ReplaceAll(s, "/var/jb/", "/-var/jb-/")
	s = strings.ReplaceAll(s, "/var/jb", "/-var/jb-")
	s = strings.ReplaceAll(s, ">/<", ">/rootfs/<")
	s = sandyPathsRe.ReplaceAllString(s, ">/rootfs/$1/")
	s = strings.ReplaceAll(s, "/-var/jb-/", "/")
	s = strings.ReplaceAll(s, "/-var/jb-", "/var/jb")
	return []byte(s)
}

// warnRoothideFixedPaths scans converted Mach-O __cstring sections for
// surviving /var/jb strings and warns about each (upstream's
// "fixed-paths-warning" — informational; string tables are not rewritten by
// the roothide pass).
func warnRoothideFixedPaths(dir string) error {
	warned := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Symlinks are skipped: the .roothidepatch siblings point at a
		// device-only path that must never be followed.
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if strings.HasPrefix(rel, "DEBIAN") || strings.Contains(rel, "pkgmirror") {
			return nil
		}
		is, err := macho.IsMachO(path)
		if err != nil || !is {
			return err
		}
		strs, err := macho.CStrings(path)
		if err != nil {
			return fmt.Errorf("scanning __cstring of %s: %w", rel, err)
		}
		for _, s := range strs {
			if strings.Contains(s.Value, "/var/jb") {
				log.Warnf("fixed-paths-warning: %s still contains %s (string tables not rewritten)", rel, s.Value)
				warned++
			}
		}
		return nil
	})
	if warned > 0 {
		log.Warnf("fixed-paths: %d /var/jb string(s) survive — verify against the roothide runtime", warned)
	}
	return err
}
