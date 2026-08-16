package device

import (
	"fmt"
	"regexp"
	"strconv"
)

// Support is the Omega support verdict for an iOS version.
type Support int

const (
	Supported   Support = iota // community-proven range
	Untested                   // future/unreleased: caution required
	Unsupported                // never run here: hard block
)

func (s Support) String() string {
	switch s {
	case Supported:
		return "supported"
	case Untested:
		return "untested"
	default:
		return "not supported"
	}
}

// OmegaPolicy encodes the version windows from two facts, not from
// extrapolation:
//
//  1. Omega's own contract: iOS 16+ required, and iOS 27 restores can reset
//     data/settings (the backup system changed; never run there).
//  2. Apple's released iOS line. Apple adopted year-based versioning at
//     WWDC 2025 and jumped from iOS 18 straight to iOS 26: there is no
//     iOS 19, 20, 21, 22, 23, 24 or 25. Sources: Wikipedia "iOS version
//     history" — "the current major version being iOS 26 which was released
//     on September 15, 2025", "iOS 26 ... the first version of iOS to use
//     Apple's new year-based versioning scheme" (announced June 9, 2025),
//     iOS 27 announced June 8, 2026.
//
// A live xkvm test (full restore + reboot, user-confirmed) verified iOS
// 26.1, so the 26 line is promoted to supported.
var OmegaPolicy = []struct {
	Min     [2]int // inclusive (major, minor)
	Max     [2]int // inclusive
	Verdict Support
	Why     string
}{
	{[2]int{0, 0}, [2]int{15, 999}, Unsupported, "Omega needs iOS 16 or higher — older iOS cannot run this restore"},
	{[2]int{16, 0}, [2]int{18, 999}, Supported, "the range Omega was built and proven on (iOS 16-18)"},
	// No 19-25 row: Apple never released those versions (18 -> 26).
	{[2]int{26, 0}, [2]int{26, 999}, Supported, "live-verified on iOS 26.1: full restore + reboot succeeded"},
	{[2]int{27, 0}, [2]int{27, 999}, Unsupported, "Omega's own hard warning: on iOS 27 the backup system changed, restores can reset your data or settings. Never run it here."},
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

// ParseVersion reads a "major.minor[.patch]" string.
func ParseVersion(s string) ([2]int, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [2]int{}, fmt.Errorf("%q is not an iOS version", s)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return [2]int{maj, min}, nil
}

// OmegaVerdict decides support for a version string, with the reason text.
func OmegaVerdict(version string) (Support, string, error) {
	v, err := ParseVersion(version)
	if err != nil {
		return Unsupported, "", err
	}
	// Apple skipped 19-25 outright (year-based versioning, 18 -> 26).
	if v[0] >= 19 && v[0] <= 25 {
		return Unsupported, "iOS 19-25 were never released: Apple moved from iOS 18 straight to iOS 26 (year-based versioning, 2025). Is the version string a typo?", nil
	}
	for _, row := range OmegaPolicy {
		if lessEq(row.Min, v) && lessEq(v, row.Max) {
			return row.Verdict, row.Why, nil
		}
	}
	// 28+ and anything beyond the table: unreleased, nobody has verified it.
	return Untested, "a future/unreleased iOS version: nobody has verified Omega here yet", nil
}

func lessEq(a, b [2]int) bool {
	return a[0] < b[0] || (a[0] == b[0] && a[1] <= b[1])
}
