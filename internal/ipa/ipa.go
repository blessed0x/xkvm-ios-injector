// Package ipa handles .ipa/.tipa containers: extraction of the app bundle
// from a Payload/ archive and repacking with configurable compression.
//
// Behavior mirrors cyan's tbhutils (get_app / make_ipa): Payload + Info.plist
// validation, hidden-entry exclusion on repack (cyan's `zip -r -x "*/.*"`),
// and per-level deflate. Unlike cyan it is pure Go — no `unzip`/`zip` binaries
// are required, which also sidesteps cyan's Chinese-character extraction bug.
package ipa

import (
	"archive/zip"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xkvm/xkvm/internal/log"
)

// minZipTime is the oldest representable zip timestamp. Entries with a zero
// or older modification time are normalized to it (matching the `zip` tool,
// which clamps to 1980 rather than failing).
var minZipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Extract unpacks an .ipa/.tipa archive into dest and returns the path of the
// app bundle (the *.app inside Payload/). It validates the archive the way
// cyan does: a Payload/ directory containing an app with an Info.plist.
func Extract(path, dest string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("%s is not a zipfile (ipa): %w", path, err)
	}
	defer zr.Close()

	var foundPayload, foundPlist bool
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "Payload/") {
			foundPayload = true
		}
		if strings.HasSuffix(f.Name, ".app/Info.plist") {
			foundPlist = true
		}
	}
	if !foundPayload {
		return "", errors.New("couldn't find a Payload folder, invalid ipa")
	}
	if !foundPlist {
		return "", errors.New("no Info.plist, invalid app")
	}

	for _, f := range zr.File {
		if err := extractEntry(f, dest); err != nil {
			return "", err
		}
	}

	app, err := FindApp(dest)
	if err != nil {
		return "", err
	}
	return app, nil
}

// FindApp returns the path of the first *.app directory under dest/Payload.
func FindApp(dest string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dest, "Payload", "*.app"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("couldn't find app folder, invalid ipa")
	}
	return matches[0], nil
}

// Repack zips tmpdir/Payload into output with the given compression level
// (0-9). Hidden entries — any path component starting with "." — are excluded,
// mirroring cyan's `zip -r -{level} -x "*/.*"` (an installd workaround). File
// symlinks are dereferenced like `zip` does; directory symlinks are skipped.
func Repack(tmpdir, output string, level int) error {
	payload := filepath.Join(tmpdir, "Payload")
	if _, err := os.Stat(payload); err != nil {
		return fmt.Errorf("no Payload directory to repack: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}

	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	if level > 0 {
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(out, level)
		})
	}

	err = filepath.WalkDir(payload, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Name() != "Payload" && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(tmpdir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		if d.IsDir() {
			return addDir(zw, rel, info)
		}

		// Dereference file symlinks like the `zip` tool; skip directory ones.
		if info.Mode()&os.ModeSymlink != 0 {
			st, err := os.Stat(path)
			if err != nil {
				log.Warnf("skipping broken symlink: %s", path)
				return nil
			}
			if st.IsDir() {
				log.Warnf("skipping directory symlink: %s", path)
				return nil
			}
			info = st
		}
		return addFile(zw, rel, path, info, level)
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func addDir(zw *zip.Writer, name string, info fs.FileInfo) error {
	fh := &zip.FileHeader{Name: name + "/", Method: zip.Store}
	fh.Modified = normTime(info.ModTime())
	_, err := zw.CreateHeader(fh)
	return err
}

func addFile(zw *zip.Writer, name, path string, info fs.FileInfo, level int) error {
	fh := &zip.FileHeader{Name: name, Method: methodFor(level)}
	fh.Modified = normTime(info.ModTime())
	fh.SetMode(info.Mode().Perm())
	w, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(w, src)
	return err
}

func methodFor(level int) uint16 {
	if level == 0 {
		return zip.Store
	}
	return zip.Deflate
}

func normTime(t time.Time) time.Time {
	if t.Before(minZipTime) {
		return minZipTime
	}
	return t
}

// extractEntry writes one zip entry to dest, enforcing containment (no
// absolute paths, no ".." escapes) and materializing safe symlinks.
func extractEntry(f *zip.File, dest string) error {
	target, err := safeJoin(dest, f.Name)
	if err != nil {
		return err
	}

	if f.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	if f.Mode()&os.ModeSymlink != 0 {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		linkTarget, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		tgt := string(linkTarget)
		// Absolute targets are always rejected: filepath.Join concatenates
		// an absolute second element instead of resetting, which would defeat
		// the containment check below — and Repack dereferences file symlinks,
		// turning a planted /etc/passwd symlink into an exfiltration vector.
		if filepath.IsAbs(tgt) {
			log.Warnf("skipping symlink with absolute target: %s -> %s", f.Name, tgt)
			return nil
		}
		resolved := filepath.Join(filepath.Dir(target), tgt)
		if !within(dest, resolved) {
			log.Warnf("skipping symlink escaping archive root: %s", f.Name)
			return nil
		}
		return os.Symlink(tgt, target)
	}

	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(w, rc)
	closeErr := w.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// safeJoin joins root + name, rejecting paths that would escape root.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean("/" + name) // keep it root-relative
	if clean == "/" {
		return root, nil
	}
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if !within(root, target) {
		return "", fmt.Errorf("refusing to extract path outside archive root: %q", name)
	}
	return target, nil
}

// within reports whether p is inside root (or equal to it).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
