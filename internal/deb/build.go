package deb

// This file adds the writer half of the deb package (the reader lives in
// deb.go/ar.go): packaging a staging directory into a valid .deb ar archive,
// plus Unpack, which extracts BOTH the control and data tars (Extract only
// handles the data payload). The format follows dpkg-deb -b conventions:
// debian-binary "2.0", control.tar.gz (from the staging DEBIAN/ dir), then
// data.tar.gz (everything else). Timestamps are zeroed so builds are
// reproducible.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Control is a generated DEBIAN/control file. Keys are emitted in the
// conventional deb order; empty fields are omitted.
type Control struct {
	Package      string
	Name         string
	Version      string
	Architecture string
	Depends      []string
	Description  string
	Maintainer   string
	Author       string
	Section      string
}

// String renders the control file in dpkg key: value form. Depends entries
// are joined with ", " (dpkg's list separator).
func (c Control) String() string {
	var b strings.Builder
	w := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	w("Package", c.Package)
	w("Name", c.Name)
	w("Version", c.Version)
	w("Architecture", c.Architecture)
	if len(c.Depends) > 0 {
		w("Depends", strings.Join(c.Depends, ", "))
	}
	w("Description", c.Description)
	w("Maintainer", c.Maintainer)
	w("Author", c.Author)
	w("Section", c.Section)
	return b.String()
}

// Build packages the directory tree at root into a valid .deb ar archive at
// dest. Convention (dpkg-deb -b): the staging DEBIAN/ directory becomes
// control.tar.gz, every other file becomes data.tar.gz, and payload entries
// are written with the canonical "./" prefix. Symlinks are preserved as tar
// symlink entries. Timestamps are zeroed for reproducible builds.
func Build(root, dest string) error {
	controlTar, err := tarDir(filepath.Join(root, "DEBIAN"), true)
	if err != nil {
		return fmt.Errorf("building control.tar.gz: %w", err)
	}
	dataTar, err := tarDir(root, false)
	if err != nil {
		return fmt.Errorf("building data.tar.gz: %w", err)
	}

	members := []struct {
		name    string
		content []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", controlTar},
		{"data.tar.gz", dataTar},
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(arMagic); err != nil {
		return err
	}
	for _, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", m.name, 0, 0, 0, 0o100644, len(m.content))
		if len(hdr) != 60 {
			return fmt.Errorf("internal: bad ar header length %d", len(hdr))
		}
		if _, err := f.WriteString(hdr); err != nil {
			return err
		}
		if _, err := f.Write(m.content); err != nil {
			return err
		}
		if len(m.content)%2 == 1 {
			if _, err := f.WriteString("\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// tarDir gzips the directory tree at root into a tar.gz byte slice. When
// control is true the entries are prefixed "./" (matching dpkg-deb's control
// archive); payload entries also use "./" so extraction roots land at the
// staging root. Directories and symlinks are emitted as their own entries;
// mode bits are preserved, ownership and times are zeroed.
func tarDir(root string, control bool) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // the root itself is implicit
		}
		// Control members live in their own archive: exclude the DEBIAN
		// directory itself AND its contents (the bare "DEBIAN" rel does not
		// match a "DEBIAN/" prefix check — without this a stray ./DEBIAN
		// entry would make dpkg create a spurious /DEBIAN on install).
		if !control && (rel == "DEBIAN" || strings.HasPrefix(filepath.ToSlash(rel), "DEBIAN/")) {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:     "./" + filepath.ToSlash(rel),
			Mode:     int64(info.Mode().Perm()),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if d.IsDir() {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = link
			return tw.WriteHeader(hdr)
		}
		hdr.Size = info.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack extracts both the control tar (into dest/DEBIAN) and the data tar
// (into dest) of debPath. This is the full deb extraction used by the
// rootless converter, which needs the entire payload — unlike Extract, which
// only unpacks data.tar and collects injectable artifacts.
func Unpack(debPath, dest string) error {
	f, err := os.Open(debPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("%w: %v", ErrNotADeb, err)
	}
	if string(magic[:]) != arMagic {
		return fmt.Errorf("%w: bad ar magic %q", ErrNotADeb, magic)
	}

	found := false
	for {
		m, err := readArMember(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(m.Name, "control.tar"):
			if err := extractTar(m.Data, m.Name, filepath.Join(dest, "DEBIAN")); err != nil {
				return fmt.Errorf("couldn't extract %s: %w", m.Name, err)
			}
			found = true
		case strings.HasPrefix(m.Name, "data.tar"):
			if err := extractTar(m.Data, m.Name, dest); err != nil {
				return fmt.Errorf("couldn't extract %s: %w", m.Name, err)
			}
			found = true
		default:
			// debian-binary, md5sums-sidecars, etc. — drain and continue.
			if _, err := io.Copy(io.Discard, m.Data); err != nil {
				return err
			}
		}
	}
	if !found {
		return ErrNotADeb
	}
	return nil
}
