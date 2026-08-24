package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageCommandGuards pins the CLI-facing validation on the three
// converter entry points: non-deb inputs, missing files, and the required
// output path must all be rejected before any converter runs.
func TestPackageCommandGuards(t *testing.T) {
	dir := t.TempDir()
	notADeb := filepath.Join(dir, "input.zip")
	if err := writeFile(notADeb, "zip"); err != nil {
		t.Fatal(err)
	}
	realDeb := filepath.Join(dir, "real.deb")
	if err := writeFile(realDeb, "ar"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		run     func(input, output string) error
		input   string
		output  string
		wantMsg string
	}{
		{"xina non-deb", RootlessXina, notADeb, filepath.Join(dir, "o.deb"), "must be a .deb"},
		{"rootful non-deb", Rootful, notADeb, filepath.Join(dir, "o.deb"), "must be a .deb"},
		{"roothide non-deb", func(i, o string) error { return Roothide(i, o, false, "auto") }, notADeb, filepath.Join(dir, "o.deb"), "must be a .deb"},
		{"rootful missing input", Rootful, filepath.Join(dir, "nope.deb"), filepath.Join(dir, "o.deb"), "does not exist"},
		{"roothide missing input", func(i, o string) error { return Roothide(i, o, false, "auto") }, filepath.Join(dir, "nope.deb"), filepath.Join(dir, "o.deb"), "does not exist"},
		{"xina missing output", RootlessXina, realDeb, "", "output path is required"},
	}
	for _, tc := range cases {
		err := tc.run(tc.input, tc.output)
		if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantMsg, err)
		}
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestFixedOutputPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"App.ipa", "App-fixed.ipa"},
		{"/tmp/x/Tweak.tipa", "/tmp/x/Tweak-fixed.tipa"},
		{"Payload/App.app", "Payload/App-fixed.app"},
	}
	for _, c := range cases {
		if got := fixedOutputPath(c.in); got != c.want {
			t.Errorf("fixedOutputPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
