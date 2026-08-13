// Package appbundle operates on an extracted *.app bundle: metadata edits
// (name/version/bundle id/min-OS), plist merging, icon replacement, watch
// app / extension removal, and mass fakesigning/thinning. It is the Go port
// of cyan's AppBundle + Plist types (cyan/tbhtypes/app_bundle.py and
// cyan/tbhtypes/plist.py).
package appbundle

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/plist"
)

// Bundle is an extracted app bundle plus its parsed Info.plist and main
// executable handle.
type Bundle struct {
	Path string
	Info plist.Dict
	Main macho.Bin
}

// Open reads the app at appDir: its Info.plist and the main executable named
// by CFBundleExecutable.
func Open(appDir string) (*Bundle, error) {
	info, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		return nil, err
	}
	exe, _ := info["CFBundleExecutable"].(string)
	if exe == "" {
		return nil, fmt.Errorf("info.plist has no CFBundleExecutable")
	}
	mainPath := filepath.Join(appDir, exe)
	if _, err := os.Stat(mainPath); err != nil {
		return nil, fmt.Errorf("main executable %s does not exist", mainPath)
	}
	return &Bundle{Path: appDir, Info: info, Main: macho.Bin{Path: mainPath}}, nil
}

// Save writes the Info.plist back as a binary plist.
func (b *Bundle) Save() error {
	return plist.Write(filepath.Join(b.Path, "Info.plist"), b.Info)
}

