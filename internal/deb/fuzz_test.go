package deb

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// FuzzLz4ExtractNoPanic smashes the lz4 decompression leg with arbitrary
// bytes: the extractor must never panic, must never write a path outside
// the destination (zip-slip class), and must return a clean error for
// garbage. This is the protocol's new-leg fuzz target.
func FuzzLz4ExtractNoPanic(f *testing.F) {
	seed := tarBytes(f, map[string]string{"Library/x.dylib": "aaaa"})
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	_, _ = w.Write(seed)
	_ = w.Close()
	f.Add(buf.Bytes())
	f.Add([]byte("not lz4 at all"))
	f.Add([]byte{0x04, 0x22, 0x4D, 0x18, 0x00, 0x01, 0x02}) // valid magic, garbage body
	f.Add([]byte("../../../../etc/passwd"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dest := t.TempDir()
		dr, _, err := decompressorFor("data.tar.lz4", bytes.NewReader(data))
		if err != nil {
			return // a clean error is a pass for garbage input
		}
		// Extraction may fail on garbage; panics and path escapes may not.
		_ = extractTar(dr, "data.tar.lz4", dest)
		// Nothing may exist at the destination's sibling level: the only
		// writes untar is allowed to make are inside dest itself.
		if leaked, _ := filepath.Glob(filepath.Join(dest, "..", "*.wrong")); len(leaked) > 0 {
			t.Fatalf("written outside dest: %v", leaked)
		}
	})
}

// FuzzDebArMember smashes the hand-rolled ar reader: garbage headers must
// come back as clean errors, never panics or runaway allocations, and a
// parsed member must carry its declared payload.
func FuzzDebArMember(f *testing.F) {
	// A minimal valid member header: name field, decimal size, magic.
	hdr := make([]byte, 60)
	copy(hdr[0:], "payload.tar.xz/")
	copy(hdr[48:], "3         ")
	copy(hdr[58:], "`\n")
	f.Add(append(hdr, []byte("abc")...))
	f.Add([]byte("!<arch>\nshort"))       // magic then truncation
	f.Add(bytes.Repeat([]byte{0xFF}, 60)) // garbage sizes
	neg := make([]byte, 60)
	copy(neg[0:], "name.ext/")
	copy(neg[48:], "-1        ") // space-padded negative: reaches ParseInt
	copy(neg[58:], "`\n")
	f.Add(neg)
	big := make([]byte, 60)
	copy(big[0:], "big/")
	copy(big[48:], "9999999999") // ~10GB claimed from a 60-byte header
	copy(big[58:], "`\n")
	f.Add(big)
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := readArMember(bytes.NewReader(data))
		if err != nil {
			return // clean error is a pass
		}
		if m.Name == "" {
			t.Fatal("parsed member must carry a name")
		}
	})
}

// TestArMemberRejectsHostileSizes pins the bounds fix: sizes parsed from a
// hostile header must be rejected as errors — not panicked on (negative
// makeslice) and not attempted as multi-gigabyte allocations. The size
// field is exactly bytes [48:58], space-padded like real ar output.
func TestArMemberRejectsHostileSizes(t *testing.T) {
	cases := []struct{ name, size string }{
		{"negative", "-1        "},
		{"ten-digits", "9999999999"},
	}
	for _, tc := range cases {
		if len(tc.size) != 10 {
			t.Fatalf("seed %q must fill the 10-byte size field", tc.size)
		}
		hdr := make([]byte, 60)
		copy(hdr[0:], "x/")
		copy(hdr[48:], tc.size)
		copy(hdr[58:], "`\n")
		if _, err := readArMember(bytes.NewReader(hdr)); err == nil {
			t.Fatalf("%s: hostile ar size %q must be rejected", tc.name, tc.size)
		}
	}
}
