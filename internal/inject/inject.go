// Package inject implements tweak injection into an extracted app bundle,
// ported from cyan's MainExecutable.inject(): deb expansion, per-type
// placement (dylib/framework → Frameworks, appex → PlugIns, other → root),
// common-dependency fixing (CydiaSubstrate→ElleKit-style paths, Orion,
// Cephei*), auto-injection of missing hooking frameworks from extras/, and
// entitlement preservation across the binary edits.
//
// Hooking runtime modes (feather-ellekit-spec.md D1/D5/D6): the default
// substrate mode rewrites legacy hooking dependencies to the substrate-named
// CydiaSubstrate.framework; ModeElleKit (the --ellekit flag, or automatic
// when a tweak needs libhooker) rewrites them to the real ElleKit.framework
// and thins it to the app's architecture at inject time. Modes are mutually
// exclusive per run.
package inject

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blessed0x/xkvm-ios-injector/internal/deb"
	"github.com/blessed0x/xkvm-ios-injector/internal/extras"
	"github.com/blessed0x/xkvm-ios-injector/internal/fsutil"
	"github.com/blessed0x/xkvm-ios-injector/internal/log"
	"github.com/blessed0x/xkvm-ios-injector/internal/macho"
)

// Mode selects the hooking runtime used to satisfy substrate-style
// dependencies for this run. Modes are mutually exclusive per run: output apps
// get exactly one hooking framework (feather-ellekit-spec.md D1).
type Mode int

const (
	// ModeSubstrate (default) rewrites to @rpath/CydiaSubstrate.framework/...
	// — the ElleKit-backed substrate-named shim, byte-identical to today.
	ModeSubstrate Mode = iota
	// ModeElleKit (--ellekit, or auto-selected for libhooker tweaks) rewrites
	// to @rpath/ElleKit.framework/ElleKit and auto-injects the real
	// ElleKit.framework, thinned to the app's architecture.
	ModeElleKit
)

// depInfo gives the canonical framework name and install path for a legacy
// hooking dependency in each runtime mode.
type depInfo struct {
	subName, subPath string // substrate mode ("" = unsatisfiable in that mode)
	ekName, ekPath   string // ellekit mode
}

// commonDeps maps dependency name fragments to their canonical forms,
// mirroring cyan's Executable.common and Feather's substrate→ElleKit swap.
// Mode-independent entries (Orion, Cephei*) carry identical values in both.
var commonDeps = map[string]depInfo{
	"substrate.":      {"CydiaSubstrate.framework", "@rpath/CydiaSubstrate.framework/CydiaSubstrate", "ElleKit.framework", "@rpath/ElleKit.framework/ElleKit"},
	"mobilesubstrate": {"CydiaSubstrate.framework", "@rpath/CydiaSubstrate.framework/CydiaSubstrate", "ElleKit.framework", "@rpath/ElleKit.framework/ElleKit"},
	"libsubstrate":    {"CydiaSubstrate.framework", "@rpath/CydiaSubstrate.framework/CydiaSubstrate", "ElleKit.framework", "@rpath/ElleKit.framework/ElleKit"},
	"cydiasubstrate":  {"CydiaSubstrate.framework", "@rpath/CydiaSubstrate.framework/CydiaSubstrate", "ElleKit.framework", "@rpath/ElleKit.framework/ElleKit"},
	// libhooker (Dopamine-era) is ElleKit-native: a tweak that needs it
	// auto-switches the whole run to the ElleKit runtime (spec D6).
	"libhooker":    {"", "", "ElleKit.framework", "@rpath/ElleKit.framework/ElleKit"},
	"orion.":       {"Orion.framework", "@rpath/Orion.framework/Orion", "Orion.framework", "@rpath/Orion.framework/Orion"},
	"cepheiprefs.": {"CepheiPrefs.framework", "@rpath/CepheiPrefs.framework/CepheiPrefs", "CepheiPrefs.framework", "@rpath/CepheiPrefs.framework/CepheiPrefs"},
	"cepheiui.":    {"CepheiUI.framework", "@rpath/CepheiUI.framework/CepheiUI", "CepheiUI.framework", "@rpath/CepheiUI.framework/CepheiUI"},
	"cephei.":      {"Cephei.framework", "@rpath/Cephei.framework/Cephei", "Cephei.framework", "@rpath/Cephei.framework/Cephei"},
}

