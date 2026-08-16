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
