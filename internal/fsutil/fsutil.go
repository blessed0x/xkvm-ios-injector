// Package fsutil holds the file-copy primitives shared by the pipeline
// stages. It exists because the same copy loop had drifted into three
// private copies: one streamed, one buffered whole files into memory, one
// silently flattened every mode to 0644. One implementation, one contract:
//
//   - CopyFile streams (no whole-file memory spike), creates dst's parent
//     directory when missing, and preserves the source's permission bits.
//   - CopyTree walks recursively preserving permissions and propagating
//     walk errors — a mirror, not a re-creation.
package fsutil

import (
	"io"
	"os"
	"path/filepath"
)

// CopyFile copies src to dst, streaming the content and preserving the
// source's permission bits. dst's parent directory is created when missing.
func CopyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// CopyTree copies the file or directory tree at src to dst (dst itself is
// created), preserving directory and file permission bits and materializing
// symlinks as their targets' content.
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		return CopyFile(path, target)
	})
}
