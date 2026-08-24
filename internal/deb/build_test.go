package deb

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// failWriter fails after n successful bytes, simulating a disk that fills
// mid-archive.
type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		got := w.n
		w.n = 0
		return got, errors.New("fake disk full")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestWriteArMembersSurfacesWriteFailure(t *testing.T) {
	members := []struct {
		name    string
		content []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"data.tar.gz", bytes.Repeat([]byte("x"), 512)},
	}
	w := &failWriter{n: 40} // dies inside the second member's payload
	if err := writeArMembers(w, members); err == nil || !strings.Contains(err.Error(), "fake disk full") {
		t.Fatalf("write failure must surface, got %v", err)
	}
}
