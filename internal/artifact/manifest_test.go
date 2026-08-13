package artifact

import (
	"path/filepath"
	"testing"
)

func TestPlacementFor(t *testing.T) {
	cases := []struct {
		rel  string
		want Placement
	}{
		{"Regram.dylib", PlacementRoot},
		{"Regram.bundle/Info.plist", PlacementOther},
		{"Frameworks/Sparkle.dylib", PlacementFrameworks},
		{"Frameworks/Sparkle.framework/Sparkle", PlacementFrameworks},
		{"PlugIns/OpenInRegramExtension.appex", PlacementPlugins},
		{"PlugIns/x.appex/Info.plist", PlacementPlugins},
		{"Sub/somewhere.dylib", PlacementOther},
	}
	for _, c := range cases {
		if got := PlacementFor(c.rel); got != c.want {
			t.Errorf("PlacementFor(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

func TestKindFor(t *testing.T) {
	cases := map[string]string{
		"a.dylib":                "dylib",
		"X.framework":            "framework",
		"Y.bundle":               "bundle",
		"Z.appex":                "appex",
		"whatever.other":         "file",
		"Frameworks/X.framework": "framework",
	}
	for in, want := range cases {
		if got := KindFor(in); got != want {
			t.Errorf("KindFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xkvm-manifest.json")
	m := &Manifest{
		Format: 1,
		Source: "in.ipa",
		Artifacts: []ManifestEntry{
			{Name: "Regram.dylib", Kind: "dylib", Placement: PlacementRoot},
			{Name: "Sparkle.dylib", Kind: "dylib", Placement: PlacementFrameworks},
			{Name: "Sparkle.bundle", Kind: "bundle", Placement: PlacementRoot},
			{Name: "OpenInRegramExtension.appex", Kind: "appex", Placement: PlacementPlugins},
		},
	}
	if err := WriteManifest(path, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Format != 1 || got.Source != "in.ipa" || len(got.Artifacts) != 4 {
		t.Fatalf("unexpected manifest: %+v", got)
	}
	if got.Artifacts[0] != m.Artifacts[0] {
		t.Errorf("first entry = %+v, want %+v", got.Artifacts[0], m.Artifacts[0])
	}
	if got.Artifacts[2].Placement != PlacementRoot {
		t.Errorf("bundle placement = %q, want root", got.Artifacts[2].Placement)
	}
}

func TestReadManifestMissing(t *testing.T) {
	if _, err := ReadManifest(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected an error for a missing manifest")
	}
}
