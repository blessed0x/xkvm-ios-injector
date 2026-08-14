package rootless

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/deb"
)

// buildControlFixture builds a rootless deb (payload under var/jb) with the
// given control body and an optional set of .DS_Store files relative to the
// staging root. Pure Go (no Mach-O), so the tests run on every CI leg.
func buildControlFixture(t *testing.T, tmp, control string, dsStores ...string) string {
	t.Helper()
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range dsStores {
		full := filepath.Join(staging, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("Finder droppings"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(staging, "var", "jb", "usr", "lib"), 0o755)
	if err := os.WriteFile(filepath.Join(staging, "var", "jb", "usr", "lib", "tweak.conf"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(tmp, "rootless.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	return in
}

// TestRoothideDSStoreCleanup pins upstream's `find ... -name ".DS_Store"
// -delete` before the repack (patch.sh line 178): macOS-built debs ship
// Finder droppings in payload dirs (and at the package root), and none may
// reach the output package — including inside the pkgmirror snapshot.
func TestRoothideDSStoreCleanup(t *testing.T) {
	tmp := t.TempDir()
	control := "Package: dsstoretweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: ds store test\nMaintainer: xkvm\n"
	in := buildControlFixture(t, tmp, control,
		"var/jb/Library/.DS_Store", // payload dir
		"var/jb/usr/lib/.DS_Store", // payload dir
		".DS_Store",                // package root (loose → rootfs/ after hoist)
	)

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, true, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	var survivors []string
	err := filepath.WalkDir(unpacked, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".DS_Store" {
			survivors = append(survivors, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(survivors) > 0 {
		t.Errorf("output package still contains .DS_Store entries: %v", survivors)
	}
	// The normal payload file survives the sweep.
	if _, err := os.Stat(filepath.Join(unpacked, "usr", "lib", "tweak.conf")); err != nil {
		t.Errorf("payload file missing after sweep: %v", err)
	}
}

// TestRoothideControlBlankLines pins the control round-trip's blank-line
// stripping — the equivalent of upstream's `sed -i '/^$/d'` (line 174),
// which runs before the arch rewrite. parseControl drops empty lines, so a
// control that ships blank lines comes out of the conversion without them
// and with every field intact.
func TestRoothideControlBlankLines(t *testing.T) {
	tmp := t.TempDir()
	control := "Package: blanktweak\nVersion: 1.0\n\nArchitecture: iphoneos-arm64\n\n\nDepends: mobilesubstrate\nDescription: blank line test\nMaintainer: xkvm\n"
	in := buildControlFixture(t, tmp, control)

	out := filepath.Join(tmp, "roothide.deb")
	if err := ConvertToRoothide(in, out, false, ""); err != nil {
		t.Fatalf("ConvertToRoothide: %v", err)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(ctl)
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			continue // the file's trailing newline, not a blank line
		}
		if strings.TrimSpace(line) == "" {
			t.Errorf("control contains a blank line after conversion; got:\n%q", got)
		}
	}
	for _, want := range []string{
		"Package: blanktweak",
		"Version: 1.0",
		"Architecture: iphoneos-arm64e",
		"Depends: mobilesubstrate",
		"Description: blank line test",
		"Maintainer: xkvm",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("control missing %q after round-trip; got:\n%s", want, got)
		}
	}
}

// TestRoothideArchGate pins upstream's input contract: only iphoneos-arm64
// (rootless) packages convert — a rootful (iphoneos-arm) or already-roothide
// (iphoneos-arm64e) control errors instead of silently producing a broken
// hoist (`[ $DEB_ARCH != "iphoneos-arm64" ]` → exit 1 upstream).
func TestRoothideArchGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arch    string
		wantErr string
	}{
		{"rootful", "iphoneos-arm", "not a rootless package"},
		{"already-roothide", "iphoneos-arm64e", "not a rootless package"},
		{"unknown", "all", "not a rootless package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			control := "Package: gatetweak\nVersion: 1.0\nArchitecture: " + tc.arch + "\nDepends: mobilesubstrate\nDescription: gate test\nMaintainer: xkvm\n"
			in := buildControlFixture(t, tmp, control)
			out := filepath.Join(tmp, "roothide.deb")
			err := ConvertToRoothide(in, out, false, "")
			if err == nil {
				t.Fatalf("ConvertToRoothide(%s) succeeded, want error", tc.arch)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q missing %q", err, tc.wantErr)
			}
		})
	}
}

// TestRoothideConflictsMangle pins upstream's Conflicts rewrite
// (`sed -i '/^Conflicts: /s/roothide/r-o-o-t-l-e-s-s-/g'`, patch.sh line
// 322) — the field's "roothide" substrings mangle to r-o-o-t-l-e-s-s- so the
// package can coexist with a stock roothide install. Surfaced by the
// member-by-member comparison against upstream: the comparison input deb
// carries no Conflicts line, and this branch had zero test coverage.
func TestRoothideConflictsMangle(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf string // full Conflicts line, or "" for none
		want string // expected Conflicts line after conversion, or "" for none
	}{
		{"plain roothide", "Conflicts: roothide", "Conflicts: r-o-o-t-l-e-s-s-"},
		{"multi-value", "Conflicts: roothide, com.example.other", "Conflicts: r-o-o-t-l-e-s-s-, com.example.other"},
		{"substring semantics", "Conflicts: roothide, roothide2", "Conflicts: r-o-o-t-l-e-s-s-, r-o-o-t-l-e-s-s-2"},
		{"no roothide", "Conflicts: com.example.other", "Conflicts: com.example.other"},
		{"no Conflicts line", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			control := "Package: conflictstweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: conflicts mangle test\nMaintainer: xkvm\n"
			if tc.conf != "" {
				control += tc.conf + "\n"
			}
			in := buildControlFixture(t, tmp, control)
			out := filepath.Join(tmp, "roothide.deb")
			if err := ConvertToRoothide(in, out, false, ""); err != nil {
				t.Fatalf("ConvertToRoothide: %v", err)
			}
			unpacked := filepath.Join(tmp, "unpacked")
			if err := deb.Unpack(out, unpacked); err != nil {
				t.Fatal(err)
			}
			ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(ctl)
			if tc.want == "" {
				if strings.Contains(got, "Conflicts") {
					t.Errorf("control gained a Conflicts line; got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want+"\n") {
				t.Errorf("control missing %q after conversion; got:\n%s", tc.want, got)
			}
		})
	}
}

// TestRoothideDynamicControlLayout pins the dynamic-mode control edits
// against upstream's DynamicPatches block (patch.sh): the Version line is
// MOVED to the end of the file (`sed -i "/^Version\:/d"` + append) with the
// ~roothide suffix, and the patches-<pkg> Pre-Depends is PREPENDED before
// any existing Pre-Depends value (`s/^Pre-Depends\:/Pre-Depends:
// $PreDepends,/`). Both surfaced by the member-by-member comparison of
// --mode dynamic against upstream.
func TestRoothideDynamicControlLayout(t *testing.T) {
	for _, tc := range []struct {
		name       string
		preDepends string   // input Pre-Depends line, or "" for none
		wantLines  []string // expected tail of the converted control
	}{
		{
			name:       "no existing Pre-Depends",
			preDepends: "",
			// Upstream appends Version, then the Pre-Depends.
			wantLines: []string{
				"Version: 1.0~roothide",
				"Pre-Depends: patches-dynlayout(= 1.0~roothide)",
			},
		},
		{
			name:       "existing Pre-Depends",
			preDepends: "Pre-Depends: mobilesubstrate (>= 0.9.5000)",
			// The existing Pre-Depends is edited in place (new dep first),
			// Version moves to the very end.
			wantLines: []string{
				"Version: 1.0~roothide",
			},
			// Pre-Depends assertion is separate (it stays in position).
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			control := "Package: dynlayout\nVersion: 1.0\nArchitecture: iphoneos-arm64\n"
			if tc.preDepends != "" {
				control += tc.preDepends + "\n"
			}
			control += "Depends: mobilesubstrate\nDescription: dynamic layout test\nMaintainer: xkvm\n"
			in := buildControlFixture(t, tmp, control)
			out := filepath.Join(tmp, "roothide.deb")
			if err := ConvertToRoothide(in, out, false, "dynamic"); err != nil {
				t.Fatalf("ConvertToRoothide: %v", err)
			}
			unpacked := filepath.Join(tmp, "unpacked")
			if err := deb.Unpack(out, unpacked); err != nil {
				t.Fatal(err)
			}
			ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
			if err != nil {
				t.Fatal(err)
			}
			got := string(ctl)
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			for i, want := range tc.wantLines {
				if lines[len(lines)-len(tc.wantLines)+i] != want {
					t.Errorf("control tail line %d = %q, want %q; got:\n%s", len(lines)-len(tc.wantLines)+i, lines[len(lines)-len(tc.wantLines)+i], want, got)
				}
			}
			if tc.name == "existing Pre-Depends" {
				if !strings.Contains(got, "Pre-Depends: patches-dynlayout(= 1.0~roothide), mobilesubstrate (>= 0.9.5000)\n") {
					t.Errorf("existing Pre-Depends not prepended new-first; got:\n%s", got)
				}
			}
		})
	}
}
