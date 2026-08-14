package rootless

// Roothide conversion (rootless deb -> roothide-jailbreak deb), a faithful
// semantics port of roothide/RootHidePatcher's patch.sh main path (GPL-3.0 —
// format/semantics reference only, no code copied; see NOTICE):
//
//  1. hoist — var/jb/* is moved to the package root; anything else in the
//     payload goes under rootfs/
//  2. control — the input must be iphoneos-arm64 (upstream refuses
//     anything else, exit 1); Architecture → iphoneos-arm64e, Conflicts
//     mangles "roothide", and Pre-Depends/version-suffix edits per mode.
//     The parse/render round-trip strips blank lines (upstream's `sed -i
//     '/^$/d'`); unlike upstream's whole-file `s|iphoneos-arm|...|` sed,
//     only the Architecture field is rewritten (a whole-file sed would
//     corrupt Description text mentioning the arch)
//  3. Mach-O — every /var/jb/... load-command dependency and LC_RPATH is
//     rewritten to @loader_path/.jbroot/... (the roothide bootstrap lives
//     inside each app's container at .jbroot, so the jailbreak is invisible
//     to the app); the result is re-signed like upstream's ldid step —
//     executables get the roothide platform entitlements merged with any
//     they already carried, other Mach-Os get a plain ad-hoc signature.
//     (Upstream replaces entitlements wholesale and its `-M` executable
//     path only re-signs binaries that were already signed; the merge and
//     unconditional sign are deliberate strict-superset improvements.) In
//     --mode auto, every patched Mach-O also gains a sibling .roothidepatch
//     symlink to /usr/lib/DynamicPatches/AutoPatches.dylib (upstream's
//     AutoPatches mechanism); --mode dynamic creates none.
//  4. scripts + plists — the same sed path translations upstream applies,
//     walking the whole package INCLUDING DEBIAN/ (upstream mv's DEBIAN
//     into the walked root): preinst/prerm/postinst/postrm/extrainst_
//     scripts get the /rootfs/ dance, every .plist is converted to XML1
//     first (plutil -convert xml1 — the XML-syntax >-root patterns only
//     match text), then LaunchDaemons and libSandy plists get their
//     /var/jb and /rootfs/ rewrites
//  5. fixed-paths-warning — surviving /var/jb strings are reported across
//     the same walk upstream scans: Mach-O __cstring strings, a load-command
//     audit for missed rewrites, and printable strings in other payload
//     files (.png/.strings excluded, matching upstream's find loop)
//  6. .DS_Store cleanup — every Finder droppings file is deleted before the
//     repack (upstream `find ... -name ".DS_Store" -delete`); output is
//     gzip-compressed (upstream -Zzstd — documented deviation: gzip is
//     universally dpkg-compatible)
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
	"github.com/xkvm/xkvm/internal/plist"
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
	// Input gate before any surgery: upstream refuses anything that isn't
	// a rootless package (`[ $DEB_ARCH != "iphoneos-arm64" ]` → exit 1,
	// which also runs BEFORE the hoist). A rootful or already-roothide deb
	// must fail with a clear message, not a confusing hoist error.
	if err := checkRootlessArch(tmpdir); err != nil {
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
	if err := removeDSStore(tmpdir); err != nil {
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
		// joins the rootfs move (upstream rmdir fails -> rootfs). A package
		// with no var/ at all leaves nothing to move — Remove fails with
		// IsNotExist, which must NOT put a phantom "var" in the loose set
		// (the later rename would ENOENT).
		if err := os.Remove(filepath.Join(dir, "var")); err != nil && !os.IsNotExist(err) {
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
// Pre-Depends/version edits. The parse/render round-trip also strips blank
// lines, matching upstream's `sed -i '/^$/d'` before the arch rewrite
// (patch.sh line 174).
//
// Input gate: upstream refuses anything that isn't a rootless package
// (`[ $DEB_ARCH != "iphoneos-arm64" ]` → exit 1); a rootful or already-
// roothide deb produces a broken hoist, so the same guard errors here with
// a clear message instead of silently converting garbage.
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
	// Upstream's DynamicPatches block MOVES Version to the end of the file
	// (`sed -i "/^Version\:/d"` then `echo "Version: ...~roothide" >>`).
	// Field order is semantically irrelevant to dpkg, but the reference's
	// observable output has Version last, and the member-by-member
	// comparison against upstream surfaced this as the one control
	// divergence in dynamic mode — match it.
	if mode == "dynamic" {
		idx := -1
		v := "~roothide" // upstream's DEB_VERSION is empty when absent
		for i := range entries {
			if entries[i].key == "Version" {
				idx = i
				v = entries[i].value
				break
			}
		}
		if idx >= 0 {
			entries = append(entries[:idx], entries[idx+1:]...)
		}
		entries = append(entries, controlEntry{key: "Version", value: v})
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

// checkRootlessArch validates the input package's Architecture field is
// iphoneos-arm64, matching upstream's refusal of anything else (`[
// $DEB_ARCH != "iphoneos-arm64" ]` → exit 1). No Architecture field also
// refuses (upstream's DEB_ARCH is empty → the same guard fires).
func checkRootlessArch(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "DEBIAN", "control"))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("not a rootless package: no DEBIAN/control to inspect")
		}
		return err
	}
	for _, e := range parseControl(string(data)) {
		if e.key == "Architecture" {
			if e.value != "iphoneos-arm64" {
				return fmt.Errorf("not a rootless package (Architecture: %s) — xkvm roothide converts iphoneos-arm64 debs; run `xkvm rootless` first for rootful inputs", e.value)
			}
			return nil
		}
	}
	return fmt.Errorf("not a rootless package: control has no Architecture field")
}

