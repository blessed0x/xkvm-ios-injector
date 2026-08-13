package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xkvm/xkvm/internal/app"
	"github.com/xkvm/xkvm/internal/cyanfile"
	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/macho"
	"github.com/xkvm/xkvm/internal/testutil"
)

// runCapturesOptions returns a Runner that records the parsed options.
func runCapturesOptions(t *testing.T, got *app.Options) Runner {
	t.Helper()
	return func(_ context.Context, opts *app.Options) error {
		*got = *opts
		return nil
	}
}

func TestRootParsesFullFlagSurface(t *testing.T) {
	var got app.Options
	cmd := NewRootCmd(runCapturesOptions(t, &got))

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"-i", "/tmp/in.ipa",
		"-o", "/tmp/out.ipa",
		"-z", "a.cyan", "-z", "b.cyan",
		"-f", "tweak.deb", "-f", "lib.dylib",
		"-n", "TestApp", "-v", "2.0", "-b", "com.xkvm.test", "-m", "14.0",
		"-k", "icon.png", "-l", "merge.plist", "-x", "ent.plist",
		"-u", "-w", "-d", "-s", "-q", "-e", "-g",
		"-c", "3",
		"--ignore-encrypted", "--overwrite", "--silent",
		"--ellekit", "--liquid-glass", "--liquid-glass-compat",
		"--fetch", "com.example.tweak", "-A", "https://repo.example.com",
		"--no-recurse", "--country", "US",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	want := app.Options{
		Input: "/tmp/in.ipa", Output: "/tmp/out.ipa",
		Cyans: []string{"a.cyan", "b.cyan"}, Files: []string{"tweak.deb", "lib.dylib"},
		Name: "TestApp", Version: "2.0", BundleID: "com.xkvm.test", MinimumOS: "14.0",
		Icon: "icon.png", PlistMerge: "merge.plist", Entitlements: "ent.plist",
		RemoveSupportedDevices: true, NoWatch: true, EnableDocuments: true,
		Fakesign: true, Thin: true, RemoveExtensions: true, RemoveEncrypted: true,
		Compress: 3, IgnoreEncrypted: true, Overwrite: true,
		ElleKit: true,
		Patches: []string{"liquid-glass", "liquid-glass-compat"},
		Fetch:   []string{"com.example.tweak"}, APTSource: []string{"https://repo.example.com"},
		NoRecurse: true, Country: "US",
	}
	if !optsEqual(got, want) {
		t.Errorf("parsed options mismatch:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestRootAcceptsSpaceSeparatedArrayValues(t *testing.T) {
	// cyan's nargs="+" lets a single -f consume space-separated values;
	// trailing positionals must fold into the last array flag.
	var got app.Options
	cmd := NewRootCmd(runCapturesOptions(t, &got))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-i", "/tmp/x.ipa", "-f", "a.deb", "b.dylib", "c.framework"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := []string{"a.deb", "b.dylib", "c.framework"}
	if !reflect.DeepEqual(got.Files, want) {
		t.Errorf("Files = %v, want %v", got.Files, want)
	}
}

func TestRootDecryptSpaceSeparatedForm(t *testing.T) {
	// The documented `--decrypt <apple-id> <password>` form must work.
	var got app.Options
	cmd := NewRootCmd(runCapturesOptions(t, &got))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-i", "/tmp/x.ipa", "--decrypt", "me@example.com", "hunter2"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := []string{"me@example.com", "hunter2"}
	if !reflect.DeepEqual(got.Decrypt, want) {
		t.Errorf("Decrypt = %v, want %v", got.Decrypt, want)
	}
}

func TestRootRequiresInput(t *testing.T) {
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for missing required -i")
	}
}

func TestRootVersionFlag(t *testing.T) {
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(out.String(), "xkvm v"+app.Version) {
		t.Errorf("version output = %q, want it to contain xkvm v%s", out.String(), app.Version)
	}
}

func TestRootPatchFlagsCollectedInRegistryOrder(t *testing.T) {
	// Enabled patch flags must land in opts.Patches in registry (sorted)
	// order regardless of the order they were passed, so patch.Apply runs
	// deterministically (spec D10).
	var got app.Options
	cmd := NewRootCmd(runCapturesOptions(t, &got))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-i", "/tmp/x.ipa", "--liquid-glass-compat", "--liquid-glass"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := []string{"liquid-glass", "liquid-glass-compat"}
	if !reflect.DeepEqual(got.Patches, want) {
		t.Errorf("Patches = %v, want %v (registry-sorted)", got.Patches, want)
	}
}

func TestRootVShortIsAppVersion(t *testing.T) {
	// -v must stay free for the app-version modifier (cyan semantics), not
	// become cobra's --version shorthand.
	var got app.Options
	cmd := NewRootCmd(runCapturesOptions(t, &got))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-i", "/tmp/x.ipa", "-v", "9.9"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.Version != "9.9" {
		t.Errorf("Options.Version = %q, want 9.9", got.Version)
	}
}

