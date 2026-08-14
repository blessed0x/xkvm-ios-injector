// Package app implements the xkvm processing pipeline.
package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Version is the xkvm release version.
const Version = "0.1.0-dev"

// Options captures the full xkvm CLI surface. Flag names, shorthands and
// semantics are cyan-compatible; Azule-exclusive features use long flags
// (see ARCHITECTURE.md §5 for the collision-resolution decisions).
type Options struct {
	Input  string
	Output string

	Cyans []string
	Files []string

	// RootDylibs lists dylibs that must land in the app root (not
	// Frameworks/) with an @executable_path load command instead of @rpath.
	// dlopen-based tweaks (e.g. Regram) resolve resources relative to
	// @executable_path and crash when relocated to Frameworks.
	RootDylibs []string

	Name         string
	Version      string
	BundleID     string
	MinimumOS    string
	Icon         string
	PlistMerge   string
	Entitlements string

	RemoveSupportedDevices bool
	NoWatch                bool
	EnableDocuments        bool
	Fakesign               bool
	Thin                   bool
	RemoveExtensions       bool
	RemoveEncrypted        bool
	Compress               int
	Patch                  bool     // inject the bundled sideload-fix dylibs
	ElleKit                bool     // use the real ElleKit.framework runtime (feather-ellekit-spec.md D2)
	Patches                []string // enabled compatibility-patch names (spec D10)

	IgnoreEncrypted bool
	Overwrite       bool

	// Azule heritage — flags are wired now, implementations land in M4/M5.
	Fetch     []string // tweak bundle ids to fetch via Canister/MobileAPT
	APTSource []string // extra APT repo URLs
	NoRecurse bool     // skip dependency recursion when fetching
	Decrypt   []string // [apple-id, password] for iOS App Store decrypt
	Country   string   // country code for ipatool / iTunes lookup
}

// validate checks option sanity before any work happens. It mirrors the
// checks in cyan's tbhutils.validate_inputs.
func (o *Options) validate() error {
	// Users paste paths from browsers/Finder with %20 (and other %-escapes)
	// for spaces and special chars. Decode them before any stat or suffix
	// check, so an encoded path behaves like the literal one. Best-effort:
	// an invalid escape is left alone rather than erroring.
	o.Input = decodeURLPath(o.Input)
	o.Output = decodeURLPath(o.Output)
	o.Files = decodeAll(o.Files)
	o.RootDylibs = decodeAll(o.RootDylibs)
	o.Cyans = decodeAll(o.Cyans)
	o.Icon = decodeURLPath(o.Icon)
	o.PlistMerge = decodeURLPath(o.PlistMerge)
	o.Entitlements = decodeURLPath(o.Entitlements)

	if !strings.HasSuffix(o.Input, ".app") &&
		!strings.HasSuffix(o.Input, ".ipa") &&
		!strings.HasSuffix(o.Input, ".tipa") {
		return fmt.Errorf("the input file must be an ipa/tipa/app")
	}
	if _, err := os.Stat(o.Input); err != nil {
		return fmt.Errorf("%s does not exist", o.Input)
	}

	if o.Output != "" {
		if !strings.HasSuffix(o.Output, ".app") &&
			!strings.HasSuffix(o.Output, ".ipa") &&
			!strings.HasSuffix(o.Output, ".tipa") {
			o.Output += ".ipa"
		}
	} else {
		o.Output = o.Input // overwrite the input unless --overwrite
	}

	if o.Compress < 0 || o.Compress > 9 {
		return fmt.Errorf("invalid compression level: %d (0-9)", o.Compress)
	}
	if o.MinimumOS != "" {
		for _, r := range o.MinimumOS {
			if !strings.ContainsRune("0123456789.", r) {
				return fmt.Errorf("invalid OS version: %s", o.MinimumOS)
			}
		}
	}

	// -f entries are local paths; --fetch ids are resolved separately in Run
	// (M4), so this stat check stays unconditional here.
	// Normalize like cyan: strip a trailing "/" and dedup by basename
	// (last wins, matching cyan's dict-overwrite semantics). M1's injection
	// loop keys on basenames for @rpath/{bn} naming, so this must happen now.
	var files []string
	filesIdx := make(map[string]int) // basename -> index into files (last wins)
	for _, f := range o.Files {
		f = strings.TrimSuffix(f, "/")
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("%q does not exist", f)
		}
		if idx, ok := filesIdx[filepath.Base(f)]; ok {
			files[idx] = f // overwrite: cyan's dict semantics (last wins)
			continue
		}
		filesIdx[filepath.Base(f)] = len(files)
		files = append(files, f)
	}
	o.Files = files

	// --root-dylib values are plain local .dylib paths, deduped by basename
	// (last wins, same semantics as -f). Each must exist and be a dylib; a
	// root-placed non-dylib would silently fall through to the default copy
	// path, so reject it up front.
	var roots []string
	rootsIdx := make(map[string]int)
	for _, f := range o.RootDylibs {
		f = strings.TrimSuffix(f, "/")
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("%q does not exist", f)
		}
		if !strings.HasSuffix(f, ".dylib") {
			return fmt.Errorf("--root-dylib %q is not a .dylib", f)
		}
		if idx, ok := rootsIdx[filepath.Base(f)]; ok {
			roots[idx] = f // overwrite: last wins
			continue
		}
		rootsIdx[filepath.Base(f)] = len(roots)
		roots = append(roots, f)
	}
	o.RootDylibs = roots
	for _, c := range o.Cyans {
		if !isRegularFile(c) {
			return fmt.Errorf("%s does not exist", c)
		}
	}
	for label, p := range map[string]string{
		"icon":         o.Icon,
		"plist":        o.PlistMerge,
		"entitlements": o.Entitlements,
	} {
		if p != "" && !isRegularFile(p) {
			return fmt.Errorf("%s: %s does not exist", label, p)
		}
		// TODO(M1): cyan also parses the entitlements file as a plist and
		// exits on failure ("couldn't parse given entitlements file");
		// add that check when howett.net/plist lands.
	}

	if len(o.Decrypt) > 0 && len(o.Decrypt) != 2 {
		return fmt.Errorf("--decrypt expects an apple id and password")
	}

	// --liquid-glass and --liquid-glass-compat are mutually exclusive: the
	// former opts the app into the iOS 26 Liquid Glass design, the latter
	// forces the legacy appearance (feather-ellekit-spec.md §4.6).
	if hasPatch(o.Patches, "liquid-glass") && hasPatch(o.Patches, "liquid-glass-compat") {
		return fmt.Errorf("--liquid-glass and --liquid-glass-compat are mutually exclusive")
	}

	return nil
}

// decodeURLPath decodes %XX URL escapes in a path (a space in a pasted
// path is usually %20). On any malformed escape it returns the input
// unchanged — a literal % is far more likely to be a real filename than a
// typo to punish.
func decodeURLPath(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	dec, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return dec
}

// decodeAll applies decodeURLPath to every element of a slice.
func decodeAll(ps []string) []string {
	for i, p := range ps {
		ps[i] = decodeURLPath(p)
	}
	return ps
}

func hasPatch(patches []string, name string) bool {
	for _, p := range patches {
		if p == name {
			return true
		}
	}
	return false
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
