package fetch

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// indexVariants are the Packages index file names tried in order (Azule's
// order, confirmed in ARCHITECTURE.md §2.2).
var indexVariants = []string{"Packages.gz", "Packages.zst", "Packages", "Packages.xz", "Packages.bz2"}

// Entry is one stanza from a MobileAPT Packages index.
type Entry struct {
	Package      string
	Version      string
	Depends      string
	Filename     string
	SHA256       string
	Architecture string
}

// fetchIndex downloads and parses the repo's Packages index, trying each
// compression variant in order until one is reachable and parseable.
func fetchIndex(ctx context.Context, client *http.Client, repo string) ([]Entry, error) {
	var lastErr error
	for _, name := range indexVariants {
		url := strings.TrimSuffix(repo, "/") + "/" + name
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
			continue
		}
		body, err := readLimited(resp.Body, 64<<20) // 64 MiB cap; errors on overflow
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		data, err := decompress(body, name)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		entries, err := parseIndex(data)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", url, err)
			continue
		}
		return entries, nil
	}
	return nil, fmt.Errorf("no reachable Packages index at %s (%v)", repo, lastErr)
}

// decompress decodes a Packages index body according to its file name.
// Plain "Packages" passes through unchanged.
func decompress(data []byte, name string) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return readLimited(zr, 128<<20)
	case strings.HasSuffix(name, ".zst"):
		zr, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return readLimited(zr, 128<<20)
	case strings.HasSuffix(name, ".xz"):
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readLimited(xr, 128<<20)
	case strings.HasSuffix(name, ".bz2"):
		return readLimited(bzip2.NewReader(bytes.NewReader(data)), 128<<20)
	default:
		return data, nil
	}
}

// parseIndex parses blank-line-separated APT stanzas. A line starting with a
// space or tab continues the previous field (folded with ", " for Depends);
// lines starting with '#' are comments and ignored.
func parseIndex(data []byte) ([]Entry, error) {
	var entries []Entry
	var cur Entry
	var curKey string
	flush := func() {
		if cur.Package != "" {
			entries = append(entries, cur)
		}
		cur = Entry{}
		curKey = ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20) // 8 MiB per-line cap
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "#"):
			continue
		case line[0] == ' ' || line[0] == '\t':
			if curKey == "" {
				continue // orphan continuation line
			}
			appendField(&cur, curKey, strings.TrimSpace(line), true)
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue // malformed line; skip
			}
			curKey = strings.TrimSpace(k)
			appendField(&cur, curKey, strings.TrimSpace(v), false)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	if len(entries) == 0 {
		return nil, fmt.Errorf("no packages found in index")
	}
	return entries, nil
}

// appendField stores one field value. Depends accumulates across folded
// lines; the other fields keep their first occurrence.
func appendField(e *Entry, key, val string, cont bool) {
	switch key {
	case "Package":
		if !cont && e.Package == "" {
			e.Package = val
		}
	case "Version":
		if !cont && e.Version == "" {
			e.Version = val
		}
	case "Depends":
		val = strings.TrimSpace(val)
		if e.Depends == "" {
			e.Depends = val
		} else if strings.HasSuffix(e.Depends, ",") {
			// A folded line that already ends in a comma (a writer split
			// mid-list) joins with a single space, not another comma.
			e.Depends += " " + val
		} else {
			e.Depends += ", " + val
		}
	case "Filename":
		if !cont && e.Filename == "" {
			e.Filename = val
		}
	case "SHA256":
		if !cont && e.SHA256 == "" {
			e.SHA256 = val
		}
	case "Architecture":
		if !cont && e.Architecture == "" {
			e.Architecture = val
		}
	}
}

// findEntry returns the stanza for id, or nil. When a repo carries multiple
// arch stanzas of the same package, the arm64 one wins (iOS target); the
// first match is the fallback.
func findEntry(entries []Entry, id string) *Entry {
	var first, arm64 *Entry
	for i := range entries {
		e := &entries[i]
		if e.Package != id {
			continue
		}
		if first == nil {
			first = e
		}
		if arm64 == nil && e.Architecture == "arm64" {
			arm64 = e
		}
	}
	if arm64 != nil {
		return arm64
	}
	return first
}

// readLimited reads at most max bytes and errors when the stream is larger,
// so an oversized (or truncated-silently) response can never be mistaken for
// a complete one.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("input exceeds %d bytes", max)
	}
	return b, nil
}
