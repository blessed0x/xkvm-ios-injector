package cyanfile

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildCyan assembles an in-memory .cyan archive for tests.
func buildCyan(t testing.TB, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FuzzParseNoPanicNoEscape smashes the .cyan parser with arbitrary archives
// (configs are shared between users, so they are hostile input by
// definition): Parse and Validate must never panic, and every file the
// parse materializes must stay inside its outDir.
func FuzzParseNoPanicNoEscape(f *testing.F) {
	f.Add(buildCyan(f, map[string]string{
		"config.json":    `{"n":"Renamed","f":["inject/t.dylib"]}`,
		"inject/t.dylib": "mach-o",
	}))
	f.Add(buildCyan(f, map[string]string{"config.json": `not json at all`}))
	f.Add(buildCyan(f, map[string]string{"other.txt": "no config here"}))
	f.Add(buildCyan(f, map[string]string{
		"config.json":          `{"x":"ent.plist"}`,
		"inject/../escape.txt": "slip",
	}))
	f.Add([]byte("plainly not a zip"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "hostile.cyan")
		if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		outDir := filepath.Join(dir, "out")
		if _, err := Parse(cfgPath, outDir); err != nil {
			return // clean rejection is a pass
		}
		_, _ = Validate(cfgPath, map[string]bool{"liquid-glass": true})
		// Nothing outside outDir may have been created.
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, path)
			if rel == "hostile.cyan" || rel == "." || rel == "out"+string(filepath.Separator) ||
				strings.HasPrefix(rel, "out") {
				return nil
			}
			t.Fatalf("parse escaped outDir: %q", path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
