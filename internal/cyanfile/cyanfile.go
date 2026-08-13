// Package cyanfile parses and generates .cyan config archives (the
// cyan/pyzule-rw shareable-patch format: a zip with config.json + inject/
// payloads + optional icon.idk / merge.plist / new.entitlements).
//
// Format (confirmed from upstream pyzule-rw cgen/__main__.py and
// tbhutils.parse_cyans): config.json is a flat dict of CLI flag values, where
// file-valued flags (-f/-k/-l/-x) are stored as `true` and their payload
// files live in the archive (inject/, icon.idk, merge.plist,
// new.entitlements). When applied, the inject payloads APPEND to the -f file
// set (cyan merges them by basename, config wins on collision) and every
// other config key OVERRIDES the corresponding CLI arg.
//
// xkvm extension: a `root_dylibs` array key lists inject payload basenames
// that must be placed in the app root with an @executable_path load command
// (the --root-dylib contract for dlopen-based tweaks like Regram). The values
// are basenames, matching how inject/ payloads are referenced.
package cyanfile

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the parsed config.json values plus the on-disk paths of the
// payloads materialized into outDir.
type Config struct {
	// Files are the absolute paths of the inject/ payloads, in archive order.
	Files []string
	// RootDylibs are the absolute paths (within Files) that must be placed in
	// the app root with an @executable_path load command.
	RootDylibs []string

	Icon        string // extracted icon.idk path, "" if none
	PlistMerge  string // extracted merge.plist path, "" if none
	Entitlement string // extracted new.entitlements path, "" if none

	Name       string
	Version    string
	BundleID   string
	MinimumOS  string
	Compress   int
	Fakesign   bool
	Thin       bool
	ElleKit    bool
	RemoveExts bool
	RemoveEnc  bool
	NoWatch    bool
	EnableDocs bool
	RemoveDevs bool
	IgnoreEnc  bool
	Overwrite  bool
	Patches    []string // enabled compatibility-patch names
}

// Parse reads a .cyan archive, extracts its payloads into outDir, and returns
// the merged config. outDir is created if needed. Unknown config keys are
// ignored (forward-compatible: a config made by a newer xkvm still applies to
// an older one for the keys it knows).
func Parse(path, outDir string) (*Config, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer zr.Close()

	var raw map[string]json.RawMessage
	found := false
	for _, f := range zr.File {
		if f.Name == "config.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("reading config.json: %w", err)
			}
			dec := json.NewDecoder(rc)
			if err := dec.Decode(&raw); err != nil {
				rc.Close()
				return nil, fmt.Errorf("parsing config.json: %w", err)
			}
			rc.Close()
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%s: no config.json in archive", path)
	}

	cfg := &Config{}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// Inject payloads: every archive entry under inject/ is materialized and
	// appended to Files (cyan merges by basename with the -f set; config wins
	// on collision, so callers must append AFTER the CLI files).
	injectDir := filepath.Join(outDir, "inject")
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "inject/") {
			continue
		}
		// Zip-slip guard: the path after inject/ must stay inside inject/.
		rel := strings.TrimPrefix(f.Name, "inject/")
		if rel == "" || filepath.Base(rel) != rel || rel == ".." || strings.Contains(rel, "..") {
			return nil, fmt.Errorf("%s: unsafe inject payload path %q", path, f.Name)
		}
		dst := filepath.Join(injectDir, rel)
		if err := extractFile(f, dst); err != nil {
			return nil, err
		}
		cfg.Files = append(cfg.Files, dst)
	}

	// root_dylibs: basenames of inject payloads that use the app-root
	// @executable_path contract. Resolve to the materialized paths.
	if v, ok := raw["root_dylibs"]; ok {
		var names []string
		if err := json.Unmarshal(v, &names); err != nil {
			return nil, fmt.Errorf("root_dylibs: %w", err)
		}
		byName := map[string]string{}
		for _, f := range cfg.Files {
			byName[filepath.Base(f)] = f
		}
		for _, n := range names {
			p, ok := byName[n]
			if !ok {
				return nil, fmt.Errorf("root_dylibs: %q is not an inject payload", n)
			}
			cfg.RootDylibs = append(cfg.RootDylibs, p)
		}
	}

	// Payload files for the other file-valued flags.
	if p, e := extractNamed(zr, outDir, "icon.idk", raw, "k"); e != nil {
		return nil, e
	} else {
		cfg.Icon = p
	}
	if p, e := extractNamed(zr, outDir, "merge.plist", raw, "l"); e != nil {
		return nil, e
	} else {
		cfg.PlistMerge = p
	}
	if p, e := extractNamed(zr, outDir, "new.entitlements", raw, "x"); e != nil {
		return nil, e
	} else {
		cfg.Entitlement = p
	}

	// Remaining scalar keys override the CLI (upstream: `args[k] = v`).
	if v, ok := raw["n"]; ok {
		_ = json.Unmarshal(v, &cfg.Name)
	}
	if v, ok := raw["v"]; ok {
		_ = json.Unmarshal(v, &cfg.Version)
	}
	if v, ok := raw["b"]; ok {
		_ = json.Unmarshal(v, &cfg.BundleID)
	}
	if v, ok := raw["m"]; ok {
		_ = json.Unmarshal(v, &cfg.MinimumOS)
	}
	if v, ok := raw["c"]; ok {
		_ = json.Unmarshal(v, &cfg.Compress)
	}
	for key, dst := range map[string]*bool{
		"s": &cfg.Fakesign, "q": &cfg.Thin, "e": &cfg.RemoveExts,
		"g": &cfg.RemoveEnc, "w": &cfg.NoWatch, "d": &cfg.EnableDocs,
		"u": &cfg.RemoveDevs, "ellekit": &cfg.ElleKit,
		"ignore-encrypted": &cfg.IgnoreEnc, "overwrite": &cfg.Overwrite,
	} {
		if v, ok := raw[key]; ok {
			var b bool
			if err := json.Unmarshal(v, &b); err == nil {
				*dst = b
			}
		}
	}
	if v, ok := raw["patches"]; ok {
		var names []string
		if err := json.Unmarshal(v, &names); err == nil {
			cfg.Patches = names
		}
	}
	// Entitlement doubles as a file payload and a bool flag: "x" as a bool
	// means new.entitlements was bundled (already extracted above); it is not
	// a plain enable flag, so do not flip it.
	if v, ok := raw["x"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil && b {
			cfg.Entitlement = filepath.Join(outDir, "new.entitlements")
		}
	}

	return cfg, nil
}

