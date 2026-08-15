package fetch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cacheTTL is how long a fetched .deb stays in the cache before it is
// considered stale. The cache is pruned lazily on every Resolve call, so
// anything older than 7 days is dropped the next time xkvm fetches.
const cacheTTL = 7 * 24 * time.Hour

// fallbackSweepTimeout bounds the ENTIRE default-repo fallback sweep (not
// each repo): a package that can't be located anywhere costs at most this
// much wall time before resolution fails, no matter how many repos hang.
// The sweep only runs on the miss path (explicit sources + Canister both
// failed), so the common resolved case never pays for it.
const fallbackSweepTimeout = 30 * time.Second

// defaultRepos are the fallback APT repositories swept when a package (or a
// dependency) can't be located through the explicit sources or the Canister
// index. The list tracks the active jailbreak repo scene — the repos on
// ios-repo-updates.com and the broader community — and is consulted only on
// the miss path, so the common Canister-resolved case never pays for it. It
// is a var (not a const) so tests can point it at httptest servers.
var defaultRepos = []string{
	"https://ali7assan.com",
	"https://check0ver.com",
	"https://lenglengyu.com",
	"https://dobabaophuc1706.github.io/repo",
	"https://sileotweak.github.io",
	"https://acreson.github.io/mirror-rootless",
	"https://akusio.github.io",
	"https://repo.alexia.lol",
	"https://alias20.gitlab.io/apt",
	"https://repo.anamy.gay",
	"http://apt.thebigboss.org/repofiles/cydia",
	"https://bvnsupport.github.io",
	"https://repo.chariz.com",
	"https://creaturecoding.com/repo",
	"https://cydiageek.yourepo.com",
	"https://apt.cydiakk.com",
	"https://repo.cypwn.xyz",
	"https://dcsyhi1998.github.io",
	"https://dhinakg.github.io/repo",
	"https://ellekit.space",
	"https://nahtedetihw.github.io",
	"http://apt.fouadraheb.com",
	"https://frcoal.cfd",
	"https://ginsu.dev/repo",
	"https://havoc.app",
	"https://cydia.ichitaso.com",
	"https://repo.icrazeios.com",
	"http://ishqip.xyz",
	"https://ib-soft.net/cydia/beta",
	"https://ib-soft.net/repo/beta",
	"https://ib-soft.net/repo",
	"https://ib-soft.net/cydia",
	"https://ios.jjolano.me",
	"https://level3tjg.me/repo",
	"https://limneos.net/repo",
	"https://34306.github.io",
	"https://lizynz.github.io",
	"https://lzsxcl.github.io/repo",
	"https://maxiwee.de",
	"https://michaelmelita1.github.io",
	"https://miro92.com/repo",
	"https://miticollo.github.io/repos/my",
	"https://nathan4s.lol/repo",
	"https://repo.niceios.com",
	"https://now4u2kid.github.io",
	"https://opa334.github.io",
	"https://p2kdev.github.io/repo",
	"https://paisseon.github.io",
	"https://repo.palera.in",
	"https://poomsmart.github.io/repo",
	"https://apt.procurs.us",
	"https://cydia.rob311.com/repo",
	"https://roothide.github.io",
	"https://repo.getsileo.app",
	"https://skypain.github.io/repo",
	"https://0xilis.github.io/rootless",
	"https://sparkdev.me",
	"https://sugiuta.github.io/repo",
	"https://apt.sutuplus.com",
	"https://tigisoftware.com/repo",
	"http://tigisoftware.com/cydia",
	"https://apt.tinyapps.cn",
	"http://repo.thuthuatjb.com",
	"https://apt.uar.no",
	"https://www.yourepo.com",
	"https://invalidunit.github.io/repo",
	"https://zerui18.github.io/zx02",
	"https://lclrc.github.io/repo",
	"https://xiangfeidexiaohuo.github.io",
	"https://apt.wxhbts.pro",
	"https://byg.iosios.net",
	"https://apt.htv123.com",
	"https://apt.25mao.com",
	"https://repo.snailovet.com",
	// Classic/legacy scene batch: the repos that carried the golden-era
	// tweaks — Chariz-era communities, Packix, Twickd, hackyouriphone,
	// iSecureOS, and the smaller personal repos still serving today.
	"https://repo.1conan.com",
	"https://apt.alfhaily.me",
	"https://repo.appweleux.com",
	"https://cokepokes.github.io",
	"http://getdelta.co",
	"https://repo.dynastic.co",
	// https variants of hosts already listed over plain http: the sweep
	// tries both, so a repo that serves either scheme resolves.
	"https://apt.fouadraheb.com",
	"https://repo.hackyouriphone.org",
	"https://haoict.github.io/cydia",
	"https://isecureos.idevicecentral.com/repo",
	"https://julio.hackyouriphone.org",
	"https://julioverne.github.io",
	"https://whoskanji.github.io",
	"https://cydia.akemi.ai",
	"https://repo.co.kr",
	"https://myxxdev.github.io",
	"https://repo.nullpixel.uk",
	"https://repo.packix.com",
	"https://rejail.ru",
	"https://sarahh12099.github.io/repo",
	"https://slyfabi.github.io",
	"https://repo.danielpitra.cz",
	"https://tigisoftware.com/cydia",
	"https://repo.twickd.com",
	"https://alo.works",
	"https://apptapp.me/repo",
	"https://arandomdev.github.io/repo",
	"https://apt.arx8x.net",
	"https://creaturesurvive.github.io",
	"https://apt.geometricsoftware.se",
	"https://greg0109.github.io/repo",
	"https://hndrk.yourepo.com",
	"https://repo.litten.love",
	"https://xenpublic.incendo.ws",
	"https://alexpng.github.io/Nepeta-Mirror",
	"https://global.niceios.com",
	"https://profejuantonio.github.io",
	"https://ryannair05.github.io/repo",
	"https://shiftcmdk.github.io/repo",
}

