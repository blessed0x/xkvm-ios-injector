package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Placement records where an artifact originally lived inside an app bundle.
// Re-injection honors it: a root-placed dylib gets the @executable_path
// contract again instead of silently moving to Frameworks/.
type Placement string

const (
	PlacementRoot       Placement = "root"       // Payload/App.app/Foo.dylib
	PlacementFrameworks Placement = "frameworks" // Payload/App.app/Frameworks/Foo.dylib
	PlacementPlugins    Placement = "plugins"    // Payload/App.app/PlugIns/Foo.appex
	PlacementOther      Placement = "other"      // nested elsewhere in the bundle
)

// ManifestEntry describes one extracted artifact.
type ManifestEntry struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // dylib | framework | bundle | appex | file
	Placement Placement `json:"placement"`
}

// Manifest is the xkvm-manifest.json sidecar written by `xkvm extract` next
// to the extracted artifacts, so re-injection can restore each file's
// original placement automatically.
type Manifest struct {
	Format    int             `json:"format"`
	Source    string          `json:"source,omitempty"` // the app/ipa/tipa input path
	Artifacts []ManifestEntry `json:"artifacts"`
}

// KindFor classifies an artifact path by its extension.
func KindFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".dylib"):
		return "dylib"
	case strings.HasSuffix(path, ".framework"):
		return "framework"
	case strings.HasSuffix(path, ".bundle"):
		return "bundle"
	case strings.HasSuffix(path, ".appex"):
		return "appex"
	default:
		return "file"
	}
}

// PlacementFor maps an artifact's path relative to the app bundle root to its
// placement. The first path component decides: Frameworks/ and PlugIns/ are
// the two structural dirs; anything else at the top level is the app root.
func PlacementFor(rel string) Placement {
	rel = filepath.ToSlash(rel)
	switch {
	case strings.HasPrefix(rel, "Frameworks/"):
		return PlacementFrameworks
	case strings.HasPrefix(rel, "PlugIns/"):
		return PlacementPlugins
	case !strings.Contains(rel, "/"):
		return PlacementRoot
	default:
		return PlacementOther
	}
}

// WriteManifest writes m as pretty JSON to path (mode 0o644).
func WriteManifest(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadManifest parses a manifest file written by WriteManifest.
func ReadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
