// Package extras embeds the hooking frameworks xkvm auto-injects (CydiaSubstrate,
// Orion, Cephei, CepheiUI, CepheiPrefs, ElleKit) and the sideload-fix dylibs
// --patch injects — the same set cyan ships in its extras/ directory. Framework binaries are GPL-family
// (Cephei) / permissive (CydiaSubstrate, Orion, ElleKit); the sideload dylibs are
// third-party binaries of unknown license (see sideload.go). Provenance and
// license details for every embedded component live in the repo-level NOTICE.
package extras

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:extras
var fsys embed.FS

// HasFramework reports whether an embedded framework (e.g.
// "CydiaSubstrate.framework") exists.
func HasFramework(name string) bool {
	_, err := fs.Stat(fsys, "extras/"+name)
	return err == nil
}

// CopyFramework materializes an embedded framework directory into destDir
// (destDir/CydiaSubstrate.framework/...).
func CopyFramework(name, destDir string) error {
	if !HasFramework(name) {
		return fmt.Errorf("embedded framework %s not found", name)
	}
	src := "extras/" + name
	return fs.WalkDir(fsys, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, name, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fsys.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
