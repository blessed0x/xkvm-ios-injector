package fetch

import "strings"

// coreDeps are system packages that can never be downloaded as tweak .debs
// (Azule's filter, confirmed in ARCHITECTURE.md §2.2). They are either part
// of the OS, of the jailbreak runtime, or handled by xkvm's own
// common-dependency fixing at injection time (substrate/ElleKit/Orion...).
var coreDeps = map[string]bool{
	"com.ex.substitute":      true,
	"org.coolstar.libhooker": true,
	"mobilesubstrate":        true,
	"coreutils":              true,
	"firmware":               true,
	"cy+cpu.arm64":           true,
	"gsc.ipad":               true,
}

// parseDepends splits a Depends: value into alternative groups. Each group
// is an ordered list of package ids with version constraints stripped;
// installing any one member satisfies the group. For example
// "a (>= 1.0), b | c (<< 2), d" becomes [][]string{{"a"}, {"b", "c"}, {"d"}}.
func parseDepends(s string) [][]string {
	var groups [][]string
	for _, group := range strings.Split(s, ",") {
		var alts []string
		for _, alt := range strings.Split(group, "|") {
			name := stripConstraint(alt)
			if name == "" {
				continue
			}
			alts = append(alts, name)
		}
		if len(alts) > 0 {
			groups = append(groups, alts)
		}
	}
	return groups
}

// stripConstraint removes a trailing "(...)" version constraint and any
// surrounding whitespace from a single dependency token.
func stripConstraint(tok string) string {
	tok = strings.TrimSpace(tok)
	if i := strings.IndexByte(tok, '('); i >= 0 {
		tok = strings.TrimSpace(tok[:i])
	}
	// Drop a multiarch qualifier ("a:any") so it isn't resolved as a
	// literal package id.
	if i := strings.IndexByte(tok, ':'); i >= 0 {
		tok = tok[:i]
	}
	return tok
}

// isCoreDep reports whether id is a system package that must never be
// fetched as a tweak.
func isCoreDep(id string) bool { return coreDeps[id] }
