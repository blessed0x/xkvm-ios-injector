package artifact

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func buildTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCollectFindsEveryInjectableKind(t *testing.T) {
	root := buildTree(t, map[string]string{
		"Frameworks/Foo.framework/Foo":        "binary",
		"Frameworks/Foo.framework/Info.plist": "plist",
		"PlugIns/Widget.appex/Widget":         "binary",
		"res.bundle/img.png":                  "png",
		"libTweak.dylib":                      "mach-o",
		"Payload/other.dylib":                 "mach-o",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{
		filepath.Join(root, "Frameworks", "Foo.framework"),
		filepath.Join(root, "PlugIns", "Widget.appex"),
		filepath.Join(root, "Payload", "other.dylib"),
		filepath.Join(root, "libTweak.dylib"),
		filepath.Join(root, "res.bundle"),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collected mismatch:\n got  %v\n want %v", got, want)
	}
}

func TestCollectSkipsNestedAndFrameworkInternals(t *testing.T) {
	root := buildTree(t, map[string]string{
		"Frameworks/Foo.framework/Foo":                              "outer binary",
		"Frameworks/Foo.framework/Frameworks/Inner.framework/Inner": "inner binary",
		"Frameworks/Foo.framework/libInnerHelper.dylib":             "internal helper",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "Foo.framework" {
		t.Fatalf("want exactly the outer framework, got %v", got)
	}
}

func TestCollectSkipsSymlinks(t *testing.T) {
	root := buildTree(t, map[string]string{"real.dylib": "mach-o"})
	if err := os.Symlink(filepath.Join(root, "real.dylib"), filepath.Join(root, "link.dylib")); err != nil {
		t.Fatal(err)
	}
	got, err := Collect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "real.dylib" {
		t.Fatalf("symlinked dylib must be skipped, got %v", got)
	}
}

func TestCollectBundleDirVsPlainFile(t *testing.T) {
	// A single ".bundle" component passes the nested-bundle guard (>1
	// occurrences trips it), so a directory named weird.a.bundle is collected;
	// a plain FILE named ok.bundle is not — only *.dylib files are collected.
	root := buildTree(t, map[string]string{
		"weird.a.bundle/x": "content",
		"ok.bundle":        "content",
	})
	got, err := Collect(root)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{filepath.Join(root, "weird.a.bundle")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bundle-dir-vs-file mismatch:\n got  %v\n want %v", got, want)
	}
}