// infoFor resolves a dependency fragment to the canonical form for the active
// mode.
func infoFor(key string, mode Mode) (name, path string) {
	info := commonDeps[key]
	switch mode {
	case ModeElleKit:
		return info.ekName, info.ekPath
	default:
		return info.subName, info.subPath
	}
}

// commonKeys returns the commonDeps keys ordered longest-first so that e.g.
// "cepheiui." matches before "cephei.".
func commonKeys() []string {
	keys := make([]string, 0, len(commonDeps))
	for k := range commonDeps {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return keys
}

// Injector drives injection into one app bundle.
type Injector struct {
	AppDir  string // path to the *.app
	MainBin macho.Bin
	tmpdir  string
	Mode    Mode // hooking runtime mode (default ModeSubstrate)
	// RootDylibs maps basenames of dylibs that must land in the app root
	// (not Frameworks/) with an @executable_path load command instead of
	// @rpath. Some dlopen-based tweaks (e.g. Regram) resolve their own
	// resources relative to @executable_path and crash when relocated to
	// Frameworks — the RC mod that ships them loads them from the app root.
	RootDylibs map[string]bool
}

// New returns an Injector for the app at appDir whose main executable is
// mainExec. tmpdir is a scratch dir for staging (caller owns cleanup). The
// hooking runtime starts in the default substrate mode; use SetMode for
// --ellekit.
func New(appDir, mainExec, tmpdir string) *Injector {
	return &Injector{
		AppDir:  appDir,
		MainBin: macho.Bin{Path: mainExec},
		tmpdir:  tmpdir,
		Mode:    ModeSubstrate,
	}
}

// SetMode selects the hooking runtime mode for this injection run.
func (in *Injector) SetMode(m Mode) { in.Mode = m }

// SetRootDylibs marks the given dylib paths as app-root-placed: they land in
// the *.app root (not Frameworks/) with an @executable_path/{name} load
// command. Basenames are stored; the paths are used only for identity.
func (in *Injector) SetRootDylibs(paths []string) {
	for _, p := range paths {
		if in.RootDylibs == nil {
			in.RootDylibs = map[string]bool{}
		}
		in.RootDylibs[filepath.Base(p)] = true
	}
}

// Inject processes the given tweak paths (.deb, .dylib, .framework, .appex,
// or other files/dirs to copy to the app root).
func (in *Injector) Inject(tweaks []string) error {
	// The staging dir may be passed as a not-yet-created path (tests call New
	// directly); make sure dylib staging can land there.
	if err := os.MkdirAll(in.tmpdir, 0o755); err != nil {
		return err
	}

	// Expand debs into their payload artifacts first (cyan's extract_deb
	// mutates the tweak set before injection).
	var items []string
	for _, t := range tweaks {
		if strings.HasSuffix(t, ".deb") {
			arts, err := deb.Extract(t, in.tmpdir)
			if err != nil {
				return fmt.Errorf("extracting %s: %w", filepath.Base(t), err)
			}
			log.Infof("extracted %s (%d artifact(s))", filepath.Base(t), len(arts))
			items = append(items, arts...)
			continue
		}
		items = append(items, t)
	}
	if len(items) == 0 {
		return nil
	}

	// Spec D6/§4.3: the libhooker auto-switch must be decided by a PRE-SCAN
	// of every staged tweak, before any dependency is rewritten. Flipping the
	// mode inline while processing tweak N would leave earlier tweaks (already
	// rewritten to the substrate runtime) pointing at a framework that is
	// never materialized once the run switches to ElleKit.
	if in.Mode == ModeSubstrate {
		for _, it := range items {
			if dependsOnLibhooker(it) {
				in.Mode = ModeElleKit
				log.Infof("tweak %s requires libhooker -> using the ElleKit runtime", filepath.Base(it))
				break
			}
		}
	}

	// Preserve the main binary's entitlements across the edits.
	ents, err := in.MainBin.ExtractEntitlements()
	if err != nil {
		log.Warnf("couldn't read entitlements: %v", err)
		ents = nil
	}
	hasEnts := len(ents) > 0
	if err := in.MainBin.RemoveSignature(); err != nil {
		return fmt.Errorf("removing signature from main executable: %w", err)
	}

	var hasPlugins, hasFrameworks bool
	for _, it := range items {
		switch {
		case strings.HasSuffix(it, ".appex"):
			hasPlugins = true
		case strings.HasSuffix(it, ".dylib"), strings.HasSuffix(it, ".framework"):
			// Root-placed dylibs live in the app root, so they must not
			// force a Frameworks/ dir or the @executable_path/Frameworks
			// rpath — that rpath is what lets @rpath loads resolve.
			if strings.HasSuffix(it, ".dylib") && in.RootDylibs[filepath.Base(it)] {
				continue
			}
			hasFrameworks = true
		}
	}
	if hasPlugins {
		if err := os.MkdirAll(filepath.Join(in.AppDir, "PlugIns"), 0o755); err != nil {
			return err
		}
	}
	if hasFrameworks {
		fw := in.frameworksDir()
		if err := os.MkdirAll(fw, 0o755); err != nil {
			return err
		}
		// A duplicate rpath is not an error: nativeAddRpath rewrites the
		// original bytes and returns nil for it.
		if err := in.MainBin.AddRpath("@executable_path/Frameworks"); err != nil {
			return fmt.Errorf("adding Frameworks rpath to main executable: %w", err)
		}
	}

	needed := map[string]bool{}
	injectedNames := basenames(items)
	for _, it := range items {
		// cyan skips symlink tweaks outright ("can potentially have some
		// security implications"); match that posture.
		if st, err := os.Lstat(it); err == nil && st.Mode()&os.ModeSymlink != 0 {
			log.Infof("skipping symlink %s", filepath.Base(it))
			continue
		}
		switch {
		case strings.HasSuffix(it, ".appex"):
			if err := in.injectAppex(it); err != nil {
				return err
			}
		case strings.HasSuffix(it, ".dylib"):
			if in.RootDylibs[filepath.Base(it)] {
				if err := in.injectDylibRoot(it, needed, injectedNames); err != nil {
					return err
				}
				continue
			}
			if err := in.injectDylib(it, needed, injectedNames); err != nil {
				return err
			}
		case strings.HasSuffix(it, ".framework"):
			if err := in.injectFramework(it); err != nil {
				return err
			}
		default:
			if err := in.copyToRoot(it); err != nil {
				return err
			}
		}
	}

	// Orion has a weak reference to substrate but still needs it present.
	if needed["orion."] {
		needed["substrate."] = true
	}
	for missing := range needed {
		if err := in.autoInject(missing); err != nil {
			return err
		}
	}

	if hasEnts {
		if err := in.MainBin.SignWithEntitlements(ents); err != nil {
			return fmt.Errorf("restoring entitlements: %w", err)
		}
		log.Infof("restored entitlements")
	}
	return nil
}

func (in *Injector) frameworksDir() string { return filepath.Join(in.AppDir, "Frameworks") }

func (in *Injector) injectAppex(path string) error {
	bn := filepath.Base(path)
	dest := filepath.Join(in.AppDir, "PlugIns", bn)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := fsutil.CopyTree(path, dest); err != nil {
		return err
	}
	log.Infof("injected %s (appex)", bn)
	return nil
}

func (in *Injector) injectDylib(path string, needed map[string]bool, injectedNames []string) error {
	bn := filepath.Base(path)
	// Work on a staging copy so fixes never touch the user's original.
	stage := filepath.Join(in.tmpdir, bn)
	if err := fsutil.CopyFile(path, stage); err != nil {
		return err
	}
	e := macho.Bin{Path: stage}
	if err := fixCommonDeps(e, needed, &in.Mode); err != nil {
		return err
	}
	if err := fixInjectedDeps(e, injectedNames, in.RootDylibs); err != nil {
		return err
	}
	if err := in.MainBin.InjectWeak("@rpath/" + bn); err != nil {
		return fmt.Errorf("injecting %s: %w", bn, err)
	}
	final := filepath.Join(in.frameworksDir(), bn)
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	log.Infof("injected %s (dylib)", bn)
	return nil
}

// injectDylibRoot stages a dylib to the app root and injects an
// @executable_path/{name} weak load command on the main binary — the load
// contract dlopen-based tweaks (e.g. Regram, whose install name is
// /Library/MobileSubstrate/DynamicLibraries/Regram.dylib and whose resources
// resolve relative to @executable_path) require. This is the opposite of
// injectDylib's Frameworks/@rpath contract and is selected per-file via
// SetRootDylibs.
func (in *Injector) injectDylibRoot(path string, needed map[string]bool, injectedNames []string) error {
	bn := filepath.Base(path)
	stage := filepath.Join(in.tmpdir, bn)
	if err := fsutil.CopyFile(path, stage); err != nil {
		return err
	}
	e := macho.Bin{Path: stage}
	if err := fixCommonDeps(e, needed, &in.Mode); err != nil {
		return err
	}
	if err := fixInjectedDeps(e, injectedNames, in.RootDylibs); err != nil {
		return err
	}
	if err := in.MainBin.InjectWeak("@executable_path/" + bn); err != nil {
		return fmt.Errorf("injecting %s: %w", bn, err)
	}
	final := filepath.Join(in.AppDir, bn)
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return err
	}
	log.Infof("injected %s (dylib, app root)", bn)
	return nil
}

