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
	"github.com/xkvm/xkvm/internal/cyanfile"
	"github.com/xkvm/xkvm/internal/extras"
	"github.com/xkvm/xkvm/internal/fetch"
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
	// .cyan configs must be parsed BEFORE validate: their inject/ payloads
	// are materialized into tmpdir and appended to Files, so the existence
	// checks in validate must see them. Upstream (parse_cyans) runs them
	// after the encryption check; xkvm runs them first because validate
	// is xkvm's input gate.
	tmpdir, err := os.MkdirTemp("", "xkvm-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	if len(opts.Cyans) > 0 {
		if err := mergeCyans(opts, tmpdir); err != nil {
			return err
		}
	}
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

	// M4: --fetch resolves tweak bundle ids to local .debs (Canister index →
	// repo Packages index → recursive Depends:) and feeds them into the same
	// injection set as -f files. --apt-source repos are searched first;
	// --no-recurse skips the dependency closure.
	if len(opts.Fetch) > 0 {
		log.Infof("fetching %d tweak(s) via Canister/MobileAPT", len(opts.Fetch))
		fetched, err := fetch.Resolve(ctx, opts.Fetch, opts.APTSource, opts.NoRecurse, filepath.Join(tmpdir, "fetch"), nil)
		if err != nil {
			return err
		}
		log.Infof("fetched %d .deb(s)", len(fetched))
		opts.Files = append(opts.Files, fetched...)
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
		// App-root dylibs (@executable_path contract) are tracked by
		// basename so the same file can be listed in both -f and
		// --root-dylib: -f carries it into the injection set, --root-dylib
		// overrides its placement.
		//
		// Auto-restore: if the -f files were produced by `xkvm extract`, the
		// extraction manifest next to them records each dylib's original
		// placement. Root-placed dylibs (e.g. Regram) are re-rooted without
		// any flag; explicit --root-dylib entries still merge in.
		rootDylibs := opts.RootDylibs
		if autoRoots, err := rootDylibsFromManifests(files); err != nil {
			return err
		} else if len(autoRoots) > 0 {
			log.Infof("restoring app-root placement from extraction manifest(s): %s", strings.Join(autoRoots, ", "))
			rootDylibs = append(rootDylibs, autoRoots...)
		}
		inj.SetRootDylibs(rootDylibs)
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

// manifestName is the sidecar `xkvm extract` writes next to the extracted
// artifacts. Re-injection reads it to restore each artifact's original
// placement (root vs Frameworks) without a --root-dylib flag.
const manifestName = "xkvm-manifest.json"

// ExtractArtifacts unpacks an app (ipa/tipa/app) and copies the injectable
// artifacts (dylibs, frameworks, appex, bundles) it contains into outDir.
// This is the `xkvm extract` command: dump tweaks from an injected app. A
// manifest sidecar (manifestName) is written next to the artifacts recording
// each one's original placement in the bundle, so re-injection honors it.
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
	manifest := &artifact.Manifest{Format: 1, Source: input}
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

// rootDylibsFromManifests walks up from each given .dylib file's directory
// (bounded to manifestMaxDepth ancestor levels) looking for an xkvm
// extraction manifest, and returns the basenames of dylibs recorded there as
// app-root-placed. Re-injection uses this to restore the original placement
// automatically — the @executable_path contract (e.g. Regram) survives
// extract → re-inject without a --root-dylib flag. Frameworks-placed dylibs
// and non-dylib artifacts need no action: the injector already sends them to
// their canonical locations by default.
const manifestMaxDepth = 4

func rootDylibsFromManifests(files []string) ([]string, error) {
	cache := map[string]*artifact.Manifest{}
	var roots []string
	seen := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".dylib") {
			continue
		}
		bn := filepath.Base(f)
		dir := filepath.Dir(f)
		for depth := 0; depth < manifestMaxDepth && dir != "" && dir != "." && dir != string(filepath.Separator); depth++ {
			m := cache[dir]
			if m == nil {
				mp := filepath.Join(dir, manifestName)
				if _, err := os.Stat(mp); err != nil {
					dir = filepath.Dir(dir)
					continue
				}
				read, err := artifact.ReadManifest(mp)
				if err != nil {
					// A corrupt sidecar must not abort the injection; drop the
					// auto-restore and let the file land by default placement.
					log.Warnf("ignoring unreadable extraction manifest %s: %v", mp, err)
					break
				}
				m = read
				cache[dir] = m
			}
			for _, e := range m.Artifacts {
				if e.Name == bn && e.Placement == artifact.PlacementRoot && !seen[bn] {
					seen[bn] = true
					roots = append(roots, bn)
				}
			}
			break
		}
	}
	return roots, nil
}

// mergeCyans applies each .cyan config to opts, mirroring cyan's
// parse_cyans: inject/ payloads APPEND to the file set (config wins on
// basename collision), root_dylibs payloads are appended to RootDylibs, the
// bundled icon/plist/entitlements are wired, and every remaining scalar key
// OVERRIDES the corresponding CLI option. Configs are applied in -z order,
// later configs winning over earlier ones for scalar keys.
func mergeCyans(opts *Options, tmpdir string) error {
	for i, c := range opts.Cyans {
		log.Infof("parsing %s..", filepath.Base(c))
		outDir := filepath.Join(tmpdir, fmt.Sprintf("cyan-%d", i))
		cfg, err := cyanfile.Parse(c, outDir)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", c, err)
		}

		// Inject payloads append after the CLI files so the basename-dedup in
		// validate() resolves collisions in the config's favor (upstream: the
		// config dict is merged last).
		opts.Files = append(opts.Files, cfg.Files...)
		opts.RootDylibs = append(opts.RootDylibs, cfg.RootDylibs...)

		if cfg.Icon != "" {
			opts.Icon = cfg.Icon
		}
		if cfg.PlistMerge != "" {
			opts.PlistMerge = cfg.PlistMerge
		}
		if cfg.Entitlement != "" {
			opts.Entitlements = cfg.Entitlement
		}
		// Remaining scalar keys override the CLI (upstream `args[k] = v`).
		if cfg.Name != "" {
			opts.Name = cfg.Name
		}
		if cfg.Version != "" {
			opts.Version = cfg.Version
		}
		if cfg.BundleID != "" {
			opts.BundleID = cfg.BundleID
		}
		if cfg.MinimumOS != "" {
			opts.MinimumOS = cfg.MinimumOS
		}
		if cfg.Compress != 0 {
			opts.Compress = cfg.Compress
		}
		opts.Fakesign = opts.Fakesign || cfg.Fakesign
		opts.Thin = opts.Thin || cfg.Thin
		opts.ElleKit = opts.ElleKit || cfg.ElleKit
		opts.RemoveExtensions = opts.RemoveExtensions || cfg.RemoveExts
		opts.RemoveEncrypted = opts.RemoveEncrypted || cfg.RemoveEnc
		opts.NoWatch = opts.NoWatch || cfg.NoWatch
		opts.EnableDocuments = opts.EnableDocuments || cfg.EnableDocs
		opts.RemoveSupportedDevices = opts.RemoveSupportedDevices || cfg.RemoveDevs
		opts.IgnoreEncrypted = opts.IgnoreEncrypted || cfg.IgnoreEnc
		opts.Overwrite = opts.Overwrite || cfg.Overwrite
		opts.Patches = append(opts.Patches, cfg.Patches...)
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