// extractNamed extracts a fixed-name payload (icon.idk / merge.plist /
// new.entitlements) when the corresponding config key is present, returning
// the on-disk path ("" when the key is absent).
func extractNamed(zr *zip.ReadCloser, outDir, archiveName string, raw map[string]json.RawMessage, key string) (string, error) {
	if _, ok := raw[key]; !ok {
		return "", nil
	}
	for _, f := range zr.File {
		if f.Name == archiveName {
			dst := filepath.Join(outDir, archiveName)
			if err := extractFile(f, dst); err != nil {
				return "", err
			}
			return dst, nil
		}
	}
	return "", fmt.Errorf("config has %q but the archive lacks %s", key, archiveName)
}

func extractFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, rc)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// GenerateOptions mirrors the cgen CLI surface.
type GenerateOptions struct {
	Output     string   // .cyan path to write
	Files      []string // tweaks/items to bundle into inject/
	RootDylibs []string // subset of Files to mark app-root (@executable_path)
	Name       string   // app name override (-n)
	Fakesign   bool     // -s
	ElleKit    bool     // --ellekit
	Patches    []string // compatibility-patch names
}

// Generate writes a .cyan archive at opts.Output: config.json plus the
// inject/ payloads. RootDylibs entries must also be in Files (same basename);
// they are recorded in config.json as basenames, matching how inject/
// payloads are referenced. The config.json flag shape matches upstream cgen:
// file-valued flags are stored as true.
func Generate(opts GenerateOptions) error {
	if opts.Output == "" {
		return fmt.Errorf("output path is required")
	}
	if !strings.HasSuffix(opts.Output, ".cyan") {
		opts.Output += ".cyan"
	}
	for _, f := range opts.Files {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("%s does not exist", f)
		}
	}
	rootNames := map[string]bool{}
	for _, r := range opts.RootDylibs {
		bn := filepath.Base(r)
		rootNames[bn] = true
		found := false
		for _, f := range opts.Files {
			if filepath.Base(f) == bn {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("--root-dylib %s must also be listed in -f (the payload must ship in inject/)", bn)
		}
	}
	// Dedup Files by basename (last wins, cyan dict semantics).
	var files []string
	seen := map[string]int{}
	for _, f := range opts.Files {
		bn := filepath.Base(f)
		if idx, ok := seen[bn]; ok {
			files[idx] = f
			continue
		}
		seen[bn] = len(files)
		files = append(files, f)
	}

	config := map[string]any{}
	if len(files) > 0 {
		config["f"] = true
	}
	if len(rootNames) > 0 {
		names := make([]string, 0, len(rootNames))
		for _, f := range files {
			if rootNames[filepath.Base(f)] {
				names = append(names, filepath.Base(f))
			}
		}
		config["root_dylibs"] = names
	}
	if opts.Name != "" {
		config["n"] = opts.Name
	}
	if opts.Fakesign {
		config["s"] = true
	}
	if opts.ElleKit {
		config["ellekit"] = true
	}
	if len(opts.Patches) > 0 {
		config["patches"] = opts.Patches
	}

	zf, err := os.Create(opts.Output)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)
	if err := writeJSONEntry(zw, "config.json", config); err != nil {
		zf.Close()
		return err
	}
	for _, f := range files {
		if err := writeFileEntry(zw, "inject/"+filepath.Base(f), f); err != nil {
			zf.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		zf.Close()
		return err
	}
	return zf.Close()
}

func writeJSONEntry(zw *zip.Writer, name string, v any) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(v)
}

func writeFileEntry(zw *zip.Writer, name, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}