func TestExtractCmd(t *testing.T) {
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"extract", "-i", "/tmp/nope.ipa", "-o", "/tmp/out"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for a nonexistent input")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention the missing input", err)
	}
}

func TestCGenRequiresOutput(t *testing.T) {
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cgen"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected cgen without -o to error")
	}
}

// TestCGenGeneratesRootDylibConfig exercises cgen end to end: it must write a
// .cyan archive whose config.json carries root_dylibs and whose inject/ holds
// the payloads. This is the round-trip guarantee behind configs that mark
// root dylibs.
func TestCGenGeneratesRootDylibConfig(t *testing.T) {
	tmp := t.TempDir()
	tweak := filepath.Join(tmp, "Regram.dylib")
	if err := os.WriteFile(tweak, []byte("fake-dylib"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "patch.cyan")

	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd.SetArgs([]string{"cgen", "-o", out, "-f", tweak, "--root-dylib", tweak, "-n", "App", "-s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cgen: %v", err)
	}

	if _, err := os.Stat(out); err != nil {
		t.Fatalf("cgen output missing: %v", err)
	}
	cfg, err := cyanfile.Parse(out, filepath.Join(tmp, "parsed"))
	if err != nil {
		t.Fatalf("parsing generated config: %v", err)
	}
	if len(cfg.RootDylibs) != 1 || filepath.Base(cfg.RootDylibs[0]) != "Regram.dylib" {
		t.Errorf("RootDylibs = %v, want [Regram.dylib]", cfg.RootDylibs)
	}
	if cfg.Name != "App" || !cfg.Fakesign {
		t.Errorf("baked scalars wrong: name=%q fakesign=%v", cfg.Name, cfg.Fakesign)
	}
}

// writeCyan is a minimal .cyan archive builder for cyan-check tests.
func writeCyan(t *testing.T, path, config string, entries map[string]string) {
	t.Helper()
	zf, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create("config.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCyanCheckValidExitsZero(t *testing.T) {
	tmp := t.TempDir()
	ok := filepath.Join(tmp, "ok.cyan")
	writeCyan(t, ok, `{"f": true, "root_dylibs": ["R.dylib"], "s": true}`, map[string]string{
		"inject/R.dylib": "r",
	})
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd.SetArgs([]string{"cyan-check", ok})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("valid config should pass: %v", err)
	}
}

func TestCyanCheckInvalidExitsNonZero(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "bad.cyan")
	writeCyan(t, bad, `{"f": true, "root_dylibs": ["Nope.dylib"]}`, map[string]string{
		"inject/R.dylib": "r",
	})
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd.SetArgs([]string{"cyan-check", bad})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected cyan-check to fail on a root_dylibs mismatch")
	}
	if !strings.Contains(err.Error(), "cyan-check") {
		t.Errorf("error = %q, want a cyan-check summary", err.Error())
	}
}

func TestCyanCheckWarningsOnlyExitsZero(t *testing.T) {
	tmp := t.TempDir()
	warn := filepath.Join(tmp, "warn.cyan")
	// Unknown key + f without payloads are warnings, not errors.
	writeCyan(t, warn, `{"future_feature": true}`, nil)
	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd.SetArgs([]string{"cyan-check", warn})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("warning-only config should pass: %v", err)
	}
}

// TestCheckCmdExitCodes: `xkvm check` on an app with an unresolved
// bundle-relative dependency must exit non-zero; a complete app exits 0.
// Native-toolchain gated: the fixtures are real Mach-O binaries.
func TestCheckCmdExitCodes(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	log.SetSilent(true)
	t.Cleanup(func() { log.SetSilent(false) })
	tmp := t.TempDir()

	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")
	if err := os.MkdirAll(filepath.Join(appDir, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	tweak := testutil.MakeTweak(t, tmp, "RyukGram")
	if err := (macho.Bin{Path: tweak}).InjectWeak("@rpath/ffmpegkit.framework/ffmpegkit"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tweak, filepath.Join(appDir, "Frameworks", "RyukGram.dylib")); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd.SetArgs([]string{"check", "-i", appDir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected check to fail on a missing framework reference")
	}

	// Ship the framework -> clean.
	fwDir := filepath.Join(appDir, "Frameworks", "ffmpegkit.framework")
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fwBin := testutil.MakeTweak(t, tmp, "ffmpegkit")
	if err := os.Rename(fwBin, filepath.Join(fwDir, "ffmpegkit")); err != nil {
		t.Fatal(err)
	}
	cmd2 := NewRootCmd(func(_ context.Context, _ *app.Options) error { return nil })
	cmd2.SetArgs([]string{"check", "-i", appDir})
	if err := cmd2.Execute(); err != nil {
		t.Errorf("check should pass once the framework is shipped: %v", err)
	}
}

func optsEqual(a, b app.Options) bool {
	return reflect.DeepEqual(a, b)
}
