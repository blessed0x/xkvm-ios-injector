package decrypt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeFileAtomic streams r to a temp file beside dest and renames it into
// place, so a failed download never leaves a partial .ipa at the output path.
func writeFileAtomic(dest string, r io.Reader) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".xkvm-dl-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}
