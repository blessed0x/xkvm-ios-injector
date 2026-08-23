package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFilePreservesPermsAndCreatesParents(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "deep", "tweak.dylib")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("mach-o bytes"), 0o755); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "out", "Frameworks", "tweak.dylib")
	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "mach-o bytes" {
		t.Fatalf("content mismatch: %q %v", data, err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("perm bits must survive the copy, got %v", st.Mode().Perm())
	}
}

func TestCopyFileOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	os.WriteFile(src, []byte("new"), 0o644)
	os.WriteFile(dst, []byte("old-content-longer"), 0o644)

	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "new" {
		t.Fatalf("O_TRUNC missing: dst holds %q", data)
	}
}

func TestCopyTreeMirrorsNestedTreeWithPerms(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "App.app")
	for _, d := range []string{
		"Frameworks/Foo.framework",
		"PlugIns/Widget.appex",
	} {
		if err := os.MkdirAll(filepath.Join(app, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(app, "Frameworks", "Foo.framework", "Foo")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := filepath.Join(app, "PlugIns", "Widget.appex", "Info.plist")
	if err := os.WriteFile(res, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}

	mirror := filepath.Join(dir, "mirror", "App.app")
	if err := CopyTree(app, mirror); err != nil {
		t.Fatal(err)
	}

	gotBin, err := os.ReadFile(filepath.Join(mirror, "Frameworks", "Foo.framework", "Foo"))
	if err != nil || string(gotBin) != "binary" {
		t.Fatalf("nested binary missing: %v", err)
	}
	st, err := os.Stat(filepath.Join(mirror, "Frameworks", "Foo.framework", "Foo"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("executable bit flattened: %v", st.Mode().Perm())
	}
	stPlist, err := os.Stat(filepath.Join(mirror, "PlugIns", "Widget.appex", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if stPlist.Mode().Perm() != 0o644 {
		t.Fatalf("plist perm changed: %v", stPlist.Mode().Perm())
	}
}

func TestCopyTreeSingleFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "one.dylib")
	os.WriteFile(src, []byte("x"), 0o644)
	dst := filepath.Join(dir, "out", "one.dylib")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("single-file tree: %v", err)
	}
}

func TestCopyFileMissingSource(t *testing.T) {
	if err := CopyFile(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "d")); err == nil {
		t.Fatal("missing source must error")
	}
}