func joinPreDepends(existing, add string) string {
	if existing == "" {
		return add
	}
	// Upstream prepends the new dep before any existing value
	// (`s/^Pre-Depends\:/Pre-Depends: $PreDepends,/`):
	// "Pre-Depends: patches-<pkg>(= ...), <existing>".
	return add + ", " + existing
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
// DEBIAN/ is walked too — upstream mv's it into the new root, so control
// scripts (preinst/prerm/postinst/postrm/extrainst_) get the same sed dance
// and DEBIAN plists get plutil-converted. Only the pkgmirror copy is skipped
// (mirrored, not installed). In --mode auto, every patched Mach-O also
// gains a sibling <file>.roothidepatch symlink to
// /usr/lib/DynamicPatches/AutoPatches.dylib — upstream's AutoPatches
// mechanism (unconditional per Mach-O, exactly like the reference's ln -s).
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
		if strings.Contains(rel, "var/mobile/Library/pkgmirror") {
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
			// Upstream plutil -convert xml1 on every payload plist (patch.sh
			// line 317), so the XML-syntax sed patterns below (e.g. the
			// libSandy >-root rewrites) match binary plists too; XML plists
			// are normalized. Like upstream under set -e, an unparseable
			// .plist aborts the conversion.
			if err := plist.ConvertToXML1(path); err != nil {
				return fmt.Errorf("plutil-converting %s: %w", rel, err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// dirn has no leading slash (rel is package-relative) while
			// upstream's fpath is /-rooted — match on "/"+dirn so the
			// LaunchDaemons/libSandy dir checks actually fire.
			dirn := "/" + filepath.Dir(rel)
			switch {
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

// roothideEntitlements is the platform-app entitlement base upstream applies
// to converted executables (RootHidePatcher's roothide.entitlements): the
// keys make a bundle executable behave as a platform binary (no sandbox,
// app-bundle/container storage) under roothide's launchd. The keys are
// Apple-defined functional data; see NOTICE for provenance.
const roothideEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>platform-application</key>
	<true/>
	<key>com.apple.private.security.no-sandbox</key>
	<true/>
	<key>com.apple.private.security.storage.AppBundles</key>
	<true/>
	<key>com.apple.private.security.storage.AppDataContainers</key>
	<true/>
</dict>
</plist>`

// signMachOWithPlatformEnts ad-hoc signs a patched Mach-O, matching
// upstream's ldid step in both patch.sh paths (roothide and Derootifier):
// executables are signed with the roothide platform entitlements — merged
// over any the binary already carried, so an existing capability is never
// lost (upstream replaces; the merge is a deliberate improvement) — and
// non-executables are signed plain ad-hoc with no entitlements (upstream's
// `-S`). The pure-Go signature is Apple-format valid
// (internal/macho/der.go), so the host's codesign accepts it — unlike
// ldid's blob on macOS.
//
// origEnts, when non-nil, is the binary's entitlements captured BEFORE the
// caller stripped the signature (the rootless converter removes it before
// editing); when nil the helper reads the still-present signature itself
// (the roothide converter's flow).
func signMachOWithPlatformEnts(path, rel string, origEnts []byte) error {
	b := macho.Bin{Path: path}
	exe, err := b.IsExecutable()
	if err != nil {
		return fmt.Errorf("checking executable type of %s: %w", rel, err)
	}
	var ents []byte
	if exe {
		ents, err = mergedPlatformEntitlements(b, origEnts)
		if err != nil {
			return err
		}
	}
	if err := b.SignWithEntitlements(ents); err != nil {
		return fmt.Errorf("signing %s: %w", rel, err)
	}
	return nil
}

// mergedPlatformEntitlements returns the roothide platform base merged over
// the binary's entitlements (roothide keys win, everything else preserved).
// An unreadable or absent existing signature yields just the base — the
// input's own ldid blob is often unparseable by go-macho, and the upstream
// reference replaces rather than merges anyway.
func mergedPlatformEntitlements(b macho.Bin, origEnts []byte) ([]byte, error) {
	merged := plist.Dict{}
	var existing []byte
	if len(origEnts) > 0 {
		existing = origEnts
	} else if cur, err := b.ExtractEntitlements(); err == nil {
		existing = cur
	}
	if len(existing) > 0 {
		if d, derr := plist.Decode(existing); derr != nil {
			return nil, fmt.Errorf("decoding existing entitlements: %w", derr)
		} else {
			merged = d
		}
	}
	base, err := plist.Decode([]byte(roothideEntitlements))
	if err != nil {
		return nil, fmt.Errorf("decoding roothide entitlements: %w", err)
	}
	for k, v := range base {
		merged[k] = v
	}
	return plist.EncodeXML(merged)
}

// patchMachOForRoothide rewrites /var/jb/... dependencies and rpaths to
// @loader_path/.jbroot/... and re-signs the result (see signMachOForRoothide).
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
	if err := signMachOWithPlatformEnts(path, rel, nil); err != nil {
		return err
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
// rewrites to /rootfs/ in scripts (applied only to unprotected paths): the
// upstream seds are ` /Applications/` etc. — space + slash + root — so the
// pattern keeps the slash before the group.
var scriptPathsRe = regexp.MustCompile(` /(Applications|Library|private|System|sbin|bin|etc|lib|usr|var)/`)

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

var sandyPathsRe = regexp.MustCompile(`>/(Applications|Library|private|System|sbin|bin|etc|lib|usr|var)/`)

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

// warnRoothideFixedPaths scans the converted package for surviving /var/jb
// strings and warns about each — upstream's "fixed-paths-warning", whose
// `strings | grep /var/jb` runs over the SAME walked file set as the patch
// loop (patch.sh lines 287/340), so non-Mach-O payload files are covered
// too, not just Mach-O __cstring. In detail:
//
//   - Mach-O: __cstring strings (string tables are not rewritten by the
//     roothide pass), plus a load-command audit — a surviving /var/jb
//     dependency or rpath is a rewrite miss, which the upstream strings
//     scan would also surface.
//   - other payload files: printable strings (strings(1)-style runs),
//     with .png/.strings extensions excluded exactly like upstream's find
//     loop (`! [[ {png,strings} =~ ... ]]`). DEBIAN/ is walked (it is in
//     the walked root upstream), so patched control scripts that still
//     carry /var/jb (deliberately restored by the sed dance) warn too.
//
// Informational, never fatal.
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
		if strings.Contains(rel, "pkgmirror") {
			return nil
		}
		is, err := macho.IsMachO(path)
		if err != nil {
			return err
		}
		if is {
			b := macho.Bin{Path: path}
			deps, err := b.AllDependencies()
			if err != nil {
				return fmt.Errorf("reading dependencies of %s for fixed-path audit: %w", rel, err)
			}
			for _, dep := range deps {
				if strings.Contains(dep, "/var/jb") {
					log.Warnf("fixed-paths-warning: %s still depends on %s (load-command rewrite missed)", rel, dep)
					warned++
				}
			}
			rpaths, err := b.Rpaths()
			if err != nil {
				return fmt.Errorf("reading rpaths of %s for fixed-path audit: %w", rel, err)
			}
			for _, rp := range rpaths {
				if strings.Contains(rp, "/var/jb") {
					log.Warnf("fixed-paths-warning: %s still carries rpath %s (rpath rewrite missed)", rel, rp)
					warned++
				}
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
		}
		ext := strings.ToLower(filepath.Ext(rel))
		if ext == ".png" || ext == ".strings" {
			return nil // upstream's find-loop exclusion
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, run := range printableRuns(data) {
			if strings.Contains(run, "/var/jb") {
				log.Warnf("fixed-paths-warning: %s still contains %s (not rewritten)", rel, run)
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

// printableRuns extracts strings(1)-style printable ASCII runs (bytes 0x20-
// 0x7e, min length 4) from arbitrary bytes — the shape upstream's
// `strings - "$file" | grep /var/jb` matches on non-Mach-O payload files.
// Any run containing /var/jb is at least 8 bytes, so the min length never
// hides it.
// removeDSStore deletes every .DS_Store in the package before the repack,
// mirroring upstream's `find "$TEMPDIR_NEW" -name ".DS_Store" -delete`
// (patch.sh line 178) — macOS-built debs frequently ship Finder droppings
// that must not reach the output package (including inside the pkgmirror
// snapshot, which upstream's find also covers).
func removeDSStore(dir string) error {
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".DS_Store" {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range found {
		if err := os.Remove(p); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		log.Infof("removed .DS_Store at %s", filepath.ToSlash(rel))
	}
	return nil
}

func printableRuns(data []byte) []string {
	var runs []string
	start := -1
	for i, b := range data {
		if b >= 0x20 && b <= 0x7e {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= 4 {
			runs = append(runs, string(data[start:i]))
		}
		start = -1
	}
	if start >= 0 && len(data)-start >= 4 {
		runs = append(runs, string(data[start:]))
	}
	return runs
}
