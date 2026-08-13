// Package patch implements Feather-style togglable compatibility patches
// applied to an extracted app bundle. Each patch is registered by name; the
// CLI binds one bool flag per registered patch (spec D10) and the pipeline
// applies the enabled set after plist operations and before fakesign
// (feather-ellekit-spec.md §4.5).
package patch

import (
	"fmt"
	"sort"
)

// Patch mutates an extracted app bundle (appDir) in place.
type Patch interface {
	// Name is the flag name (--<name>) and registry key.
	Name() string
	// Apply runs the patch against the app bundle directory.
	Apply(appDir string) error
}

var registry = map[string]Patch{}

// Register adds a patch to the global registry. Duplicate names panic so a
// mis-registration fails at startup rather than silently overriding.
func Register(p Patch) {
	name := p.Name()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("patch %q already registered", name))
	}
	registry[name] = p
}

// Names returns the registered patch names in sorted order, giving the CLI a
// stable, deterministic flag order.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Apply runs the named patches against appDir in order. An unknown name is an
// error so a typo'd config surfaces instead of silently doing nothing.
func Apply(appDir string, enabled []string) error {
	for _, name := range enabled {
		p, ok := registry[name]
		if !ok {
			return fmt.Errorf("unknown patch %q (registered: %v)", name, Names())
		}
		if err := p.Apply(appDir); err != nil {
			return fmt.Errorf("patch %s: %w", name, err)
		}
	}
	return nil
}
