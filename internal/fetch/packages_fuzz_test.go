package fetch

import (
	"testing"
)

// FuzzParseIndex smashes the MobileAPT Packages stanza parser: arbitrary
// repo answers (truncated lines, missing colons, runaway continuations,
// NULs) must parse to clean entries or errors — never panics, never an
// entry without a Package id.
func FuzzParseIndex(f *testing.F) {
	f.Add([]byte("Package: com.example.tweak\nVersion: 1.0\nDepends: a | b\n\n"))
	f.Add([]byte("Package: x"))
	f.Add([]byte("NoColonHere\n\nPackage:"))
	f.Add([]byte(" \n \n"))
	f.Add([]byte(""))
	f.Add([]byte("Package: p\n continuation line without key\nFilename: ./f.deb\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseIndex(data)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.Package == "" {
				t.Fatal("parsed entry with an empty Package id")
			}
		}
	})
}
