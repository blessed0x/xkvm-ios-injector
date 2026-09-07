// Package deb extracts jailbreak tweak packages (.deb) and returns the
// injectable artifacts (dylibs, frameworks, appex, bundles) that cyan's
// extract_deb collects. Compression of data.tar.* is detected by name:
// gzip, xz, zstd, bzip2, lz4, lzma, or raw tar.
//
// GNU ar long-name string tables ("//") and symbol tables ("/") are skipped;
// deb member names are short, so no table resolution is needed.
package deb

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"github.com/pierrec/lz4/v4"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"

	"github.com/blessed0x/xkvm-ios-injector/internal/artifact"
)

// ErrNotADeb is returned when the input is not an ar archive containing a
// data.tar member.
var ErrNotADeb = errors.New("not a deb archive (no data.tar member)")

// Extract unpacks debPath into dest and returns the injectable artifact paths
// (see artifact.Collect).
func Extract(debPath, dest string) ([]string, error) {
	f, err := os.Open(debPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var magic [8]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotADeb, err)
	}
	if string(magic[:]) != arMagic {
		return nil, fmt.Errorf("%w: bad ar magic %q", ErrNotADeb, magic)
	}

	for {
		m, err := readArMember(f)
		if err == io.EOF {
			return nil, ErrNotADeb
		}
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(m.Name, "data.tar") {
			return extractTarMember(m.Data, m.Name, dest)
		}
		// Not the payload — drain and keep looking for data.tar.
		if _, err := io.Copy(io.Discard, m.Data); err != nil {
			return nil, err
		}
	}
}

// extractTarMember decompresses a data.tar.* member into dest and collects
// the resulting artifacts.
func extractTarMember(r io.Reader, name, dest string) ([]string, error) {
	dr, closer, err := decompressorFor(name, r)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	if err := untar(dr, dest); err != nil {
		return nil, fmt.Errorf("couldn't extract %s: %w", name, err)
	}
	arts, err := artifact.Collect(dest)
	if err != nil {
		return nil, err
	}
	return arts, nil
}

// extractTar decompresses a data.tar.* / control.tar.* member into dest
// without artifact collection (the full-payload path used by Unpack).
func extractTar(r io.Reader, name, dest string) error {
	dr, closer, err := decompressorFor(name, r)
	if err != nil {
		return err
	}
	defer closer.Close()
	return untar(dr, dest)
}

func decompressorFor(name string, r io.Reader) (io.Reader, io.Closer, error) {
	switch {
	case strings.HasSuffix(name, ".gz"), strings.HasSuffix(name, ".gzip"):
		zr, err := gzip.NewReader(r)
		return zr, zr, err
	case strings.HasSuffix(name, ".xz"):
		xr, err := xz.NewReader(r)
		return xr, io.NopCloser(xr), err
	case strings.HasSuffix(name, ".zst"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		// klauspost's Decoder.Close returns no error, so wrap it.
		return zr.IOReadCloser(), closeFunc(zr.Close), nil
	case strings.HasSuffix(name, ".lz4"):
		// LZ4 frame format (what `lz4` and dpkg's lz4 variant write); the
		// legacy raw-block format is not a .tar.lz4 member.
		return lz4.NewReader(r), io.NopCloser(nil), nil
	case strings.HasSuffix(name, ".bz2"):
		return bzip2.NewReader(r), io.NopCloser(nil), nil
	case strings.HasSuffix(name, ".lzma"):
		lr, err := lzma.NewReader(r)
		return lr, io.NopCloser(lr), err
	default:
		return r, io.NopCloser(nil), nil
	}
}

// untar extracts a tar stream into dest. Symlinks and special files are
// skipped — never needed for injection, and symlinks are excluded from
// artifact collection anyway (mirroring cyan's security note). Path traversal
// is rejected.
func untar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !isSafeName(hdr.Name) {
			return fmt.Errorf("refusing to extract path outside dest: %q", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		// TypeRegA ('\x00') is the legacy GNU spelling; tar.Reader already
		// normalizes it to TypeReg during parsing, so a single case suffices.
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Skip links entirely (see package comment).
		default:
			// Skip devices, fifos, etc.
		}
	}
}

// closeFunc adapts a no-value Close to io.Closer.
type closeFunc func()

func (f closeFunc) Close() error {
	f()
	return nil
}

// isSafeName rejects absolute names and any parent-traversal ("..") component.
// Debs are attacker-controlled inputs (fetched from repos), so we refuse
// outright rather than sanitize into the root — unlike ipa.Extract, which
// neutralizes hostile names into the extraction root instead. The difference
// is deliberate: an IPA is user-provided, a .deb may be pulled from anywhere.
func isSafeName(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
