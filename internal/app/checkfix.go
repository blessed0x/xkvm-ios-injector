package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/appbundle"
	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/fetch"
	"github.com/xscope0/xkvm-ios-injector/internal/ipa"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// FixOptions controls the auto-resolution behavior of CheckAndFix. Confirm is
// the tier-2 (heuristic dlopen finding) gate: it is called with the wanted
// artifact name and the found path, and must return true for the injection to
// happen. AutoYes bypasses the gate entirely (equivalent to answering yes to
// everything). AllowFetch lets the fixer search the Canister/MobileAPT repos
// as a last resort when no local source has the artifact.
type FixOptions struct {
	AllowFetch bool
	AutoYes    bool
	Confirm    func(name, artifact string) bool
}

// CheckAndFix is the `xkvm check --fix` pipeline: it opens the bundle, runs
// the completeness check, and for every unresolved bundle-relative reference
// tries to find the missing artifact — in the given fix dirs, inside .debs
// those dirs contain, in the persistent fetch cache, then (optionally) in the
// repos by name. Found artifacts are placed exactly where the check looks for
// them, the whole bundle is re-signed (newly added Mach-Os are unsigned), the
// check re-runs, and a fixed copy is written to output. The original input is
// never touched. Returns an error when any reference remains unresolved, so
// the command can exit non-zero.
func CheckAndFix(input, output string, fixDirs []string, fo FixOptions) error {
	if _, err := os.Stat(input); err != nil {
		return fmt.Errorf("%s does not exist", input)
	}
	ext := strings.ToLower(filepath.Ext(input))
	isIPA := ext == ".ipa" || ext == ".tipa"
	if !isIPA && ext != ".app" {
		return fmt.Errorf("the input must be an app/ipa/tipa")
	}
	if output == "" {
		output = fixedOutputPath(input)
	}
	inAbs, _ := filepath.Abs(input)
	outAbs, _ := filepath.Abs(output)
	if outAbs == inAbs {
		return errors.New("refusing to overwrite the input; pass -o with a new path")
	}

	tmpdir, err := os.MkdirTemp("", "xkvm-fix-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	appDir, err := prepareApp(input, tmpdir, isIPA)
	if err != nil {
		return err
	}
	bundle, err := appbundle.Open(appDir)
	if err != nil {
		return err
	}

	missing, err := bundle.CheckReferences()
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		log.Infof("check %s: all bundle-relative dependencies resolve — nothing to fix", filepath.Base(input))
		return nil
	}
	log.Infof("check %s: %d unresolved reference(s); searching for the missing artifacts", filepath.Base(input), len(missing))

	s := newFixSearcher(fixDirs, fo.AllowFetch, tmpdir)
	applied := 0
	for _, m := range missing {
		name, dir, tier2 := bundle.FixTarget(m)
		if name == "" {
			log.Warnf("can't determine what %s needs (dep %q); skipping", m.From, m.Dep)
			continue
		}
		if !s.hasSources() && !fo.AllowFetch {
			log.Warnf("no search sources given (pass --fix-dir, or leave --no-fetch off); %s stays unresolved", m.Dep)
			continue
		}
		artifact, err := s.locate(name)
		if err != nil {
			return err
		}
		if artifact == "" {
			log.Warnf("couldn't find %s anywhere %s; %s stays unresolved", name, s.describe(), m.From)
			continue
		}
		if tier2 && !fo.AutoYes && (fo.Confirm == nil || !fo.Confirm(name, artifact)) {
			log.Warnf("not auto-injecting %s (heuristic match) without confirmation", name)
			continue
		}
		dst := filepath.Join(dir, name)
		if err := copyArtifact(artifact, dst); err != nil {
			return fmt.Errorf("placing %s: %w", artifact, err)
		}
		log.Infof("placed %s into the fixed app (from %s)", name, artifact)
		applied++
	}

	if applied == 0 {
		log.Infof("check %s: found nothing to fix; no output written", filepath.Base(input))
	}

	// Newly added Mach-Os are unsigned — iOS would refuse to load them.
	if applied > 0 {
		if _, err := bundle.FakesignAll(); err != nil {
			return fmt.Errorf("re-signing the fixed app: %w", err)
		}
	}

	// Re-run the check on the fixed bundle, and write the fixed copy when
	// anything was actually changed. Remaining references always surface as
	// an error so `check --fix` exits non-zero when it couldn't finish.
	remaining, err := bundle.CheckReferences()
	if err != nil {
		return err
	}
	if applied > 0 {
		if err := writeFixedOutput(tmpdir, appDir, output, isIPA); err != nil {
			return err
		}
		log.Infof("wrote the fixed copy to %s", output)
	}

	for _, m := range remaining {
		log.Errorf("%s references %s, which is still not in the app bundle", m.From, m.Dep)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("check %s: %d unresolved bundle-relative dependenc(ies) remain after fixing %d",
			filepath.Base(input), len(remaining), applied)
	}
	log.Infof("check %s: fixed %d reference(s) — all bundle-relative dependencies resolve", filepath.Base(input), applied)
	return nil
}

// fixedOutputPath derives the default output name: <base>-fixed<ext> next to
// the input (App.ipa → App-fixed.ipa, App.app → App-fixed.app).
func fixedOutputPath(input string) string {
	ext := filepath.Ext(input)
	base := strings.TrimSuffix(filepath.Base(input), ext)
	return filepath.Join(filepath.Dir(input), base+"-fixed"+ext)
}

