package tui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failWriter fails once more than n bytes are written, simulating a disk
// that fills mid-export.
type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		return 0, errors.New("fake disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestZipDebsToSurfacesWriteFailure(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.deb")
	b := filepath.Join(dir, "b.deb")
	os.WriteFile(a, bytes.Repeat([]byte("x"), 128), 0o644)
	os.WriteFile(b, bytes.Repeat([]byte("y"), 128), 0o644)

	w := &failWriter{n: 32} // dies when the buffered archive finally flushes
	n, err := zipDebsTo(w, []string{a, b})
	if err == nil || !strings.Contains(err.Error(), "fake disk full") {
		t.Fatalf("write failure must surface (from Close's flush), got %v", err)
	}
	// The count reflects successful entry creation, not underlying bytes:
	// the zip writer buffers, so both entries were staged before the flush
	// hit the full disk.
	if n != 2 {
		t.Fatalf("staged-deb count = %d, want 2", n)
	}
}
