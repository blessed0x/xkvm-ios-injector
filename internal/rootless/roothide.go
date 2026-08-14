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
//     matching xkvm's rootless converter contract)
//  4. scripts + plists — the same sed path translations upstream applies
//     (preinst/prerm/postinst/postrm/extrainst_, LaunchDaemons and libSandy
//     plists)
//  5. fixed-paths-warning — remaining /var/jb strings in converted Mach-O
//     __cstring sections are reported, exactly like upstream's scan
//
// --pkgmirror mirrors the package to var/mobile/Library/pkgmirror with the
// control dir renamed DEBIAN.<pkg> for roothide's package manager. mode
// ("", "auto", "dynamic") controls the Pre-Depends/version edits: "" adds
// none (upstream's default without a mode argument), "auto" adds
// rootless-compat(>= 0.9), "dynamic" adds a ~roothide version suffix and a
// patches-<pkg>(= <ver>~roothide) Pre-Depends.

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
	if err := editControlRoothide(tmpdir, mode); err != nil {
		return err
	}
	if pkgmirror {
		if err := makePkgMirror(tmpdir); err != nil {
			return err
		}
	}
	if err := patchPayloadForRoothide(tmpdir); err != nil {
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
		jbEntries, err := os.ReadDir(jb)
		if err != nil {
			return err
		}
		for _, e := range jbEntries {
			if err := os.Rename(filepath.Join(jb, e.Name()), filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Remove(jb); err != nil {
			return err
		}
		// Best-effort removal of the emptied var/ (upstream rmdir with
		// `|| true`).
		_ = os.Remove(filepath.Join(dir, "var"))
		log.Infof("hoisted var/jb payload to package root")
	} else if err != nil && !os.IsNotExist(err) {
		return err
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
// DEBIAN and the pkgmirror copy are skipped (mirrored, not installed).
func patchPayloadForRoothide(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
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
			return patchMachOForRoothide(path, rel)
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
		if d.IsDir() {
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
