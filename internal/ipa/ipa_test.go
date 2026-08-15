package ipa

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

const testInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>Test</string>
  <key>CFBundleIdentifier</key><string>com.example.test</string>
</dict></plist>`

// writeAppDir writes a small app bundle under base/Payload/Test.app.
func writeAppDir(t *testing.T, base string, hidden, oldTime bool) {
	t.Helper()
	app := filepath.Join(base, "Payload", "Test.app")
	mustMkdir(t, filepath.Join(app, "Frameworks"))
	mustWrite(t, filepath.Join(app, "Info.plist"), testInfoPlist)
	mustWrite(t, filepath.Join(app, "Test"), "\xcf\xfa\xed\xfe placeholder")
	mustWrite(t, filepath.Join(app, "Frameworks", "lib.dylib"), "dylib-bytes")
	if hidden {
		mustWrite(t, filepath.Join(app, ".hidden"), "secret")
	}
	if oldTime {
		old := time.Unix(0, 0)
		_ = os.Chtimes(filepath.Join(app, "Test"), old, old)
	}
}

// zipDir zips a directory tree into out, independently of package code.
func zipDir(t *testing.T, root, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		// Zip entry names must be forward-slash; filepath.Rel yields native
		// separators (backslashes on Windows), which would break extraction.
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractFindsAppAndContents(t *testing.T) {
	dir := t.TempDir()
	writeAppDir(t, dir, false, false)
	ipaPath := filepath.Join(t.TempDir(), "x.ipa")
	zipDir(t, dir, ipaPath) // zip the dir that CONTAINS Payload/

	dest := t.TempDir()
	got, err := Extract(ipaPath, dest)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if filepath.Base(got) != "Test.app" {
		t.Errorf("app = %q, want .../Test.app", got)
	}
	if _, err := os.Stat(filepath.Join(got, "Frameworks", "lib.dylib")); err != nil {
		t.Errorf("dylib not extracted: %v", err)
	}
}

func TestExtractRejectsNonZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ipa")
	if err := os.WriteFile(p, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(p, t.TempDir()); err == nil {
		t.Fatal("expected an error for a non-zip input")
	}
}

func TestExtractRejectsNoPayload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.ipa")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("foo.txt")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	_ = f.Close()

	if _, err := Extract(p, t.TempDir()); err == nil {
		t.Fatal("expected an error for an archive without Payload/")
	}
}

func TestExtractNeutralizesTraversal(t *testing.T) {
	// A hostile entry "../evil" must never land outside the extraction root.
	dest := t.TempDir()
	p := filepath.Join(t.TempDir(), "x.ipa")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("Payload/Test.app/Info.plist")
	_, _ = w.Write([]byte(testInfoPlist))
	w2, _ := zw.Create("../evil")
	_, _ = w2.Write([]byte("owned"))
	_ = zw.Close()
	_ = f.Close()

	if _, err := Extract(p, dest); err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); err == nil {
		t.Fatal("traversal escaped the extraction root")
	}
}

func TestExtractSkipsAbsoluteSymlink(t *testing.T) {
	// A symlink with an absolute target must never be materialized (Repack
	// dereferences file symlinks, so this is an exfiltration vector).
	dest := t.TempDir()
	p := filepath.Join(t.TempDir(), "x.ipa")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("Payload/Test.app/Info.plist")
	_, _ = w.Write([]byte(testInfoPlist))
	hdr := &zip.FileHeader{Name: "Payload/Test.app/link"}
	hdr.SetMode(os.ModeSymlink | 0o777)
	sl, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sl.Write([]byte("/etc/passwd"))
	_ = zw.Close()
	_ = f.Close()

	if _, err := Extract(p, dest); err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "Payload/Test.app/link")); err == nil {
		t.Error("absolute-target symlink was materialized")
	}
}

func TestRepackGoldenEntries(t *testing.T) {
	dir := t.TempDir()
	writeAppDir(t, dir, true, false) // includes a .hidden entry
	out := filepath.Join(t.TempDir(), "out.ipa")

	if err := Repack(dir, out, 6); err != nil {
		t.Fatalf("Repack() error = %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	gotNames := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		gotNames = append(gotNames, f.Name)
	}
	wantNames := []string{
		"Payload/",
		"Payload/Test.app/",
		"Payload/Test.app/Frameworks/",
		"Payload/Test.app/Frameworks/lib.dylib",
		"Payload/Test.app/Info.plist",
		"Payload/Test.app/Test",
	}
	sort.Strings(gotNames)
	sort.Strings(wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("entries = %v, want %v", gotNames, wantNames)
	}

	// dirs must be stored; files must be deflated at level 6.
	for _, f := range zr.File {
		wantMethod := zip.Deflate
		if len(f.Name) > 0 && f.Name[len(f.Name)-1] == '/' {
			wantMethod = zip.Store
		}
		if f.Method != wantMethod {
			t.Errorf("entry %q method = %d, want %d", f.Name, f.Method, wantMethod)
		}
	}
}

func TestRepackLevelZeroStores(t *testing.T) {
	dir := t.TempDir()
	writeAppDir(t, dir, false, false)
	out := filepath.Join(t.TempDir(), "out.ipa")
	if err := Repack(dir, out, 0); err != nil {
		t.Fatalf("Repack() error = %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Method != zip.Store {
			t.Errorf("entry %q method = %d, want Store", f.Name, f.Method)
		}
	}
}

func TestRepackClampsOldTimestamps(t *testing.T) {
	dir := t.TempDir()
	writeAppDir(t, dir, false, true) // "Test" has a 1970 mtime
	out := filepath.Join(t.TempDir(), "out.ipa")
	if err := Repack(dir, out, 6); err != nil {
		t.Fatalf("Repack() error = %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	floor := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, f := range zr.File {
		if f.Modified.Before(floor) {
			t.Errorf("entry %q modified = %v, before 1980", f.Name, f.Modified)
		}
	}
}

func TestRoundTripExtractRepackExtract(t *testing.T) {
	dir := t.TempDir()
	writeAppDir(t, dir, false, false)
	ipaPath := filepath.Join(t.TempDir(), "x.ipa")
	zipDir(t, dir, ipaPath) // zip the dir that CONTAINS Payload/

	dest1 := t.TempDir()
	if _, err := Extract(ipaPath, dest1); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.ipa")
	if err := Repack(dest1, out, 6); err != nil {
		t.Fatal(err)
	}

	dest2 := t.TempDir()
	if _, err := Extract(out, dest2); err != nil {
		t.Fatal(err)
	}

	set1 := relSet(t, filepath.Join(dest1, "Payload"))
	set2 := relSet(t, filepath.Join(dest2, "Payload"))
	if !reflect.DeepEqual(set1, set2) {
		t.Errorf("round trip changed the app:\nbefore %v\nafter  %v", set1, set2)
	}
}

func relSet(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
