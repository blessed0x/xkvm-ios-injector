package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/artifact"
	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
	"github.com/xscope0/xkvm-ios-injector/internal/plist"
	"github.com/xscope0/xkvm-ios-injector/internal/rootless"
)

// DebifyOptions configures `xkvm debify`: wrapping a tweak dylib (or a
// payload directory) into a standard MobileSubstrate .deb.
type DebifyOptions struct {
	Input  string // .dylib file, or a directory treated as the payload root
	Output string // .deb destination

	Name        string   // display name / package name base
	Version     string   // default "1.0"
	Maintainer  string   // default "xkvm"
	Author      string   // default "xkvm"
	Description string   // default: "<name> (built with xkvm debify)"
	BundleIDs   []string // target app bundle ids → Filter/Bundles plist
	Filter      string   // exact filter plist to ship instead of a generated one
	Resources   []string // extra payload files as src:dest pairs
	Depends     []string // Depends entries (default: mobilesubstrate)
}

// Debify is the `xkvm debify` entry point: build a MobileSubstrate tweak deb
// from a dylib (+ optional resources/filter), matching the Dylib-to-Deb
// Converter format: DEBIAN/control + Library/MobileSubstrate/DynamicLibraries/
// {name}.dylib (+ {name}.plist filter).
func Debify(o DebifyOptions) error {
	st, err := os.Stat(o.Input)
	if err != nil {
		return fmt.Errorf("%s does not exist", o.Input)
	}
	if !st.IsDir() && !strings.HasSuffix(o.Input, ".dylib") {
		return fmt.Errorf("the input must be a .dylib or a payload directory")
	}

	display := o.Name
	if display == "" {
		display = strings.TrimSuffix(filepath.Base(o.Input), filepath.Ext(o.Input))
	}
	pkg := sanitizePkgName(display)
	version := o.Version
	if version == "" {
		version = "1.0"
	}
	depends := o.Depends
	if len(depends) == 0 {
		depends = []string{"mobilesubstrate"}
	}
	description := o.Description
	if description == "" {
		description = display + " (built with xkvm debify)"
	}
	maintainer := o.Maintainer
	if maintainer == "" {
		maintainer = "xkvm"
	}
	author := o.Author
	if author == "" {
		author = "xkvm"
	}

	tmpdir, err := os.MkdirTemp("", "xkvm-debify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)
	if err := os.MkdirAll(filepath.Join(tmpdir, "DEBIAN"), 0o755); err != nil {
		return err
	}

	ctl := deb.Control{
		Package:      pkg,
		Name:         display,
		Version:      version,
		Architecture: "iphoneos-arm64",
		Depends:      depends,
		Description:  description,
		Maintainer:   maintainer,
		Author:       author,
		Section:      "Tweaks",
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "DEBIAN", "control"), []byte(ctl.String()), 0o644); err != nil {
		return err
	}

	// Payload: a single dylib lands in DynamicLibraries (the substrate
	// convention); a directory is copied wholesale as the payload root.
	dlDir := filepath.Join(tmpdir, "Library", "MobileSubstrate", "DynamicLibraries")
	if st.IsDir() {
		if err := copyDir(o.Input, tmpdir); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(dlDir, 0o755); err != nil {
			return err
		}
		if err := copyFile(o.Input, filepath.Join(dlDir, pkg+".dylib")); err != nil {
			return err
		}
		log.Infof("payload: %s", filepath.Join("Library", "MobileSubstrate", "DynamicLibraries", pkg+".dylib"))
	}

	// Filter plist: an explicit --filter wins; otherwise generate one from
	// the --bundle-id list (substrate Filter/Bundles).
	if o.Filter != "" {
		if err := os.MkdirAll(dlDir, 0o755); err != nil {
			return err
		}
		if err := copyFile(o.Filter, filepath.Join(dlDir, pkg+".plist")); err != nil {
			return err
		}
	} else if len(o.BundleIDs) > 0 {
		if err := os.MkdirAll(dlDir, 0o755); err != nil {
			return err
		}
		ids := make([]any, 0, len(o.BundleIDs))
		for _, id := range o.BundleIDs {
			ids = append(ids, id)
		}
		if err := plist.WriteXML(filepath.Join(dlDir, pkg+".plist"), plist.Dict{
			"Filter": plist.Dict{"Bundles": ids},
		}); err != nil {
			return err
		}
		log.Infof("filter: bundles %s", strings.Join(o.BundleIDs, ", "))
	}

	// Extra payload files: src:dest pairs, copied verbatim (file or tree).
	for _, r := range o.Resources {
		src, dest, ok := strings.Cut(r, ":")
		if !ok || src == "" || dest == "" {
			return fmt.Errorf("--resource expects src:dest, got %q", r)
		}
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("--resource source %q does not exist", src)
		}
		target := filepath.Join(tmpdir, filepath.FromSlash(dest))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyDir(src, target); err != nil {
			return err
		}
		log.Infof("payload: %s", dest)
	}

	if dir := filepath.Dir(o.Output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := deb.Build(tmpdir, o.Output); err != nil {
		return fmt.Errorf("building %s: %w", o.Output, err)
	}
	log.Infof("wrote deb at %s (package %s, %s)", o.Output, pkg, version)
	return nil
}

