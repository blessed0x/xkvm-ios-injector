package rootless

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/deb"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/testutil"
)

// TestShouldConvert pins the rootless-patcher conversion semantics: first
// path component must be a bootstrap root, special cases and file:// force
// conversion, blacklisted substrings veto (checked last).
func TestShouldConvert(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Bootstrap roots convert.
		{"/Library/MobileSubstrate/DynamicLibraries/X.dylib", true},
		{"/Library/PreferenceLoader/Preferences/X", true},
		{"/usr/lib/libsubstrate.dylib", true},
		{"/usr/lib/libsubstitute.dylib", true},
		{"/Applications/Foo.app", true},
		{"/var/mobile/Library/Preferences/MyTweakPrefs.plist", true},
		{"/Library", true},
		{"Library/Foo", true}, // relative form also converts (prefix var/jb/)
		// Apple system libraries never convert (load-command-layer additions
		// to the upstream blacklist — upstream scans __cstring where these
		// never appear).
		{"/usr/lib/libSystem.B.dylib", false},
		{"/usr/lib/libSystem.tbd", false},
		{"/usr/lib/libobjc.A.dylib", false},
		{"/usr/lib/libc++.1.dylib", false},
		{"/usr/lib/libz.1.dylib", false},
		{"/usr/lib/libsqlite3.dylib", false},
		{"/usr/lib/libcompression.dylib", false},
		{"/usr/lib/swift/libswiftCore.dylib", false},
		// Upstream blacklist entries.
		{"/System/Library/Frameworks/X.framework/X", false},
		{"/usr/lib/dyld", false},
		{"/var/mobile/Library/Preferences/com.apple.foo.plist", false},
		{"/var/jb/Library/MobileSubstrate/DynamicLibraries/X.dylib", false},
		{"/Library/Wallpaper", false},
		// Non-bootstrap roots and bare names.
		{"@rpath/Local.framework/Local", false},
		{"@executable_path/X.dylib", false},
		{"@loader_path/X.dylib", false},
		{"X.framework/X", false},
		{"", false},
		// Single-component strings never convert (upstream's
		// [pathComponents count] == 1 rejection), even bootstrap-named ones.
		{"usr", false},
		{"var", false},
		{"Library", false},
		// file:// forces conversion.
		{"file:///Library/Foo", true},
		// "private" is not a bootstrap root, so this is rejected BEFORE the
		// special case can fire — faithful to upstream's check order.
		{"/private/var/mobile/Library/Preferences/X.plist", false},
		// Special cases fire only when the first component is a bootstrap root.
		{"/var/tmp/x", true},
	}
	for _, c := range cases {
		if got := ShouldConvert(c.in); got != c.want {
			t.Errorf("ShouldConvert(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestConvertString pins the rewriting itself: special cases first, then
// file:// / absolute / relative prefixing.
func TestConvertString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Library/MobileSubstrate/DynamicLibraries/X.dylib", "/var/jb/Library/MobileSubstrate/DynamicLibraries/X.dylib"},
		{"/usr/lib/libsubstrate.dylib", "/var/jb/usr/lib/libsubstrate.dylib"},
		{"Library/Foo", "var/jb/Library/Foo"},
		{"file:///Library/Foo", "file:///var/jb/Library/Foo"},
		// Special cases apply before the prefix (upstream order).
		{"/private/var/mobile/Library/Preferences/X.plist", "/var/jb/var/mobile/Library/Preferences/X.plist"},
		{"/var/tmp/x", "/var/jb/tmp/x"},
	}
	for _, c := range cases {
		if got := ConvertString(c.in); got != c.want {
			t.Errorf("ConvertString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestConvertEndToEnd runs the full rootful→rootless conversion on a real
// Mach-O: a tweak dylib carrying substrate-style dependencies. Verifies the
// payload lands under var/jb, load-command paths convert, Apple system libs
// and @rpath deps are untouched, and the control file is edited.
func TestConvertEndToEnd(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	tweak := testutil.MakeTweak(t, tmp, "CoolTweak")
	b := macho.Bin{Path: tweak}
	// The fixture tweak links the classic rootful runtime set (weak, so the
	// missing libraries don't matter for the test) plus a sibling tweak and
	// an @rpath framework that must NOT convert.
	for _, dep := range []string{
		"/usr/lib/libsubstrate.dylib",
		"/Library/MobileSubstrate/DynamicLibraries/Sibling.dylib",
		"@rpath/Local.framework/Local",
	} {
		if err := b.InjectWeak(dep); err != nil {
			t.Fatalf("InjectWeak(%s): %v", dep, err)
		}
	}

	// Build a rootful deb: control + the tweak in DynamicLibraries.
	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	control := "Package: cooltweak\nVersion: 1.0\nArchitecture: iphoneos-arm64\nDepends: mobilesubstrate\nDescription: test tweak\nMaintainer: xkvm\n"
	if err := os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte(control), 0o644); err != nil {
		t.Fatal(err)
	}
	payloadDir := filepath.Join(staging, "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(payloadDir, 0o755)
	if err := copyFile(t, tweak, filepath.Join(payloadDir, "CoolTweak.dylib")); err != nil {
		t.Fatal(err)
	}
	rootful := filepath.Join(tmp, "rootful.deb")
	if err := deb.Build(staging, rootful); err != nil {
		t.Fatalf("Build: %v", err)
	}

	out := filepath.Join(tmp, "rootless.deb")
	if err := Convert(rootful, out, false, false); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// Payload moved under var/jb; DEBIAN stays at the top.
	converted := filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "CoolTweak.dylib")
	if _, err := os.Stat(converted); err != nil {
		t.Fatalf("converted payload missing at %s: %v", converted, err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "DEBIAN", "control")); err != nil {
		t.Fatalf("DEBIAN/control missing at top level: %v", err)
	}

	// Load commands: jailbreak paths converted, Apple/@rpath untouched.
	// AllDependencies (unfiltered) is used because Dependencies applies
	// cyan's /Library/, /usr/lib/, @ starter rule, which would hide the
	// converted /var/jb/... paths.
	deps, err := macho.Bin{Path: converted}.AllDependencies()
	if err != nil {
		t.Fatalf("AllDependencies: %v", err)
	}
	got := strings.Join(deps, "\n")
	for _, want := range []string{
		"/var/jb/usr/lib/libsubstrate.dylib",
		"/var/jb/Library/MobileSubstrate/DynamicLibraries/Sibling.dylib",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("converted deps missing %q; got:\n%s", want, got)
		}
	}
	for _, mustNot := range []string{
		"/var/jb/usr/lib/libSystem.B.dylib",
		"/var/jb/@rpath/Local.framework/Local",
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("dep %q must not be rewritten; got:\n%s", mustNot, got)
		}
	}
	if !strings.Contains(got, "/usr/lib/libSystem.B.dylib") {
		t.Errorf("Apple libSystem dep was rewritten; got:\n%s", got)
	}
	if !strings.Contains(got, "@rpath/Local.framework/Local") {
		t.Errorf("@rpath dep was rewritten; got:\n%s", got)
	}

	// Control edits: Architecture and the rootless runtime dependency.
	ctlData, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	ctl := string(ctlData)
	if !strings.Contains(ctl, "Architecture: iphoneos-arm64") {
		t.Errorf("control missing iphoneos-arm64; got:\n%s", ctl)
	}
	if !strings.Contains(ctl, RuntimeDep) {
		t.Errorf("control missing %s; got:\n%s", RuntimeDep, ctl)
	}
	if !strings.Contains(ctl, "Package: cooltweak") {
		t.Errorf("control lost the package id; got:\n%s", ctl)
	}
}

// TestConvertAlreadyRootless: a deb whose payload is already under var/jb is
// rebuilt unchanged (no double-nesting).
func TestConvertAlreadyRootless(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()

	staging := filepath.Join(tmp, "staging")
	os.MkdirAll(filepath.Join(staging, "DEBIAN"), 0o755)
	os.WriteFile(filepath.Join(staging, "DEBIAN", "control"), []byte("Package: x\nVersion: 1\nArchitecture: iphoneos-arm64\n"), 0o644)
	jbPayload := filepath.Join(staging, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")
	os.MkdirAll(jbPayload, 0o755)
	os.WriteFile(filepath.Join(jbPayload, "X.dylib"), []byte("already-rootless"), 0o644)

	in := filepath.Join(tmp, "in.deb")
	if err := deb.Build(staging, in); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "out.deb")
	if err := Convert(in, out, false, false); err != nil {
		t.Fatalf("Convert: %v", err)
	}

	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatal(err)
	}
	// Payload still exactly one var/jb deep; not var/jb/var/jb.
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "X.dylib")); err != nil {
		t.Fatalf("payload missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "var")); err == nil {
		t.Fatal("payload was double-nested under var/jb/var/jb")
	}
}

func copyFile(t *testing.T, src, dst string) error {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
