package rootless

import (
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/plist"
)

// TestLaunchDaemonsGoldenReal pins the LaunchDaemons plist branch against
// the REAL sandyd daemon plists from opa334/libSandy (MIT, committed at
// testdata/fixtures/plists/): the same daemon shipped in its rootful and
// rootless variants. Upstream's branch (patch.sh lines 318-320) is a single
// `plutil -convert xml1` + `s|/var/jb/|/|g` when the dirname contains
// /Library/LaunchDaemons:
//
//   - the ROOTLESS variant's ProgramArguments points at
//     /var/jb/usr/local/libexec/sandyd → /usr/local/libexec/sandyd (the
//     jbroot prefix stripped — on roothide the jbroot IS the root, so
//     launchd finds the daemon at the real /usr/local)
//   - the ROOTFUL variant has no /var/jb path → its /usr/local path is
//     untouched, proving the branch never mangles plain /usr paths (and no
//     rootfs dance applies to LaunchDaemons, unlike libSandy)
//
// Pure Go (plists only), so it runs on every CI leg.
func TestLaunchDaemonsGoldenReal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		sha     string
	}{
		{
			name:    "rootless",
			fixture: "com.opa334.sandyd.rootless.plist",
			sha:     "9b858e414ebd81688cf868b813877aed1faead2a0413c18af2b65480b0f58dba",
		},
		{
			name:    "rootful",
			fixture: "com.opa334.sandyd.plist",
			sha:     "fa20343eac583ba674aaea0117a07634e0ff2e30fa44ffe77563b7e107dccd21",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := readPinnedFixture(t, tc.fixture, tc.sha)
			rel := "Library/LaunchDaemons/" + tc.fixture
			data := convertPlistAt(t, t.TempDir(), rel, raw)
			if !strings.HasPrefix(string(data), "<?xml") {
				t.Fatalf("converted plist not XML; first bytes: %q", data[:min(40, len(data))])
			}
			d, err := plist.Decode(data)
			if err != nil {
				t.Fatalf("converted plist unparseable: %v", err)
			}

			// ProgramArguments: the jbroot path strips to the root, the
			// plain /usr/local path is untouched — both land on the real
			// /usr/local/libexec/sandyd.
			args, ok := d["ProgramArguments"].([]any)
			if !ok || len(args) != 1 || args[0] != "/usr/local/libexec/sandyd" {
				t.Errorf("ProgramArguments = %v, want [/usr/local/libexec/sandyd]", d["ProgramArguments"])
			}
			// The daemon's identity and lifecycle keys survive untouched.
			for key, want := range map[string]any{
				"Label":     "com.opa334.sandyd",
				"UserName":  "root",
				"RunAtLoad": true,
				"KeepAlive": false,
			} {
				if got := d[key]; got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
			if mach, ok := d["MachServices"].(map[string]any); !ok || mach["com.opa334.sandyd"] != true {
				t.Errorf("MachServices = %v, want {com.opa334.sandyd: true}", d["MachServices"])
			}

			got := string(data)
			if strings.Contains(got, "/var/jb") {
				t.Errorf("converted plist still contains /var/jb (the LaunchDaemons strip must fire):\n%s", got)
			}
			// LaunchDaemons get NO rootfs dance — a plain /usr path must not
			// become /rootfs/usr.
			if strings.Contains(got, "/rootfs/") {
				t.Errorf("converted plist contains /rootfs/ — the rootfs dance must not apply to LaunchDaemons:\n%s", got)
			}
		})
	}
}
