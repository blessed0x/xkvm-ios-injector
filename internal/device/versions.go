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
	Untested                   // newer than the proven range: caution required
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

// OmegaPolicy encodes the version window from Omega's own contract: the
// README requires iOS 16+ and hard-warns that iOS 27 restores can reset
// data/settings (Apple changed the backup system). The supported window is
// the era the tool was built and exercised in (16-18); everything newer up
// to 26 gets the untested caution. One table, one place to update.
var OmegaPolicy = []struct {
	Min     [2]int // inclusive (major, minor)
	Max     [2]int // inclusive
	Verdict Support
	Why     string
}{
	{[2]int{0, 0}, [2]int{15, 999}, Unsupported, "Omega needs iOS 16 or higher — older iOS cannot run this restore"},
	{[2]int{16, 0}, [2]int{18, 999}, Supported, "the range Omega was built and proven on (iOS 16-18)"},
	{[2]int{19, 0}, [2]int{26, 999}, Untested, "newer than the proven Omega range — nobody on record has verified it here yet"},
	{[2]int{27, 0}, [2]int{99, 999}, Unsupported, "Omega's own hard warning: on iOS 27 the backup system changed, restores can reset your data or settings. Never run it here."},
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
	for _, row := range OmegaPolicy {
		if lessEq(row.Min, v) && lessEq(v, row.Max) {
			return row.Verdict, row.Why, nil
		}
	}
	return Unsupported, "no policy row covers this version", nil
}

func lessEq(a, b [2]int) bool {
	return a[0] < b[0] || (a[0] == b[0] && a[1] <= b[1])
}