// change sets keys to val, saving if any actually changed (cyan's Plist.change).
func (b *Bundle) change(val any, keys ...string) (bool, error) {
	changed := false
	for _, k := range keys {
		if b.Info[k] == val {
			continue
		}
		b.Info[k] = val
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, b.Save()
}

// ChangeName sets CFBundleName and CFBundleDisplayName, plus any localized
// InfoPlist.strings under *.lproj (best effort, mirroring cyan).
func (b *Bundle) ChangeName(name string) error {
	changed, err := b.change(name, "CFBundleName", "CFBundleDisplayName")
	if err != nil {
		return err
	}
	if !changed {
		log.Infof("name was already %q", name)
		return nil
	}
	log.Infof("changed name to %q", name)

	n := 0
	lprojs, _ := filepath.Glob(filepath.Join(b.Path, "*.lproj"))
	for _, lproj := range lprojs {
		p := filepath.Join(lproj, "InfoPlist.strings")
		d, err := plist.Open(p)
		if err != nil {
			continue // file may not exist or is not plist-parseable
		}
		altered := false
		for _, k := range []string{"CFBundleName", "CFBundleDisplayName"} {
			if d[k] == name {
				continue
			}
			d[k] = name
			altered = true
		}
		if !altered {
			continue
		}
		if err := plist.Write(p, d); err != nil {
			continue
		}
		n++
	}
	if n != 0 {
		log.Infof("changed %d localized names", n)
	}
	return nil
}

// ChangeVersion sets CFBundleVersion and CFBundleShortVersionString.
func (b *Bundle) ChangeVersion(version string) error {
	changed, err := b.change(version, "CFBundleVersion", "CFBundleShortVersionString")
	if err != nil {
		return err
	}
	if changed {
		log.Infof("changed version to %q", version)
	} else {
		log.Infof("version was already %q", version)
	}
	return nil
}

// ChangeBundleID sets CFBundleIdentifier and propagates the rename to any
// *.appex/Info.plist two levels deep (cyan's change_bundle_id).
func (b *Bundle) ChangeBundleID(bundleID string) error {
	orig, _ := b.Info["CFBundleIdentifier"].(string)
	changed, err := b.change(bundleID, "CFBundleIdentifier")
	if err != nil {
		return err
	}
	if !changed {
		log.Infof("bundle id was already %q", bundleID)
		return nil
	}
	log.Infof("changed bundle id to %q", bundleID)

	n := 0
	for _, dir := range dirEntries(b.Path) {
		exts, _ := filepath.Glob(filepath.Join(b.Path, dir, "*.appex"))
		for _, ext := range exts {
			ep := filepath.Join(ext, "Info.plist")
			d, err := plist.Open(ep)
			if err != nil {
				continue
			}
			cur, _ := d["CFBundleIdentifier"].(string)
			if cur == "" {
				continue
			}
			d["CFBundleIdentifier"] = strings.ReplaceAll(cur, orig, bundleID)
			if err := plist.Write(ep, d); err != nil {
				continue
			}
			n++
		}
	}
	if n != 0 {
		log.Infof("changed %d other bundle ids", n)
	}
	return nil
}

// ChangeMinimumOS sets MinimumOSVersion.
func (b *Bundle) ChangeMinimumOS(min string) error {
	changed, err := b.change(min, "MinimumOSVersion")
	if err != nil {
		return err
	}
	if changed {
		log.Infof("changed minimum version to %q", min)
	} else {
		log.Infof("minimum version was already %q", min)
	}
	return nil
}

// MergePlist sets every key from the given plist file into Info.plist
// (cyan's merge_plist).
func (b *Bundle) MergePlist(path string) error {
	d, err := plist.Open(path)
	if err != nil {
		return fmt.Errorf("couldn't parse %s: %w", path, err)
	}
	changed := false
	for k, v := range d {
		if b.Info[k] == v {
			continue
		}
		b.Info[k] = v
		changed = true
	}
	if !changed {
		log.Infof("no modified plist entries")
		return nil
	}
	if err := b.Save(); err != nil {
		return err
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	log.Infof("set plist keys: %s", strings.Join(keys, ", "))
	return nil
}

// RemoveUISupportedDevices removes UISupportedDevices from Info.plist.
func (b *Bundle) RemoveUISupportedDevices() error {
	if _, ok := b.Info["UISupportedDevices"]; !ok {
		log.Infof("no UISupportedDevices")
		return nil
	}
	delete(b.Info, "UISupportedDevices")
	if err := b.Save(); err != nil {
		return err
	}
	log.Infof("removed UISupportedDevices")
	return nil
}

// EnableDocuments sets UISupportsDocumentBrowser and UIFileSharingEnabled.
func (b *Bundle) EnableDocuments() error {
	changed, err := b.change(true, "UISupportsDocumentBrowser", "UIFileSharingEnabled")
	if err != nil {
		return err
	}
	if changed {
		log.Infof("enabled documents support")
	} else {
		log.Infof("documents support was already enabled")
	}
	return nil
}

// RemoveWatchApps removes Watch, WatchKit and com.apple.WatchPlaceholder.
func (b *Bundle) RemoveWatchApps() error {
	if b.remove("Watch", "WatchKit", "com.apple.WatchPlaceholder") {
		log.Infof("removed watch app")
	} else {
		log.Infof("watch app not present")
	}
	return nil
}

// RemoveAllExtensions removes Extensions and PlugIns.
func (b *Bundle) RemoveAllExtensions() error {
	if b.remove("Extensions", "PlugIns") {
		log.Infof("removed app extensions")
	} else {
		log.Infof("no app extensions")
	}
	return nil
}

// remove deletes the named paths (absolute or relative to the bundle).
func (b *Bundle) remove(names ...string) bool {
	existed := false
	for _, name := range names {
		path := name
		if !strings.Contains(name, b.Path) {
			path = filepath.Join(b.Path, name)
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			continue
		}
		existed = true
	}
	return existed
}

// RemoveEncryptedExtensions removes one-level *.appex bundles whose main
// binary is encrypted (cryptid 1). Returns the removed names.
func (b *Bundle) RemoveEncryptedExtensions() ([]string, error) {
	var removed []string
	for _, dir := range dirEntries(b.Path) {
		exts, _ := filepath.Glob(filepath.Join(b.Path, dir, "*.appex"))
		for _, ext := range exts {
			sub, err := Open(ext)
			if err != nil {
				continue
			}
			enc, err := sub.Main.IsEncrypted()
			if err != nil || !enc {
				continue
			}
			_ = os.RemoveAll(ext)
			removed = append(removed, filepath.Base(sub.Main.Path))
		}
	}
	if len(removed) == 0 {
		log.Infof("no encrypted plugins")
	} else {
		log.Infof("removed encrypted plugins: %s", strings.Join(removed, ", "))
	}
	return removed, nil
}

// executables returns every binary in the bundle that would need signing or
// thinning: all *.dylib files at any depth, and the main binary of each
// *.appex and *.framework (cyan's recursive get_executables + the
// CFBundleExecutable resolution). A WalkDir pass is used because Go's
// filepath.Glob treats ** as a single component.
func (b *Bundle) executables() []macho.Bin {
	var bins []macho.Bin
	bins = append(bins, b.Main)
	add := func(p string) {
		// Framework/appex binaries can be discovered both via their own
		// Info.plist and (rarely) via a duplicate dylib entry; dedupe.
		for _, e := range bins {
			if e.Path == p {
				return
			}
		}
		bins = append(bins, macho.Bin{Path: p})
	}
	_ = filepath.WalkDir(b.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasSuffix(d.Name(), ".appex") || strings.HasSuffix(d.Name(), ".framework") {
				if exe, ok := bundleExecutable(path); ok {
					add(filepath.Join(path, exe))
				}
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".dylib") {
			add(path)
		}
		return nil
	})
	return bins
}

// bundleExecutable reads CFBundleExecutable from a bundle dir's Info.plist.
func bundleExecutable(dir string) (string, bool) {
	d, err := plist.Open(filepath.Join(dir, "Info.plist"))
	if err != nil {
		return "", false
	}
	exe, _ := d["CFBundleExecutable"].(string)
	return exe, exe != ""
}

// FakesignAll ad-hoc signs the main executable and every injected binary.
func (b *Bundle) FakesignAll() (int, error) {
	return b.massOperate("fakesigned", func(bin macho.Bin) bool {
		return bin.Fakesign() == nil
	})
}

// ThinAll thins every binary to arm64.
func (b *Bundle) ThinAll() (int, error) {
	return b.massOperate("thinned", func(bin macho.Bin) bool {
		return bin.ThintoArm64() == nil
	})
}

func (b *Bundle) massOperate(op string, fn func(macho.Bin) bool) (int, error) {
	count := 0
	for _, bin := range b.executables() {
		if fn(bin) {
			count++
		}
	}
	log.Infof("%s %d item(s)", op, count)
	return count, nil
}

// dirEntries lists the immediate child directories of path, sorted.
func dirEntries(path string) []string {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// randomSuffix returns a random lowercase-hex string (cyan uses uuid4 hex
// truncated to 7 chars plus a trailing 'a' so the icon name never ends in a
// digit).
func randomSuffix() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "cyan_" + hex.EncodeToString(buf)[:7] + "a"
}