func (in *Injector) injectFramework(path string) error {
	bn := filepath.Base(path)
	// Framework binary name = framework name minus ".framework".
	binaryName := strings.TrimSuffix(bn, ".framework")
	if err := in.MainBin.InjectWeak(fmt.Sprintf("@rpath/%s/%s", bn, binaryName)); err != nil {
		return fmt.Errorf("injecting %s: %w", bn, err)
	}
	dest := filepath.Join(in.frameworksDir(), bn)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := fsutil.CopyTree(path, dest); err != nil {
		return err
	}
	log.Infof("injected %s (framework)", bn)
	return nil
}

func (in *Injector) copyToRoot(path string) error {
	bn := filepath.Base(path)
	dest := filepath.Join(in.AppDir, bn)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	var err error
	if st, statErr := os.Stat(path); statErr != nil {
		return statErr
	} else if st.IsDir() {
		err = fsutil.CopyTree(path, dest)
	} else {
		err = fsutil.CopyFile(path, dest)
	}
	if err != nil {
		return err
	}
	log.Infof("copied %s to app root", bn)
	return nil
}

// autoInject copies the missing hooking framework for a common-dependency key
// from the embedded extras into Frameworks/, resolved for the active mode. In
// ElleKit mode the real ElleKit.framework is thinned to the main executable's
// architecture after copying (spec D8/D9).
func (in *Injector) autoInject(key string) error {
	name, _ := infoFor(key, in.Mode)
	if name == "" {
		return fmt.Errorf("no runtime for dependency %q in mode %d", key, in.Mode)
	}
	fw := in.frameworksDir()
	if err := os.MkdirAll(fw, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(fw, name)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := extras.CopyFramework(name, fw); err != nil {
		return err
	}
	log.Infof("auto-injected %s", name)

	if name == "ElleKit.framework" {
		archs, err := in.MainBin.Architectures()
		if err != nil || len(archs) == 0 {
			return nil // leave the fat runtime; fakesign/thin can still handle it
		}
		exe := filepath.Join(dest, "ElleKit")
		if err := (macho.Bin{Path: exe}).ThinToArch(archs[0]); err != nil {
			log.Warnf("couldn't thin %s to %s: %v", name, archs[0], err)
		} else {
			log.Infof("thinned %s to %s", name, archs[0])
		}
	}
	return nil
}

// fixCommonDeps rewrites known hooking dependencies (substrate/libhooker/orion/
// cephei*) to their canonical @rpath forms for the active mode, mirroring
// cyan's fix_common_dependencies and Feather's substrate→ElleKit swap. mode is
// a pointer so a libhooker dependency can auto-switch the whole run to the
// ElleKit runtime (spec D6) mid-scan.
func fixCommonDeps(e macho.Bin, needed map[string]bool, mode *Mode) error {
	if err := e.RemoveSignature(); err != nil {
		return err
	}
	deps, err := e.Dependencies()
	if err != nil {
		return err
	}
	for _, dep := range deps {
		lower := strings.ToLower(dep)
		for _, key := range commonKeys() {
			if !strings.Contains(lower, key) {
				continue
			}
			// libhooker is the Dopamine/ElleKit-native runtime: the pre-scan in
			// Inject() already flipped the run to ModeElleKit; this guard is a
			// no-op safety net for direct callers of fixCommonDeps.
			if key == "libhooker" && *mode == ModeSubstrate {
				*mode = ModeElleKit
			}
			_, path := infoFor(key, *mode)
			needed[key] = true
			if dep == path {
				break
			}
			if err := e.ChangeDependency(dep, path); err != nil {
				log.Warnf("couldn't fix dependency %q -> %q in %s: %v", dep, path, filepath.Base(e.Path), err)
			} else {
				log.Infof("fixed common dependency in %s: %s -> %s", filepath.Base(e.Path), dep, path)
			}
			break
		}
	}
	return nil
}

// fixInjectedDeps rewrites dependencies that reference other injected tweaks
// to their install locations, mirroring cyan's fix_dependencies. Frameworks/
// dylibs get @rpath; app-root dylibs (rootNames) get @executable_path — the
// same contract their loader uses, so a tweak depending on a root-placed
// dylib resolves it where it actually landed.
func fixInjectedDeps(e macho.Bin, names []string, rootNames map[string]bool) error {
	deps, err := e.Dependencies()
	if err != nil {
		return err
	}
	for _, dep := range deps {
		for _, cname := range names {
			if !strings.Contains(dep, cname) {
				continue
			}
			var npath string
			if strings.HasSuffix(cname, ".framework") {
				npath = "@rpath/" + cname + "/" + strings.TrimSuffix(cname, ".framework")
			} else if rootNames[cname] {
				npath = "@executable_path/" + cname
			} else {
				npath = "@rpath/" + cname
			}
			if dep != npath {
				if err := e.ChangeDependency(dep, npath); err != nil {
					log.Warnf("couldn't fix dependency %q -> %q in %s: %v", dep, npath, filepath.Base(e.Path), err)
				}
			}
			break
		}
	}
	return nil
}

// dependsOnLibhooker reports whether the given staged item depends on
// libhooker. A .dylib is checked directly; a .framework resolves its inner
// binary (Foo.framework/Foo); anything else has no load commands to scan.
// Used by the D6 pre-scan before any dependency rewrite happens.
func dependsOnLibhooker(item string) bool {
	var binPath string
	switch {
	case strings.HasSuffix(item, ".dylib"):
		binPath = item
	case strings.HasSuffix(item, ".framework"):
		binPath = filepath.Join(item, strings.TrimSuffix(filepath.Base(item), ".framework"))
	default:
		return false
	}
	deps, err := (macho.Bin{Path: binPath}).Dependencies()
	if err != nil {
		log.Warnf("couldn't read dependencies of %s: %v", filepath.Base(item), err)
		return false
	}
	for _, dep := range deps {
		if strings.Contains(strings.ToLower(dep), "libhooker") {
			return true
		}
	}
	return false
}

func basenames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}
