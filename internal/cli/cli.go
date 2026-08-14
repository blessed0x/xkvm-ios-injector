// Package cli wires the xkvm command line: flag definitions, help text and
// subcommands (xkvm, xkvm cgen). Flag semantics are cyan-compatible; see
// ARCHITECTURE.md §5 for the full surface and collision decisions.
package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/xkvm/xkvm/internal/app"
	"github.com/xkvm/xkvm/internal/cyanfile"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/patch"
)

// Runner is the injectable pipeline entry point. Tests replace it with a
// capture function; production wires it to app.Run.
type Runner func(ctx context.Context, opts *app.Options) error

// Main builds and executes the root command, mapping any error to a non-zero exit.
func Main() {
	if err := NewRootCmd(app.Run).Execute(); err != nil {
		log.Errorf("%v", err)
		os.Exit(1)
	}
}

// NewRootCmd returns the `xkvm` command with the full cyan-compatible flag
// surface, plus the Azule-heritage long flags (implemented in M4/M5).
func NewRootCmd(run Runner) *cobra.Command {
	opts := &app.Options{}
	var (
		silent      bool
		showVersion bool
	)
	// Declared before the RunE closure (which references it) so it is in
	// scope; filled in below as flags are bound.
	var patchFlags map[string]*bool

	cmd := &cobra.Command{
		Use:   "xkvm [flags] -i <app>",
		Short: "iOS app modifier & tweak injector (cyan + Azule heritage, in Go)",
		Long: `xkvm is a Go rewrite merging pyzule-rw/cyan (app modification and tweak
injection) with Azule's repo fetching and App Store decryption.

Flag names and semantics are cyan-compatible. All modification flags apply to
the -i input; the result is written to -o, or overwrites the input.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// cyan's argparse uses nargs="+", so space-separated values after a
		// single flag are legal there. pflag can only repeat flags, so any
		// trailing positional args are folded into the last-specified array
		// flag by collectTrailingArgs (below).
		Args: cobra.ArbitraryArgs,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			log.SetSilent(silent)
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Fprintf(cmd.OutOrStdout(), "xkvm v%s\n", app.Version)
				return nil
			}
			if opts.Input == "" {
				// Manual check (cobra's MarkFlagRequired runs before RunE
				// and would block --version). Matches cyan's argparse
				// required=True for -i/--input.
				return fmt.Errorf("required flag(s) \"input\" not set")
			}
			collectTrailingArgs(cmd, opts, args)
			collectEnabledPatches(opts, patchFlags)
			return run(cmd.Context(), opts)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&opts.Input, "input", "i", "", "the app to be modified (.app/.ipa/.tipa)")
	f.StringVarP(&opts.Output, "output", "o", "", "output path (.app/.ipa/.tipa); defaults to overwriting the input")
	f.StringArrayVarP(&opts.Cyans, "cyan", "z", nil, ".cyan config file(s) to use (repeatable; values may be space-separated)")
	f.StringArrayVarP(&opts.Files, "file", "f", nil, "tweak to inject / item to add to the bundle (repeatable; values may be space-separated)")
	f.StringArrayVar(&opts.RootDylibs, "root-dylib", nil, "inject dylib to the app root with an @executable_path load command instead of Frameworks/@rpath — for dlopen-based tweaks like Regram that resolve resources relative to @executable_path (repeatable)")
	f.StringVarP(&opts.Name, "name", "n", "", "modify the app's name")
	f.StringVarP(&opts.Version, "app-version", "v", "", "modify the app's version")
	f.StringVarP(&opts.BundleID, "bundle-id", "b", "", "modify the app's bundle id")
	f.StringVarP(&opts.MinimumOS, "minimum-os", "m", "", "modify the app's minimum OS version")
	f.StringVarP(&opts.Icon, "icon", "k", "", "modify the app's icon (image file)")
	f.StringVarP(&opts.PlistMerge, "plist", "l", "", "a plist to merge with the app's Info.plist")
	f.StringVarP(&opts.Entitlements, "entitlements", "x", "", "add or modify entitlements on the main binary")
	f.BoolVarP(&opts.RemoveSupportedDevices, "remove-supported-devices", "u", false, "remove UISupportedDevices")
	f.BoolVarP(&opts.NoWatch, "no-watch", "w", false, "remove all watch apps")
	f.BoolVarP(&opts.EnableDocuments, "enable-documents", "d", false, "enable documents support")
	f.BoolVarP(&opts.Fakesign, "fakesign", "s", false, "fakesign all binaries (AppSync/TrollStore)")
	f.BoolVarP(&opts.Thin, "thin", "q", false, "thin all binaries to arm64")
	f.BoolVarP(&opts.RemoveExtensions, "remove-extensions", "e", false, "remove all app extensions")
	f.BoolVarP(&opts.RemoveEncrypted, "remove-encrypted", "g", false, "only remove encrypted app extensions")
	f.IntVarP(&opts.Compress, "compress", "c", 6, "ipa compression level (0-9, default 6)")
	f.BoolVar(&opts.IgnoreEncrypted, "ignore-encrypted", false, "skip the main binary encryption check")
	f.BoolVar(&opts.Overwrite, "overwrite", false, "overwrite existing files without confirming")
	f.BoolVar(&opts.Patch, "patch", false, "inject the bundled sideload dylib set (App Store/keychain repairs + bundled tweaks; implies --fakesign)")
	f.BoolVar(&opts.ElleKit, "ellekit", false, "use the real ElleKit runtime: rewrite all legacy hooking spellings to @rpath/ElleKit.framework/ElleKit and thin the framework to the app's architecture (feather-ellekit-spec.md D2)")
	// One bool flag per registered compatibility patch (spec D10). The
	// registry is stable-sorted, so flag order is deterministic.
	patchFlags = make(map[string]*bool, len(patch.Names()))
	for _, name := range patch.Names() {
		b := new(bool)
		patchFlags[name] = b
		f.BoolVar(b, name, false, "apply the "+name+" compatibility patch")
	}
	f.BoolVar(&silent, "silent", false, "silence everything but errors")
	// version is long-only so -v stays free for app-version.
	f.BoolVar(&showVersion, "version", false, "print xkvm version and exit")
	// Azule heritage — wired now, implemented in M4/M5.
	f.StringArrayVar(&opts.Fetch, "fetch", nil, "fetch tweak(s) by bundle id via Canister/MobileAPT (repeatable; values may be space-separated)")
	f.StringArrayVarP(&opts.APTSource, "apt-source", "A", nil, "extra APT repo URL(s) to fetch from (repeatable; values may be space-separated)")
	f.BoolVar(&opts.NoRecurse, "no-recurse", false, "don't install fetched package dependencies")
	f.StringArrayVar(&opts.Decrypt, "decrypt", nil, "iOS only: decrypt an App Store app (apple-id password; values may be space-separated)")
	f.StringVarP(&opts.Country, "country", "C", "", "country code for ipatool / iTunes lookup")

	cmd.AddCommand(newCGenCmd())
	cmd.AddCommand(newExtractCmd())
	cmd.AddCommand(newCyanCheckCmd())
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newDebifyCmd())
	cmd.AddCommand(newUndebCmd())
	cmd.AddCommand(newRootlessCmd())
	cmd.AddCommand(newRoothideCmd())
	return cmd
}

// collectEnabledPatches gathers the enabled per-patch flags into opts.Patches
// in registry (sorted) order, so patch.Apply runs deterministically.
func collectEnabledPatches(opts *app.Options, flags map[string]*bool) {
	for _, name := range patch.Names() {
		if b, ok := flags[name]; ok && *b {
			opts.Patches = append(opts.Patches, name)
		}
	}
}

// collectTrailingArgs folds positional args (possible only because cyan's
// nargs="+" flags accept space-separated values) into the last-specified
// array flag. Priority order mirrors argparse's greedy "last flag wins"
// semantics for the common single-array-flag invocations; with several array
// flags the last one parsed is ambiguous in pflag, so this is a documented
// approximation. Stray positional args therefore become -f values by default.
func collectTrailingArgs(cmd *cobra.Command, opts *app.Options, args []string) {
	if len(args) == 0 {
		return
	}
	switch {
	case cmd.Flags().Changed("fetch"):
		opts.Fetch = append(opts.Fetch, args...)
	case cmd.Flags().Changed("decrypt"):
		opts.Decrypt = append(opts.Decrypt, args...)
	case cmd.Flags().Changed("apt-source"):
		opts.APTSource = append(opts.APTSource, args...)
	case cmd.Flags().Changed("cyan"):
		opts.Cyans = append(opts.Cyans, args...)
	default:
		opts.Files = append(opts.Files, args...)
	}
}

// newCheckCmd is the merge-completeness gate: verify every bundle-relative
// load-command dependency in an app/ipa resolves inside the bundle. Catches
// the ffmpegkit-class gap — a tweak referencing @rpath/X.framework that the
// app doesn't ship. Exit code 1 when any reference is unresolved.
func newCheckCmd() *cobra.Command {
	var input string
	cmd := &cobra.Command{
		Use:   "check -i <app|ipa|tipa>",
		Short: "verify bundle-relative dependencies resolve (merge-completeness check)",
		Long: `check scans every Mach-O in an app/ipa/tipa for two classes of
unresolved bundle-relative reference:
  1. load-command dependencies (@rpath/, @executable_path/, @loader_path/)
     whose target is absent from the bundle, and
  2. bare NAME.framework strings in non-main binaries — the runtime-dlopen
     signature (e.g. RyukGram dlopening ffmpegkit.framework) whose framework
     the app doesn't ship reachably.
Frameworks in Frameworks/ and at the app root are reachable from any binary;
a framework nested inside a .bundle is reachable only from binaries of the
same tweak family. System frameworks and paths are ignored; dlopen findings
are reported as suspected. Exit code is 1 when any reference is unresolved.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.CheckBundle(input)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the app/ipa/tipa to check")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}

// newCyanCheckCmd validates .cyan config file(s) without applying them:
// payload/root_dylibs mismatches and everything else that would fail at apply
// time. Exit code 1 on any error-level finding, 0 with warnings only.
func newCyanCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cyan-check <file.cyan>...",
		Short: "validate .cyan config file(s) before applying them",
		Long: `cyan-check reads each .cyan archive (config.json + inject/ payloads) and
reports problems without extracting anything to disk: root_dylibs entries
with no matching inject/ payload, k/l/x file payloads the archive lacks,
unsafe payload paths, and unknown patch names. Warnings cover unknown
config keys (forward-compatible) and odd value types. Exit code is 1 when
any error-level finding exists, 0 when only warnings (or nothing) did.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			knownPatches := map[string]bool{}
			for _, n := range patch.Names() {
				knownPatches[n] = true
			}
			failed := 0
			for _, f := range args {
				issues, err := cyanfile.Validate(f, knownPatches)
				if err != nil {
					log.Errorf("cyan-check %s: %v", f, err)
					failed++
					continue
				}
				log.Infof("cyan-check %s", f)
				errs, warns := 0, 0
				for _, is := range issues {
					switch is.Level {
					case cyanfile.IssueError:
						errs++
						log.Errorf("error: %s", is.Message)
					default:
						warns++
						log.Warnf("warning: %s", is.Message)
					}
				}
				if errs == 0 {
					log.Infof("%d error(s), %d warning(s) — OK", errs, warns)
				} else {
					log.Infof("%d error(s), %d warning(s) — INVALID", errs, warns)
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("cyan-check: %d file(s) with errors", failed)
			}
			return nil
		},
	}
	return cmd
}

// newDebifyCmd wraps a tweak dylib (or payload directory) into a standard
// MobileSubstrate .deb (Dylib-to-Deb-Converter format): DEBIAN/control +
// Library/MobileSubstrate/DynamicLibraries/{name}.dylib + filter plist.
func newDebifyCmd() *cobra.Command {
	var (
		input, output, name, version, maintainer, author, description, filter string
		bundleIDs, resources, depends                                         []string
	)
	cmd := &cobra.Command{
		Use:   "debify -i <tweak.dylib|dir> -o <out.deb>",
		Short: "build a MobileSubstrate .deb from a dylib (or payload dir)",
		Long: `debify wraps a tweak dylib into a standard rootful .deb, matching the
Dylib-to-Deb-Converter format: the dylib lands in
Library/MobileSubstrate/DynamicLibraries/ and an optional Filter/Bundles
plist is generated from --bundle-id. An input directory is treated as a
complete payload root and copied verbatim; --resource adds extra payload
files as src:dest pairs. Depends defaults to mobilesubstrate; --depends
replaces it. Use 'xkvm rootless' to convert the result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("required flag(s) \"output\" not set")
			}
			return app.Debify(app.DebifyOptions{
				Input:       input,
				Output:      output,
				Name:        name,
				Version:     version,
				Maintainer:  maintainer,
				Author:      author,
				Description: description,
				BundleIDs:   bundleIDs,
				Filter:      filter,
				Resources:   resources,
				Depends:     depends,
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the tweak .dylib or payload directory")
	f.StringVarP(&output, "output", "o", "", "output .deb path")
	f.StringVar(&name, "name", "", "package/display name (default: input basename)")
	f.StringVar(&version, "version", "", "package version (default 1.0)")
	f.StringVar(&maintainer, "maintainer", "", "Maintainer field (default xkvm)")
	f.StringVar(&author, "author", "", "Author field (default xkvm)")
	f.StringVar(&description, "description", "", "Description field")
	f.StringArrayVar(&bundleIDs, "bundle-id", nil, "target app bundle id(s) for the Filter/Bundles plist (repeatable)")
	f.StringVar(&filter, "filter", "", "exact filter plist to ship instead of a generated one")
	f.StringArrayVar(&resources, "resource", nil, "extra payload file as src:dest (repeatable)")
	f.StringArrayVar(&depends, "depends", nil, "Depends entr(ies); replaces the default mobilesubstrate (repeatable)")
	_ = cmd.MarkFlagRequired("input")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// newUndebCmd extracts a tweak .deb and dumps its injectable artifacts
// (dylib/framework/bundle) with a placement manifest — the Forte (deb→dylib)
// equivalent, tool-native.
func newUndebCmd() *cobra.Command {
	var input, output string
	cmd := &cobra.Command{
		Use:   "undeb -i <tweak.deb> -o <dir>",
		Short: "extract tweak artifacts (dylibs, bundles) from a .deb",
		Long: `undeb unpacks a tweak .deb and copies its injectable artifacts (dylibs,
frameworks, bundles) into -o, writing the same xkvm-manifest.json sidecar
as 'xkvm extract' so re-injection restores original placements. This is the
deb→dylib direction of the Dylib-to-Deb-Converter / Forte workflow.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Undeb(input, output)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the .deb to extract from")
	f.StringVarP(&output, "output", "o", "", "directory to write artifacts to")
	_ = cmd.MarkFlagRequired("input")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// newRootlessCmd converts a rootful .deb to a rootless one: payload repacked
// under var/jb, control edits (iphoneos-arm64 + the rootless runtime
// dependency), and Mach-O load-command paths rewritten under /var/jb
// (rootless-patcher port; load-command layer only — see ARCHITECTURE.md).
func newRootlessCmd() *cobra.Command {
	var input, output string
	var thin, tweakinject bool
	cmd := &cobra.Command{
		Use:   "rootless -i <rootful.deb> -o <rootless.deb> [--thin] [--tweakinject]",
		Short: "convert a rootful .deb to rootless",
		Long: `rootless converts a rootful jailbreak .deb to a rootless one, porting the
rootless-patcher pipeline: the payload is repacked under var/jb, the control
file gains iphoneos-arm64 + the rootless runtime dependency
(cy+cpu.arm64v8 | oldabi-xina | oldabi), and Mach-O load-command dylib
paths whose first component is a bootstrap root (/Library, /usr, ...) are
rewritten under /var/jb honoring the ConversionRuleset blacklist. Every
converted Mach-O is re-signed like Derootifier's ldid step: executables get
the roothide platform entitlements merged with any they already carried,
other Mach-Os get a plain ad-hoc signature (pure-Go, Apple-format valid).
--thin thins every Mach-O to arm64 (best-effort). Runtime dlopen strings
compiled into __TEXT (CFString/data pointers) are NOT rewritten — the
load-command layer is the supported boundary. Already-rootless packages
are rebuilt unchanged.

--tweakinject applies the modern Dopamine/ellekit conventions (ported from
Derootifier): DynamicLibraries moves to usr/lib/TweakInject, CydiaSubstrate
deps become @rpath/libsubstrate.dylib (the ellekit substrate shim), install
names become @rpath/<basename>, and the /usr/lib + /var/jb/usr/lib rpaths
are added.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Rootless(input, output, thin, tweakinject)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the rootful .deb to convert")
	f.StringVarP(&output, "output", "o", "", "output .deb path")
	f.BoolVar(&thin, "thin", false, "thin every Mach-O to arm64 (best-effort)")
	f.BoolVar(&tweakinject, "tweakinject", false, "emit the modern TweakInject layout + @rpath/libsubstrate.dylib shim (Derootifier conventions)")
	_ = cmd.MarkFlagRequired("input")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// newRoothideCmd converts a rootless .deb to a roothide-jailbreak one:
// var/jb payload hoisted to the package root, system files under rootfs/,
// /var/jb → @loader_path/.jbroot load-command and rpath rewrites, and
// iphoneos-arm64e control edits (RootHidePatcher port; GPL semantics
// reference only — see NOTICE).
func newRoothideCmd() *cobra.Command {
	var input, output string
	var pkgmirror bool
	var mode string
	cmd := &cobra.Command{
		Use:   "roothide -i <rootless.deb> -o <roothide.deb> [--pkgmirror] [--mode auto|dynamic]",
		Short: "convert a rootless .deb to a roothide-jailbreak one",
		Long: `roothide converts a rootless jailbreak .deb (var/jb payload) to a
roothide-jailbreak package, porting RootHidePatcher's patch.sh: the
var/jb payload is hoisted to the package root, remaining system files move
under rootfs/, every /var/jb/... load-command dependency and LC_RPATH is
rewritten to @loader_path/.jbroot/..., the control file becomes
iphoneos-arm64e, and preinst/prerm/postinst/postrm/extrainst_ scripts plus
LaunchDaemons and libSandy plists get the same path translations. Every
patched Mach-O is re-signed like upstream's ldid step: executables get the
roothide platform entitlements merged with any they already carried, other
Mach-Os get a plain ad-hoc signature (pure-Go, Apple-format valid).
A fixed-paths warning reports surviving /var/jb strings in __cstring.

--pkgmirror mirrors the package to var/mobile/Library/pkgmirror with the
control dir renamed DEBIAN.<pkg> for roothide's package manager. --mode
controls the Pre-Depends/version edits: default adds none; auto adds
rootless-compat(>= 0.9); dynamic adds a ~roothide version suffix and a
patches-<pkg>(= <ver>~roothide) Pre-Depends.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.Roothide(input, output, pkgmirror, mode)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the rootless .deb to convert")
	f.StringVarP(&output, "output", "o", "", "output .deb path")
	f.BoolVar(&pkgmirror, "pkgmirror", false, "mirror the package to var/mobile/Library/pkgmirror")
	f.StringVar(&mode, "mode", "", "roothide control edit mode: auto or dynamic (default: none)")
	_ = cmd.MarkFlagRequired("input")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// newExtractCmd dumps the injectable artifacts (dylib/framework/appex/bundle)
// from an app bundle into an output directory.
func newExtractCmd() *cobra.Command {
	var input, output string
	cmd := &cobra.Command{
		Use:   "extract -i <app> -o <dir>",
		Short: "extract tweaks (dylibs, frameworks, bundles, appex) from an app",
		Long:  "extract unpacks an .ipa/.tipa (or reads an .app) and copies the injected tweak artifacts it contains into -o.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.ExtractArtifacts(input, output)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&input, "input", "i", "", "the app to extract from (.app/.ipa/.tipa)")
	f.StringVarP(&output, "output", "o", "", "directory to write artifacts to")
	_ = cmd.MarkFlagRequired("input")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// newCGenCmd is the .cyan config generator (cyan/pyzule-rw cgen parity,
// plus the xkvm --root-dylib extension).
func newCGenCmd() *cobra.Command {
	var (
		output     string
		files      []string
		rootDylibs []string
		name       string
		fakesign   bool
		ellekit    bool
		patches    []string
	)
	cmd := &cobra.Command{
		Use:   "cgen -o <out.cyan> [-f tweak ...] [--root-dylib dylib ...]",
		Short: "generate a shareable .cyan config file",
		Long: `cgen generates .cyan config files for reproducible IPA patching,
matching the upstream cyan/pyzule-rw config format (config.json + inject/
payloads). xkvm extension: --root-dylib marks an inject payload for the
app-root @executable_path contract (dlopen-based tweaks like Regram).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return fmt.Errorf("required flag(s) \"output\" not set")
			}
			return cyanfile.Generate(cyanfile.GenerateOptions{
				Output:     output,
				Files:      files,
				RootDylibs: rootDylibs,
				Name:       name,
				Fakesign:   fakesign,
				ElleKit:    ellekit,
				Patches:    patches,
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "", "output .cyan file")
	f.StringArrayVarP(&files, "file", "f", nil, "tweak to inject / item to add (repeatable; payloads ship in inject/)")
	f.StringArrayVar(&rootDylibs, "root-dylib", nil, "mark an injected dylib for the app root @executable_path contract (must also be in -f; repeatable)")
	f.StringVarP(&name, "name", "n", "", "app name to bake into the config")
	f.BoolVarP(&fakesign, "fakesign", "s", false, "bake fakesign into the config")
	f.BoolVar(&ellekit, "ellekit", false, "bake the ElleKit runtime into the config")
	f.StringArrayVar(&patches, "patch", nil, "bake compatibility patch name(s) into the config (repeatable)")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}