// CacheDir returns the persistent fetch cache directory
// (os.UserCacheDir()/xkvm/fetch, ~/.cache/xkvm/fetch on systems without a
// user cache dir). Fetched dependency .debs live here so a later run that
// needs the same package reuses the download instead of hitting the network,
// and stale entries are pruned by the 7-day TTL on every Resolve.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			base = filepath.Join(home, ".cache")
		} else {
			return "", err
		}
	}
	return filepath.Join(base, "xkvm", "fetch"), nil
}

// cacheFileName maps a package id + version to a cache file name. Versioned
// names let two versions of the same package coexist (a version bump is a
// cache miss, not a silent overwrite). The version is sanitized to
// filename-safe characters; an empty version falls back to the bare id name.
func cacheFileName(id, version string) string {
	if version == "" {
		return id + ".deb"
	}
	var b strings.Builder
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '+', r == '-', r == '~':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return id + "__" + b.String() + ".deb"
}

// pruneCache removes cached .deb files older than cacheTTL (measured from
// the file's modification time, i.e. when it was written). Best-effort: an
// unreadable cache directory is simply left alone.
func pruneCache(dir string, now time.Time) {
	pruneExpiredAt(dir, now)
}

// pruneExpiredAt is pruneCache returning how many files were removed.
func pruneExpiredAt(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := now.Add(-cacheTTL)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// CacheUsage reports the persistent cache directory and what's in it: the
// number of cached .debs and their total size. Used by `xkvm cache` and the
// TUI cache menu so users can see what the smart dependency solver has
// downloaded without digging through the filesystem.
func CacheUsage() (dir string, debs int, bytes int64, err error) {
	dir, err = CacheDir()
	if err != nil {
		return "", 0, 0, err
	}
	debs, bytes, err = countCache(dir)
	return dir, debs, bytes, err
}

// countCache counts the cached .debs in dir and their total size. A missing
// directory counts as an empty cache, not an error.
func countCache(dir string) (debs int, bytes int64, err error) {
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, 0, nil
		}
		return 0, 0, rerr
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			debs++
			bytes += info.Size()
		}
	}
	return debs, bytes, nil
}

// PruneExpired removes cached .debs older than the 7-day TTL and returns how
// many were dropped. Missing cache directory is not an error — there's
// nothing to prune. Used by `xkvm cache --clear` and the TUI cache menu.
func PruneExpired() (removed int, err error) {
	dir, err := CacheDir()
	if err != nil {
		return 0, err
	}
	return pruneExpiredAt(dir, time.Now()), nil
}

// ClearCache removes EVERY cached .deb, regardless of age, and returns how
// many were removed. The explicit "forget everything" — distinct from
// PruneExpired's 7-day TTL sweep.
func ClearCache() (removed int, err error) {
	dir, err := CacheDir()
	if err != nil {
		return 0, err
	}
	return clearCacheDir(dir), nil
}

// clearCacheDir removes every .deb in dir (regardless of age) and returns
// how many were removed.
func clearCacheDir(dir string) int {
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".deb") {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed
}

// HumanBytes renders a byte count readably (B / KB / MB / GB, one decimal).
// Shared by `xkvm cache` and the TUI cache menu.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