// Undeb is the `xkvm undeb` entry point (Forte parity): extract a tweak .deb
// and dump its injectable artifacts (dylibs, frameworks, bundles) into
// outDir, with the same placement manifest `xkvm extract` writes so
// re-injection honors original locations.
func Undeb(input, outDir string) error {
	if !strings.HasSuffix(input, ".deb") {
		return fmt.Errorf("the input must be a .deb")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	tmpdir, err := os.MkdirTemp("", "xkvm-undeb-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	log.Infof("extracting %s..", input)
	arts, err := deb.Extract(input, tmpdir)
	if err != nil {
		return err
	}
	if len(arts) == 0 {
		log.Warnf("no tweak artifacts found in %s", input)
		return nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	return exportArtifacts(tmpdir, outDir, input, arts)
}

// Rootless is the `xkvm rootless` entry point: convert a rootful .deb to a
// rootless one (payload under var/jb, control edits, Mach-O load-command
// path conversion). tweakinject additionally applies the modern
// Dopamine/ellekit conventions (TweakInject layout, @rpath/libsubstrate.dylib
// shim). See internal/rootless.
func Rootless(input, output string, thin, tweakinject bool) error {
	if !strings.HasSuffix(input, ".deb") {
		return fmt.Errorf("the input must be a .deb")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	if output == "" {
		return fmt.Errorf("the output path is required")
	}
	return rootless.Convert(input, output, thin, tweakinject)
}

// RootlessXina is the `xkvm rootless --xina` entry point: convert a rootful
// .deb to a Xina-style rootless one (the Xinam1nePatcher pipeline: short
// symlink-form byte seds, @rpath conventions, plist/script seds). See
// internal/rootless.
func RootlessXina(input, output string) error {
	if !strings.HasSuffix(input, ".deb") {
		return fmt.Errorf("the input must be a .deb")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	if output == "" {
		return fmt.Errorf("the output path is required")
	}
	return rootless.ConvertToXina(input, output)
}

// Rootful is the `xkvm rootful` entry point: convert a rootless .deb back to
// a rootful one (var/jb payload hoisted, /var/jb load commands and string
// paths plus the Xina short forms rewritten to rootful paths, @rpath
// substrate shims undone, control edits reversed). See internal/rootless.
func Rootful(input, output string) error {
	if !strings.HasSuffix(input, ".deb") {
		return fmt.Errorf("the input must be a .deb")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	if output == "" {
		return fmt.Errorf("the output path is required")
	}
	return rootless.ConvertToRootful(input, output)
}

// Roothide is the `xkvm roothide` entry point: convert a rootless .deb to a
// roothide-jailbreak one (var/jb payload hoisted to the package root, system
// files under rootfs/, /var/jb → @loader_path/.jbroot load-command and rpath
// rewrites, arm64e control edits). See internal/rootless.
func Roothide(input, output string, pkgmirror bool, mode string) error {
	if !strings.HasSuffix(input, ".deb") {
		return fmt.Errorf("the input must be a .deb")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	if output == "" {
		return fmt.Errorf("the output path is required")
	}
	return rootless.ConvertToRoothide(input, output, pkgmirror, mode)
}

// exportArtifacts copies collected artifacts to outDir, deduping by basename
// and writing the placement manifest — shared by `xkvm extract` and
// `xkvm undeb`.
func exportArtifacts(appDir, outDir, source string, arts []string) error {
	manifest := &artifact.Manifest{Format: 1, Source: source}
	for _, a := range arts {
		base := filepath.Base(a)
		dest := filepath.Join(outDir, base)
		if _, err := os.Stat(dest); err == nil {
			log.Warnf("skipping duplicate artifact %s (already extracted)", base)
			continue
		}
		if err := copyDir(a, dest); err != nil {
			return err
		}
		rel, err := filepath.Rel(appDir, a)
		if err != nil {
			return err
		}
		manifest.Artifacts = append(manifest.Artifacts, artifact.ManifestEntry{
			Name:      base,
			Kind:      artifact.KindFor(base),
			Placement: artifact.PlacementFor(rel),
		})
		log.Infof("extracted %s (%s, %s)", base, artifact.KindFor(base), artifact.PlacementFor(rel))
	}
	if err := artifact.WriteManifest(filepath.Join(outDir, manifestName), manifest); err != nil {
		return fmt.Errorf("writing extraction manifest: %w", err)
	}
	log.Infof("wrote %s (%d artifact(s), placements remembered)", manifestName, len(manifest.Artifacts))
	return nil
}

// copyFile copies a single file, preserving mode.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, st.Mode().Perm())
}

// sanitizePkgName lowercases and reduces display names to dpkg-legal package
// characters ([a-z0-9+.-]); everything else is dropped.
func sanitizePkgName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '.', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "tweak"
	}
	return b.String()
}
