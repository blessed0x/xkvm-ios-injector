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
