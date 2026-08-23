package ipa

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzSafeJoin smashes the zip-slip guard with hostile archive names: a
// success must always land inside root, and every traversal shape must be
// rejected — including Windows separators and drive letters, which filepath
// treats differently per GOOS.
func FuzzSafeJoin(f *testing.F) {
	for _, s := range []string{
		"Payload/App.app/bin",
		"../../etc/passwd",
		"/etc/passwd",
		"..\\\\..\\\\windows\\\\system32\\\\evil.dll",
		"C:\\evil.txt",
		"a/../../b",
		"./ok/./name.dylib",
		"",
		".../...//..//x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		root := t.TempDir()
		target, err := safeJoin(root, name)
		if err != nil {
			return // refused: fine
		}
		if !within(root, target) {
			t.Fatalf("safeJoin(%q) escaped root: %q", name, target)
		}
		// Traversal means a WHOLE segment of dots-dot-dot; a directory
		// literally named "..." is legal and must not false-positive.
		for _, seg := range strings.Split(filepath.ToSlash(target), "/") {
			if seg == ".." {
				t.Fatalf("cleaned target still traverses: %q", target)
			}
		}
	})
}
