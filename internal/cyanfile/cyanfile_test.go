package cyanfile

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeArchive builds a .cyan zip with the given config.json bytes and extra
// archive entries (name → content) for the payloads.
func writeArchive(t *testing.T, path string, config string, entries map[string]string) {
	t.Helper()
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestGenerateParseRoundTrip exercises the full cycle: cgen generates a
// config marking a payload as a root dylib; Parse must recover the payload
// file, its app-root placement, and the baked scalars.
func TestGenerateParseRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	tweak := writeTemp(t, tmp, "Regram.dylib", "\xcf\xfa\xed\xfe fake-dylib")
	other := writeTemp(t, tmp, "Other.dylib", "other")

	out := filepath.Join(tmp, "patch.cyan")
	if err := Generate(GenerateOptions{
		Output:     out,
		Files:      []string{tweak, other},
		RootDylibs: []string{tweak},
		Name:       "PatchedApp",
		Fakesign:   true,
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg, err := Parse(out, filepath.Join(tmp, "out"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfg.Files) != 2 {
		t.Fatalf("Files = %d, want 2", len(cfg.Files))
	}
	if filepath.Base(cfg.Files[0]) != "Regram.dylib" || filepath.Base(cfg.Files[1]) != "Other.dylib" {
		t.Errorf("Files order/names wrong: %v", cfg.Files)
	}
	// The root dylib resolves to the materialized inject payload path.
	if len(cfg.RootDylibs) != 1 || filepath.Base(cfg.RootDylibs[0]) != "Regram.dylib" {
		t.Errorf("RootDylibs = %v, want [Regram.dylib]", cfg.RootDylibs)
	}
	if cfg.Name != "PatchedApp" {
		t.Errorf("Name = %q, want PatchedApp", cfg.Name)
	}
	if !cfg.Fakesign {
		t.Error("Fakesign not parsed from config")
	}
}

// TestParseHandWrittenConfig verifies a hand-authored config.json (the
// documented format: file flags stored as true, payloads in the archive)
// parses, including the root_dylibs basename mapping and the bundled
// icon/plist/entitlements payloads.
func TestParseHandWrittenConfig(t *testing.T) {
	tmp := t.TempDir()
	cyan := filepath.Join(tmp, "hand.cyan")
	writeArchive(t, cyan, `{
		"f": true,
		"root_dylibs": ["Regram.dylib"],
		"k": true, "l": true, "x": true,
		"n": "HandApp", "s": true, "c": 3
	}`, map[string]string{
		"inject/Regram.dylib": "regram-bytes",
		"inject/Other.dylib":  "other-bytes",
		"icon.idk":            "icon",
		"merge.plist":         "plist",
		"new.entitlements":    "ents",
	})

	cfg, err := Parse(cyan, filepath.Join(tmp, "out"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfg.Files) != 2 {
		t.Fatalf("Files = %d, want 2 (inject payloads)", len(cfg.Files))
	}
	if len(cfg.RootDylibs) != 1 || filepath.Base(cfg.RootDylibs[0]) != "Regram.dylib" {
		t.Errorf("RootDylibs = %v, want [Regram.dylib]", cfg.RootDylibs)
	}
	if cfg.Icon == "" || cfg.PlistMerge == "" || cfg.Entitlement == "" {
		t.Errorf("bundled payloads not extracted: icon=%q plist=%q ents=%q", cfg.Icon, cfg.PlistMerge, cfg.Entitlement)
	}
	if cfg.Name != "HandApp" || !cfg.Fakesign || cfg.Compress != 3 {
		t.Errorf("scalars wrong: name=%q fakesign=%v compress=%d", cfg.Name, cfg.Fakesign, cfg.Compress)
	}
}

// TestParseRejectsZipSlip ensures an inject/ payload path escaping the
// archive root is rejected rather than written outside the output dir.
func TestParseRejectsZipSlip(t *testing.T) {
	tmp := t.TempDir()
	cyan := filepath.Join(tmp, "evil.cyan")
	writeArchive(t, cyan, `{"f": true}`, map[string]string{
		"inject/../../evil.dylib": "evil",
	})
	if _, err := Parse(cyan, filepath.Join(tmp, "out")); err == nil {
		t.Fatal("expected zip-slip payload to be rejected")
	}
	if _, err := os.Stat(filepath.Join(tmp, "evil.dylib")); err == nil {
		t.Fatal("zip-slip payload escaped the output dir")
	}
}

// TestParseRejectsUnknownRootDylib: a root_dylibs basename with no matching
// inject payload is a config error, not silently ignored.
func TestParseRejectsUnknownRootDylib(t *testing.T) {
	tmp := t.TempDir()
	cyan := filepath.Join(tmp, "bad.cyan")
	writeArchive(t, cyan, `{"f": true, "root_dylibs": ["Nope.dylib"]}`, map[string]string{
		"inject/Other.dylib": "other",
	})
	if _, err := Parse(cyan, filepath.Join(tmp, "out")); err == nil {
		t.Fatal("expected unknown root_dylibs basename to be rejected")
	}
}

// TestParseMissingConfigJson: an archive without config.json is an error.
func TestParseMissingConfigJson(t *testing.T) {
	tmp := t.TempDir()
	cyan := filepath.Join(tmp, "empty.cyan")
	writeArchive(t, cyan, "", map[string]string{"inject/x.dylib": "x"})
	if _, err := Parse(cyan, filepath.Join(tmp, "out")); err == nil {
		t.Fatal("expected missing config.json to be rejected")
	}
}

// TestGenerateRejectsRootDylibNotInFiles: --root-dylib entries must ship in
// the inject payload set (their basename must exist in Files).
func TestGenerateRejectsRootDylibNotInFiles(t *testing.T) {
	tmp := t.TempDir()
	other := writeTemp(t, tmp, "Other.dylib", "other")
	if err := Generate(GenerateOptions{
		Output:     filepath.Join(tmp, "x.cyan"),
		Files:      []string{other},
		RootDylibs: []string{filepath.Join(tmp, "Missing.dylib")},
	}); err == nil {
		t.Fatal("expected root-dylib not in -f to be rejected")
	}
}

// TestParseIgnoresUnknownKeys: forward compatibility — a config from a newer
// xkvm with keys this version doesn't know still parses the known keys.
func TestParseIgnoresUnknownKeys(t *testing.T) {
	tmp := t.TempDir()
	cyan := filepath.Join(tmp, "future.cyan")
	writeArchive(t, cyan, `{"f": true, "future_feature": {"a": 1}, "n": "App"}`, map[string]string{
		"inject/x.dylib": "x",
	})
	cfg, err := Parse(cyan, filepath.Join(tmp, "out"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Name != "App" || len(cfg.Files) != 1 {
		t.Errorf("known keys not applied: name=%q files=%d", cfg.Name, len(cfg.Files))
	}
}

// TestConfigScalarFields sanity-checks the Config struct shape so a future
// change to the field set is caught early.
func TestConfigScalarFields(t *testing.T) {
	cfg := &Config{
		RootDylibs: []string{"/a.dylib"},
		Patches:    []string{"liquid-glass"},
		ElleKit:    true,
	}
	wantFiles := []string{"/a.dylib"}
	if !reflect.DeepEqual(cfg.RootDylibs, wantFiles) {
		t.Errorf("RootDylibs = %v", cfg.RootDylibs)
	}
	if len(cfg.Patches) != 1 || !cfg.ElleKit {
		t.Errorf("patches=%v ellekit=%v", cfg.Patches, cfg.ElleKit)
	}
}
