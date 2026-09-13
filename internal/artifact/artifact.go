// Package artifact collects injectable file types from a directory tree:
// *.dylib files and *.appex / *.framework / *.bundle directories, following
// cyan's extract_deb conventions. It is used both for .deb payloads and for
// dumping tweaks from an existing app bundle (xkvm extract).
package artifact

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Collect returns the artifact paths under root, excluding symlinks and
// nested bundles (paths with more than one ".bundle"/".framework" component).
// Descending into a matched framework/bundle/appex is skipped to avoid
// re-collecting their contents.
func Collect(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.Count(rel, ".bundle") > 1 || strings.Count(rel, ".framework") > 1 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		base := d.Name()
		if d.IsDir() {
			if strings.HasSuffix(base, ".appex") ||
				strings.HasSuffix(base, ".framework") ||
				strings.HasSuffix(base, ".bundle") {
				out = append(out, path)
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(base, ".dylib") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