// writeFixedOutput repacks the fixed app: an .ipa/.tipa gets zipped back with
// the standard compression; a .app dir is copied to the output path.
func writeFixedOutput(tmpdir, appDir, output string, isIPA bool) error {
	if isIPA {
		return ipa.Repack(tmpdir, output, 6)
	}
	if _, err := os.Stat(output); err == nil {
		os.RemoveAll(output)
	}
	return copyDir(appDir, output)
}

// copyArtifact copies a file or a whole directory tree (framework/bundle) to
// dst. An existing dst is left alone: several references may name the same
// artifact, and the first placement satisfies them all.
func copyArtifact(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyDir(src, dst)
}

// fixSearcher finds missing artifacts across local sources and (optionally)
// the repos. Search order: on-disk artifacts in the fix dirs and the fetch
// cache, then artifacts inside .deb packages those places contain, then the
// Canister/MobileAPT resolver by name (best-effort).
type fixSearcher struct {
	fetch    bool
	dirs     []string // on-disk roots: --fix-dir values + the fetch cache dir
	cache    string   // the persistent fetch cache dir (for the deb pre-filter)
	scratch  string   // persistent staging for artifacts found inside .debs
	searched []string // roots actually searched, for the "searched ..." log
}

// newFixSearcher builds the searcher. scratchRoot is a directory that lives
// for the whole fix call (CheckAndFix's tmpdir): artifacts pulled out of
// .deb packages are staged there so the unpack scratch can be deleted
// without leaving the caller with a dangling path.
func newFixSearcher(fixDirs []string, allowFetch bool, scratchRoot string) *fixSearcher {
	s := &fixSearcher{
		fetch:    allowFetch,
		dirs:     append([]string{}, fixDirs...),
		scratch:  filepath.Join(scratchRoot, "debsearch"),
		searched: fixDirs,
	}
	_ = os.MkdirAll(s.scratch, 0o755)
	if cd, err := fetch.CacheDir(); err == nil && cd != "" {
		s.cache = cd
		s.dirs = append(s.dirs, cd)
	}
	return s
}

func (s *fixSearcher) hasSources() bool {
	return len(s.dirs) > 0
}

func (s *fixSearcher) describe() string {
	if len(s.searched) == 0 {
		if s.fetch {
			return "(only the repos)"
		}
		return "(no local sources)"
	}
	return "(searched " + strings.Join(s.searched, ", ") + ")"
}

// locate returns the path of an artifact whose basename equals name (a
// framework/bundle directory or a dylib file), or "" when nothing matched.
func (s *fixSearcher) locate(name string) (string, error) {
	if p, ok := findNamed(s.dirs, name); ok {
		return p, nil
	}
	for _, dir := range s.dirs {
		// Fix dirs are searched exhaustively; the cache is pre-filtered by
		// name so a large cache isn't fully unpacked per lookup.
		if p, ok := s.findInDebs(dir, name, dir != s.cache); ok {
			return p, nil
		}
	}
	if s.fetch {
		log.Infof("searching the repos for %s (best-effort, may take a moment)...", name)
		paths, err := fetch.Resolve(context.Background(), []string{name}, nil, true, s.cache, nil)
		if err != nil {
			log.Warnf("repo search for %s failed: %v", name, err)
			return "", nil
		}
		for _, debPath := range paths {
			if p, ok := s.searchDeb(debPath, name); ok {
				return p, nil
			}
		}
	}
	return "", nil
}

// findNamed walks the roots looking for a file or directory whose basename
// equals name. The first match wins (roots are searched in order).
func findNamed(roots []string, name string) (string, bool) {
	for _, root := range roots {
		if root == "" {
			continue
		}
		found := ""
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || found != "" {
				return nil
			}
			if d.Name() == name {
				found = p
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			return found, true
		}
	}
	return "", false
}

// findInDebs unpacks every .deb under root and searches inside.
func (s *fixSearcher) findInDebs(root, name string, all bool) (string, bool) {
	if root == "" {
		return "", false
	}
	stem := strings.TrimSuffix(strings.ToLower(name), strings.ToLower(filepath.Ext(name)))
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".deb") &&
			(all || strings.Contains(strings.ToLower(d.Name()), stem)) {
			out = append(out, p)
		}
		return nil
	})
	for _, debPath := range out {
		if p, ok := s.searchDeb(debPath, name); ok {
			return p, true
		}
	}
	return "", false
}

// searchDeb unpacks one .deb to a throwaway scratch dir, looks for the
// artifact, and stages it into the searcher's persistent scratch so the
// returned path outlives the unpack dir (which is deleted on return).
func (s *fixSearcher) searchDeb(debPath, name string) (string, bool) {
	scratch, err := os.MkdirTemp("", "xkvm-debsearch-*")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(scratch)
	if err := deb.Unpack(debPath, scratch); err != nil {
		return "", false
	}
	src, ok := findNamed([]string{scratch}, name)
	if !ok {
		return "", false
	}
	dst := filepath.Join(s.scratch, "found", name)
	if err := copyArtifact(src, dst); err != nil {
		return "", false
	}
	return dst, true
}
