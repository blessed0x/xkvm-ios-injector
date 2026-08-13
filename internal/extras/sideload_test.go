package extras

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestMaterializeSideloadFixes ensures the bundled fix dylibs exist, are
// materialized, and are real arm64 Mach-O binaries (so the native signer
// never has to round-trip a fat file).
func TestMaterializeSideloadFixes(t *testing.T) {
	dir := t.TempDir()
	paths, err := MaterializeSideloadFixes(dir)
	if err != nil {
		t.Fatalf("MaterializeSideloadFixes: %v", err)
	}
	if len(paths) != len(SideloadFixNames) {
		t.Fatalf("got %d fixes, want %d", len(paths), len(SideloadFixNames))
	}
	for i, p := range paths {
		if filepath.Base(p) != SideloadFixNames[i] {
			t.Errorf("fix[%d] = %s, want %s", i, filepath.Base(p), SideloadFixNames[i])
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		if len(data) < 4 {
			t.Fatalf("%s too small (%d bytes)", p, len(data))
		}
		// Thin arm64 Mach-O magic (MH_MAGIC_64 little-endian); reject fat
		// (0xcafebabe) and thin 32-bit (0xfeedface) variants.
		if !bytes.Equal(data[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) {
			t.Errorf("%s is not a thin arm64 Mach-O (magic %x)", p, data[:4])
		}
	}
}
