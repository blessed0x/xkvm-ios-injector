package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateDecodesURLEscapedPaths pins the pasted-path fix: paths users
// copy from browsers/Finder often arrive with %20 (and other %-escapes) for
// spaces. validate() must decode them before any stat, so a %20-encoded
// path to a real file succeeds. This is the regression for the
// "X does not exist" report on AyuGram's SP_9.1.76_ES+GS_STACKED.ipa, whose
// real name has no spaces but whose directory ("AyuGram Desktop") does.
func TestValidateDecodesURLEscapedPaths(t *testing.T) {
	dir := t.TempDir()
	// A directory with a space in the name, like "AyuGram Desktop".
	spaced := filepath.Join(dir, "AyuGram Desktop")
	if err := os.MkdirAll(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	appPath := filepath.Join(spaced, "Stock.ipa")
	if err := os.WriteFile(appPath, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	dylib := filepath.Join(spaced, "Cool Tweak.dylib")
	if err := os.WriteFile(dylib, []byte("dylib"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same paths with the space URL-escaped, exactly as a pasted path
	// arrives. %2F in the encoded form must NOT be unescaped (that would
	// escape the directory separator) — keep it simple: only test %20 here.
	encApp := filepath.Join(dir, "AyuGram%20Desktop", "Stock.ipa")
	encDylib := filepath.Join(dir, "AyuGram%20Desktop", "Cool%20Tweak.dylib")

	o := &Options{Input: encApp, Files: []string{encDylib}, Overwrite: true}
	if err := o.validate(); err != nil {
		t.Fatalf("validate() with %%-escaped paths failed: %v", err)
	}
	if o.Input != appPath {
		t.Errorf("Input not decoded: got %q want %q", o.Input, appPath)
	}
	if len(o.Files) != 1 || o.Files[0] != dylib {
		t.Errorf("Files not decoded: got %q want %q", o.Files, dylib)
	}
}

// TestValidateLeavesLiteralPercentAlone: a filename that genuinely contains
// a literal "%" (not a valid escape) must not be mangled or rejected.
func TestValidateLeavesLiteralPercentAlone(t *testing.T) {
	dir := t.TempDir()
	// "100%Off.ipa" — the "%Of" is not a valid hex escape, so PathUnescape
	// fails and the path must stay byte-identical.
	appPath := filepath.Join(dir, "100%Off.ipa")
	if err := os.WriteFile(appPath, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Options{Input: appPath, Overwrite: true}
	if err := o.validate(); err != nil {
		t.Fatalf("validate() failed on a literal-percent filename: %v", err)
	}
	if o.Input != appPath {
		t.Errorf("literal %% path was mangled: got %q", o.Input)
	}
}
