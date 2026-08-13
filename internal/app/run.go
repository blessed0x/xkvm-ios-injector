package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xkvm/xkvm/internal/appbundle"
	"github.com/xkvm/xkvm/internal/artifact"
	"github.com/xkvm/xkvm/internal/extras"
	"github.com/xkvm/xkvm/internal/inject"
	"github.com/xkvm/xkvm/internal/ipa"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/patch"
	"github.com/xkvm/xkvm/internal/plist"
)

// Run executes the xkvm pipeline, mirroring cyan's logic.main() ordering:
// encryption check → extension removal → injection → metadata edits →
// uisd/watch/documents → mass fakesign/thin → output. Azule-heritage
// fetch/decrypt (M4/M5) are out of scope per the user's narrowing.
func Run(ctx context.Context, opts *Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	// Compare cleaned absolute paths so aliases like `-i foo.ipa -o ./foo.ipa`
	// are caught too.
	inAbs, _ := filepath.Abs(opts.Input)
	outAbs, _ := filepath.Abs(opts.Output)
	if outAbs == inAbs && !opts.Overwrite {
		return errors.New("refusing to overwrite the input; pass --overwrite or -o with a new path")
	}

	log.Infof("xkvm v%s", Version)
	log.Infof("input: %s", opts.Input)
	log.Infof("output: %s", opts.Output)

	inputIsIPA := strings.HasSuffix(opts.Input, ".ipa") || strings.HasSuffix(opts.Input, ".tipa")
	outputIsIPA := strings.HasSuffix(opts.Output, ".ipa") || strings.HasSuffix(opts.Output, ".tipa")

	tmpdir, err := os.MkdirTemp("", "xkvm-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	appDir, err := prepareApp(opts.Input, tmpdir, inputIsIPA)
	if err != nil {
		return err
	}

	bundle, err := appbundle.Open(appDir)
	if err != nil {
		return err
	}
	logAppIdentity(bundle.Info)

	// The main binary must be decryptable to be edited.
	encrypted, err := bundle.Main.IsEncrypted()
	if err != nil {
		return err
	}
	if encrypted {
		if opts.IgnoreEncrypted {
			log.Infof("main binary is encrypted, ignoring")
		} else {
			return errors.New("main binary is encrypted; exiting (pass --ignore-encrypted to continue)")
		}
	}

	// Extension removal runs before injection: the user may inject their own.
	if opts.RemoveExtensions {
		if err := bundle.RemoveAllExtensions(); err != nil {
			return err
		}
	} else if opts.RemoveEncrypted {
		if _, err := bundle.RemoveEncryptedExtensions(); err != nil {
			return err
		}
	}

	// --patch: bundle the sideload-fix dylibs into the injection set so they
	// land in Frameworks/ with an LC_LOAD_DYLIB on the main binary, exactly
	// like any -f tweak. Fakesign is implied: iOS refuses to load unsigned
	// dylibs, so a patch without signing would silently never apply.
	files := opts.Files
	if opts.Patch {
		fixes, err := extras.MaterializeSideloadFixes(filepath.Join(tmpdir, "sideload"))
		if err != nil {
			return err
		}
		log.Infof("adding %d bundled sideload fix(es)", len(fixes))
		files = append(files, fixes...)
		if !opts.Fakesign {
			opts.Fakesign = true
			log.Infof("--patch implies --fakesign")
		}
	}

	if len(files) > 0 {
		injectDir := filepath.Join(tmpdir, "inject")
		if err := os.MkdirAll(injectDir, 0o755); err != nil {
			return err
		}
		inj := inject.New(appDir, bundle.Main.Path, injectDir)
		if opts.ElleKit {
			inj.SetMode(inject.ModeElleKit)
			log.Infof("using the ElleKit runtime (--ellekit)")
		}
		if err := inj.Inject(files); err != nil {
			return err
		}
	}

	if opts.Name != "" {
		if err := bundle.ChangeName(opts.Name); err != nil {
			return err
		}
	}
	if opts.Version != "" {
		if err := bundle.ChangeVersion(opts.Version); err != nil {
			return err
		}
	}
	if opts.BundleID != "" {
		if err := bundle.ChangeBundleID(opts.BundleID); err != nil {
			return err
		}
	}
	if opts.MinimumOS != "" {
		if err := bundle.ChangeMinimumOS(opts.MinimumOS); err != nil {
			return err
		}
	}
	if opts.Icon != "" {
		if err := bundle.ChangeIcon(opts.Icon); err != nil {
			return err
		}
	}
	if opts.PlistMerge != "" {
		if err := bundle.MergePlist(opts.PlistMerge); err != nil {
			return err
		}
	}
	if opts.Entitlements != "" {
		ents, err := os.ReadFile(opts.Entitlements)
		if err != nil {
			return err
		}
		if err := bundle.Main.SignWithEntitlements(ents); err != nil {
			return fmt.Errorf("couldn't sign with entitlements: %w", err)
		}
		log.Infof("merged new entitlements")
	}

	if opts.RemoveSupportedDevices {
		if err := bundle.RemoveUISupportedDevices(); err != nil {
			return err
		}
	}
	if opts.NoWatch {
		if err := bundle.RemoveWatchApps(); err != nil {
			return err
		}
	}
	if opts.EnableDocuments {
		if err := bundle.EnableDocuments(); err != nil {
			return err
		}
	}

	// Compatibility patches (feather-ellekit-spec.md §4.5) run after all
	// plist/bundle operations and before fakesign: the Liquid Glass Mach-O
	// edit mutates the main binary, so it must precede signing.
	if len(opts.Patches) > 0 {
		if err := patch.Apply(appDir, opts.Patches); err != nil {
			return err
		}
	}

	if opts.Fakesign {
		if _, err := bundle.FakesignAll(); err != nil {
			return err
		}
	}
	if opts.Thin {
		if _, err := bundle.ThinAll(); err != nil {
			return err
		}
	}

	// create subdirectories if necessary, like cyan
	if dir := filepath.Dir(opts.Output); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if outputIsIPA {
		log.Infof("generating ipa with compression level %d..", opts.Compress)
		if err := ipa.Repack(tmpdir, opts.Output, opts.Compress); err != nil {
			return err
		}
		log.Infof("generated ipa at %s", opts.Output)
	} else {
		if _, err := os.Stat(opts.Output); err == nil {
			os.RemoveAll(opts.Output)
		}
		if err := copyDir(appDir, opts.Output); err != nil {
			return err
		}
		log.Infof("generated app at %s", opts.Output)
	}
	return nil
}

// ExtractArtifacts unpacks an app (ipa/tipa/app) and copies the injectable
// artifacts (dylibs, frameworks, appex, bundles) it contains into outDir.
// This is the `xkvm extract` command: dump tweaks from an injected app.
func ExtractArtifacts(input, outDir string) error {
	ext := strings.ToLower(filepath.Ext(input))
	isIPA := ext == ".ipa" || ext == ".tipa"
	if !isIPA && ext != ".app" {
		return fmt.Errorf("the input must be an app/ipa/tipa")
	}
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}

	tmpdir, err := os.MkdirTemp("", "xkvm-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	var appDir string
	if isIPA {
		log.Infof("extracting %s..", input)
		appDir, err = ipa.Extract(input, tmpdir)
		if err != nil {
			return err
		}
	} else {
		appDir = input
	}

	arts, err := artifact.Collect(appDir)
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
	for _, a := range arts {
		base := filepath.Base(a)
		dest := filepath.Join(outDir, base)
		if _, err := os.Stat(dest); err == nil {
			// Two artifacts can share a basename (e.g. Frameworks/X.framework
			// and PlugIns/x/X.framework); never clobber the first one.
			log.Warnf("skipping duplicate artifact %s (already extracted)", base)
			continue
		}
		if err := copyDir(a, dest); err != nil {
			return err
		}
		log.Infof("extracted %s", base)
	}
	return nil
}

func prepareApp(input, tmpdir string, isIPA bool) (string, error) {
	if isIPA {
		log.Infof("extracting ipa..")
		app, err := ipa.Extract(input, tmpdir)
		if err != nil {
			return "", err
		}
		log.Infof("extracted ipa")
		return app, nil
	}
	log.Infof("copying app..")
	dest := filepath.Join(tmpdir, "Payload", filepath.Base(input))
	if err := copyDir(input, dest); err != nil {
		return "", err
	}
	log.Infof("copied app")
	return dest, nil
}

func logAppIdentity(info plist.Dict) {
	if name, _ := info["CFBundleName"].(string); name != "" {
		log.Infof("app name: %s", name)
	}
	if bid, _ := info["CFBundleIdentifier"].(string); bid != "" {
		log.Infof("bundle id: %s", bid)
	}
}

// copyDir copies a directory tree (or a single file) from src to dst,
// preserving permissions and materializing symlinks.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
