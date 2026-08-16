package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/app"
	"github.com/xscope0/xkvm-ios-injector/internal/cyanfile"
	"github.com/xscope0/xkvm-ios-injector/internal/deb"
	"github.com/xscope0/xkvm-ios-injector/internal/decrypt"
)

// runUI pipes input into a fresh UI and returns everything it printed.
func runUI(t *testing.T, input string, run func(ctx context.Context, opts *app.Options) error) string {
	t.Helper()
	u := NewForTest(strings.NewReader(input), &bytes.Buffer{})
	if run != nil {
		u.Run = run
	}
	u.Start()
	return u.Out.(*bytes.Buffer).String()
}

func TestTUIHelpScreen(t *testing.T) {
	out := runUI(t, "6\nq\n", nil)
	for _, want := range []string{
		"help — in plain words",
		"xkvm is a toolbox for iOS apps",
		"xkvm -i App.ipa -f MyTweak.dylib -o App-Tweaked.ipa",
		"xkvm check -i App-Tweaked.ipa",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help screen missing %q", want)
		}
	}
}

func TestTUIAboutScreen(t *testing.T) {
	out := runUI(t, "7\nq\n", nil)
	for _, want := range []string{"about xkvm", "cyan / pyzule-rw", "Azule", "Derootifier"} {
		if !strings.Contains(out, want) {
			t.Errorf("about screen missing %q", want)
		}
	}
}

func TestTUIMenuDispatchInjects(t *testing.T) {
	var got *app.Options
	// identity, patch, ellekit, root-dylib all answered "n" (the file is a
	// .dylib so the root-dylib question is asked), then quit.
	out := runUI(t, "1\napp.ipa\nTweak.dylib\n\nn\nn\nn\nn\nq\n", func(_ context.Context, opts *app.Options) error {
		got = opts
		return nil
	})
	if got == nil {
		t.Fatal("Run was never called; output:\n" + out)
	}
	if got.Input != "app.ipa" || len(got.Files) != 1 || got.Files[0] != "Tweak.dylib" {
		t.Errorf("options not wired from prompts: %+v", got)
	}
	if !got.Fakesign {
		t.Errorf("fakesign should default on for sideloading, got %+v", got)
	}
	if got.Patch {
		t.Errorf("patch should stay off when the user declines, got %+v", got)
	}
	if got.ElleKit {
		t.Errorf("ellekit should stay off when the user declines, got %+v", got)
	}
	if len(got.RootDylibs) != 0 {
		t.Errorf("root-dylibs should stay empty when the user declines, got %+v", got)
	}
	if got.BundleID != "" || got.Name != "" || got.Icon != "" {
		t.Errorf("identity edits should stay empty when the user declines, got %+v", got)
	}
	if !strings.Contains(out, "your tweaked app is ready!") {
		t.Errorf("success line missing; output:\n%s", out)
	}
}

func TestTUIMenuDispatchExtractAndCheck(t *testing.T) {
	var calls []string
	run := func(_ context.Context, opts *app.Options) error {
		calls = append(calls, "run")
		return nil
	}
	out := runUI(t, "2\n1\napp.ipa\n\n5\napp.ipa\nq\n", run)
	// extract (option 1 = from an app) + check use the real app package funcs,
	// not u.Run — so Run is never called; the calls are captured via the app
	// package instead. Here we only assert the flows render their prompts
	// without crashing.
	for _, want := range []string{"which app should I look inside?", "which app should I check?"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q; output:\n%s", want, out)
		}
	}
	_ = calls
}

func TestTUICheckFixFlowPromptsAndRoutes(t *testing.T) {
	// Menu 5 with the fix pass accepted: fix-dir + fetch + output prompts
	// must render, and the flow must route to CheckAndFix (its distinctive
	// "does not exist" error, rather than CheckBundle's report-only path).
	// The input file doesn't exist, so CheckAndFix fails fast before any
	// search touches the cache or the network.
	out := runUI(t, "5\napp.ipa\ny\n/opt/tweaks\n\ny\n\nq\n", nil)
	for _, want := range []string{
		"want me to try to fix missing files automatically?",
		"which folder should I search for the missing files?",
		"add another search folder?",
		"also search the online repos",
		"where should the fixed copy go?",
		"all fixed — every tweak can now find what it needs!",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fix flow missing %q; output:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "app.ipa does not exist") {
		t.Errorf("fix flow should have failed fast on the missing input (CheckAndFix routing); output:\n%s", out)
	}
}

func TestTUICheckDeclinesFixStaysPlainCheck(t *testing.T) {
	// Answering no to the fix offer keeps the plain check: no fix-dir / fetch
	// / output prompts, and CheckBundle's report-only phrasing.
	out := runUI(t, "5\napp.ipa\nn\nq\n", nil)
	for _, absent := range []string{
		"which folder should I search for the missing files?",
		"also search the online repos",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("declined fix flow still asked %q; output:\n%s", absent, out)
		}
	}
	for _, want := range []string{"which app should I check?", "that didn't work"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain check missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIErrorShown(t *testing.T) {
	out := runUI(t, "1\napp.ipa\nTweak.dylib\n\nn\nn\nn\nn\nq\n", func(_ context.Context, _ *app.Options) error {
		return errors.New("boom: no such file")
	})
	for _, want := range []string{"that didn't work", "boom: no such file"} {
		if !strings.Contains(out, want) {
			t.Errorf("error path missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIEOFEndsGracefully(t *testing.T) {
	// Piping 'q' from the main menu exits with the bye message.
	out := runUI(t, "q\n", nil)
	if !strings.Contains(out, "bye!") {
		t.Errorf("missing farewell; output:\n%s", out)
	}
}

func TestTUIEOFExitsWithoutHanging(t *testing.T) {
	// No input at all (EOF on the very first read, e.g. 'echo | xkvm' or a
	// script with closed stdin): the menu shows, the loop sees EOF, and the
	// TUI must exit cleanly instead of re-prompting against a closed stream
	// forever. This is what keeps a bare `xkvm` in a non-interactive context
	// from hanging.
	out := runUI(t, "", nil)
	if !strings.Contains(out, "bye!") {
		t.Errorf("missing farewell on immediate EOF; output:\n%s", out)
	}
}

func TestTUIEOFInFlowReturnsToMenuThenExits(t *testing.T) {
	// EOF lands mid-flow (menu 3, then no input): the flow's empty-answer
	// path sends the user back to the menu, and the loop's EOF check then
	// exits. Pins that a half-typed scripted run terminates instead of
	// spinning.
	out := runUI(t, "3\n", nil)
	if !strings.Contains(out, "bye!") {
		t.Errorf("missing farewell after mid-flow EOF; output:\n%s", out)
	}
}

func TestTUIBadChoiceThenQuit(t *testing.T) {
	out := runUI(t, "99\nq\n", nil)
	if !strings.Contains(out, "try a number from the list") {
		t.Errorf("bad-choice message missing; output:\n%s", out)
	}
	if !strings.Contains(out, "bye!") {
		t.Errorf("missing farewell; output:\n%s", out)
	}
}

// goldenShadowDeb is the committed real-world fixture: Shadow_3.0-0.rc3.deb
// (jjolano's jailbreak-detection bypass, fat armv7+arm64+arm64e). The TUI
// convert flow runs the real app.Rootless against it — no mocks — so this
// test proves the menu actually drives a genuine deb-to-rootless conversion
// and lands the result where the user said. Pure Go, so it runs on every CI
// leg (same fixture the internal/rootless golden tests use).
const goldenShadowDeb = "../../testdata/fixtures/debs/Shadow_3.0-0.rc3.deb"

func TestTUIConvertFlowRootlessRealDeb(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("golden fixture missing at %s: %v", fixture, err)
	}
	out := filepath.Join(tmp, "shadow-rootless.deb")

	// Menu 3 (convert) -> option 1 (rootful .deb to rootless) -> the real
	// fixture -> the output path -> the two new option prompts left at their
	// defaults (thin: no, tweakinject: no) -> quit.
	input := "3\n1\n" + fixture + "\n" + out + "\n\n\nq\n"
	uiOut := runUI(t, input, nil)

	// The flow must report success and actually produce the .deb.
	if !strings.Contains(uiOut, "rootless .deb ready") {
		t.Errorf("success line missing; output:\n%s", uiOut)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("converted .deb not written to the prompted path %s: %v\noutput:\n%s", out, err, uiOut)
	}

	// The result is a real rootless package: payload under var/jb, control
	// carrying iphoneos-arm64 (the rootless arch), dylib still a valid file.
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatalf("Unpack converted deb: %v", err)
	}
	for _, want := range []string{
		filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "Shadow.dylib"),
		filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries", "Shadow.plist"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("rootless payload missing %s: %v", filepath.Base(want), err)
		}
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Architecture: iphoneos-arm64", "cy+cpu.arm64v8"} {
		if !strings.Contains(string(ctl), want) {
			t.Errorf("converted control missing %q:\n%s", want, ctl)
		}
	}
}

// TestTUIConvertFlowRootlessTweakInjectRealDeb drives the new tweakinject
// option prompt (menu 3 -> option 1, answering yes) against the real Shadow
// fixture: the converted package must land under the modern TweakInject
// layout (var/jb/usr/lib/TweakInject) instead of the legacy DynamicLibraries
// path — proving the prompt actually reaches app.Rootless.
func TestTUIConvertFlowRootlessTweakInjectRealDeb(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("golden fixture missing at %s: %v", fixture, err)
	}
	out := filepath.Join(tmp, "shadow-ti.deb")

	// thin: no (empty), tweakinject: yes.
	uiOut := runUI(t, "3\n1\n"+fixture+"\n"+out+"\n\ny\nq\n", nil)
	if !strings.Contains(uiOut, "rootless .deb ready") {
		t.Errorf("success line missing; output:\n%s", uiOut)
	}
	unpacked := filepath.Join(tmp, "unpacked")
	if err := deb.Unpack(out, unpacked); err != nil {
		t.Fatalf("Unpack converted deb: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "usr", "lib", "TweakInject", "Shadow.dylib")); err != nil {
		t.Errorf("TweakInject layout payload missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpacked, "var", "jb", "Library", "MobileSubstrate", "DynamicLibraries")); !os.IsNotExist(err) {
		t.Errorf("legacy DynamicLibraries layout should be gone in TweakInject mode (stat err=%v)", err)
	}
}

// TestTUIConvertFlowRoothideRealDeb drives the roothide convert flow with its
// two new option prompts (pkgmirror left off, mode left at auto) against a
// real rootless deb prepared with the real app.Rootless: the menu must report
// success and land a real .deb.
func TestTUIConvertFlowRoothideRealDeb(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	rootlessDeb := filepath.Join(tmp, "shadow-rootless.deb")
	if err := app.Rootless(fixture, rootlessDeb, false, false); err != nil {
		t.Fatalf("preparing rootless input: %v", err)
	}
	out := filepath.Join(tmp, "shadow-roothide.deb")

	// pkgmirror: no (empty), mode: auto (empty).
	uiOut := runUI(t, "3\n2\n"+rootlessDeb+"\n"+out+"\n\n\nq\n", nil)
	if !strings.Contains(uiOut, "roothide .deb ready") {
		t.Errorf("success line missing; output:\n%s", uiOut)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("roothide .deb not written to the prompted path %s: %v\noutput:\n%s", out, err, uiOut)
	}
}

// TestTUIConvertFlowXinaAndRootfulRealDeb drives the two new convert options
// against the real Shadow fixture: menu 3 -> option 3 (Xina-style rootless)
// converts the rootful deb directly; menu 3 -> option 4 (back to rootful)
// converts a rootless deb prepared with the real app.Rootless. Both flows
// must report success and land a real package at the prompted path — pure
// Go, so they run on every CI leg.
func TestTUIConvertFlowXinaAndRootfulRealDeb(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("golden fixture missing at %s: %v", fixture, err)
	}

	// --- Option 3: rootful -> Xina-style rootless ---
	xinaOut := filepath.Join(tmp, "shadow-xina.deb")
	uiOut := runUI(t, "3\n3\n"+fixture+"\n"+xinaOut+"\nq\n", nil)
	if !strings.Contains(uiOut, "Xina-style rootless .deb ready") {
		t.Errorf("xina success line missing; output:\n%s", uiOut)
	}
	if _, err := os.Stat(xinaOut); err != nil {
		t.Fatalf("xina .deb not written to the prompted path %s: %v\noutput:\n%s", xinaOut, err, uiOut)
	}
	unpacked := filepath.Join(tmp, "xina-unpacked")
	if err := deb.Unpack(xinaOut, unpacked); err != nil {
		t.Fatal(err)
	}
	ctl, err := os.ReadFile(filepath.Join(unpacked, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctl), "(Xinafied-rootless)") {
		t.Errorf("xina control missing the Name suffix; got:\n%s", ctl)
	}

	// --- Option 4: rootless -> rootful, on a real rootless deb we prepare
	// with the real app.Rootless (the menu drives app.Rootful against it).
	rootlessDeb := filepath.Join(tmp, "shadow-rootless.deb")
	if err := app.Rootless(fixture, rootlessDeb, false, false); err != nil {
		t.Fatalf("preparing rootless input: %v", err)
	}
	rootfulOut := filepath.Join(tmp, "shadow-rootful.deb")
	uiOut = runUI(t, "3\n4\n"+rootlessDeb+"\n"+rootfulOut+"\nq\n", nil)
	if !strings.Contains(uiOut, "rootful .deb ready") {
		t.Errorf("rootful success line missing; output:\n%s", uiOut)
	}
	if _, err := os.Stat(rootfulOut); err != nil {
		t.Fatalf("rootful .deb not written to the prompted path %s: %v\noutput:\n%s", rootfulOut, err, uiOut)
	}
	unpacked2 := filepath.Join(tmp, "rootful-unpacked")
	if err := deb.Unpack(rootfulOut, unpacked2); err != nil {
		t.Fatal(err)
	}
	ctl2, err := os.ReadFile(filepath.Join(unpacked2, "DEBIAN", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ctl2), "Architecture: iphoneos-arm") {
		t.Errorf("rootful control missing iphoneos-arm; got:\n%s", ctl2)
	}
	if strings.Contains(string(ctl2), "cy+cpu.arm64v8") {
		t.Errorf("rootful control still carries the rootless runtime dep; got:\n%s", ctl2)
	}
	if _, err := os.Stat(filepath.Join(unpacked2, "Library", "MobileSubstrate", "DynamicLibraries", "Shadow.dylib")); err != nil {
		t.Errorf("rootful payload missing at package root: %v", err)
	}
}

// TestTUIEmptyPathTwiceReturnsToMenu pins the empty-input escalation: the
// first empty answer to a required path gets a nudge, the second one in a row
// sends the user back to the main menu (where q then quits cleanly).
func TestTUIEmptyPathTwiceReturnsToMenu(t *testing.T) {
	out := runUI(t, "1\n\n\nq\n", nil)
	for _, want := range []string{
		"that needs a path", // first empty → nudge
		"back to the menu",  // second empty → cancel out of the flow
		"bye!",              // q from the main menu → clean quit
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-input escalation missing %q; output:\n%s", want, out)
		}
	}
}

// TestTUIQuitWordCancelsFlow pins that typing q (or back/cancel) at any
// path question bails out of the flow to the main menu, so users who don't
// know the menu loop can always escape.
func TestTUIQuitWordCancelsFlow(t *testing.T) {
	out := runUI(t, "1\nq\nq\n", nil)
	for _, want := range []string{"ok, back to the menu.", "bye!"} {
		if !strings.Contains(out, want) {
			t.Errorf("cancel-in-flow missing %q; output:\n%s", want, out)
		}
	}
}

// TestTUIUndebFlowRealDeb drives menu 2 option 2 (extract from a .deb) against
// the real Shadow fixture deb and asserts the extracted artifacts land in the
// prompted folder, exactly like the CLI's 'xkvm undeb'.
func TestTUIUndebFlowRealDeb(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmp, "out")
	uiOut := runUI(t, "2\n2\n"+fixture+"\n"+outDir+"\nq\n", nil)
	if !strings.Contains(uiOut, "tweaks extracted to") {
		t.Errorf("success line missing; output:\n%s", uiOut)
	}
	for _, want := range []string{
		filepath.Join(outDir, "Shadow.dylib"),
		filepath.Join(outDir, "ShadowSettings.bundle"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("undeb flow missing %s: %v\noutput:\n%s", filepath.Base(want), err, uiOut)
		}
	}
}

// TestTUICyanCheckFlowRealGenerated drives menu 4 option 3 (check a .cyan
// recipe) against a real recipe generated from the Shadow fixture, and asserts
// the flow reports the recipe is safe to apply.
func TestTUICyanCheckFlowRealGenerated(t *testing.T) {
	tmp := t.TempDir()
	fixture, err := filepath.Abs(goldenShadowDeb)
	if err != nil {
		t.Fatal(err)
	}
	exDir := filepath.Join(tmp, "ex")
	if err := app.Undeb(fixture, exDir); err != nil {
		t.Fatalf("preparing fixture dylib: %v", err)
	}
	cyan := filepath.Join(tmp, "recipe.cyan")
	if err := cyanfile.Generate(cyanfile.GenerateOptions{
		Output: cyan,
		Files:  []string{filepath.Join(exDir, "Shadow.dylib")},
	}); err != nil {
		t.Fatalf("generating recipe: %v", err)
	}
	uiOut := runUI(t, "4\n3\n"+cyan+"\nq\n", nil)
	if !strings.Contains(uiOut, "that .cyan checks out") {
		t.Errorf("cyan-check success line missing; output:\n%s", uiOut)
	}
	if strings.Contains(uiOut, "[fail]") {
		t.Errorf("cyan-check reported failure on a clean recipe; output:\n%s", uiOut)
	}
}

// TestTUIFetchFlow drives menu 8 with a stubbed resolver and Run: the bundle
// id, repo source, and dependency choice reach the resolver, and the fetched
// file lands in the injection set.
// id is collected, the stub resolves it to a .deb, the flow reports the
// download, and the follow-up "inject now?" chains into the shared inject
// runner with the fetched file. Hermetic — no network.
func TestTUIFetchFlow(t *testing.T) {
	// Bundle id, then the new prompts: a repo source (wired to the resolver
	// as --apt-source), dependencies answered no (wired as --no-recurse), the
	// download folder, then straight into the shared inject flow.
	u := NewForTest(strings.NewReader(
		"8\ncom.example.tweak\nhttps://repo.chariz.com\nn\n/tmp/fetched\ny\napp.ipa\n/tmp/out.ipa\nn\nn\ny\nq\n"), &bytes.Buffer{})
	var gotIDs, gotFiles []string
	var gotSources []string
	gotNoRecurse := false
	u.Fetch = func(_ context.Context, ids, sources []string, noRecurse bool, _ string) ([]string, error) {
		gotIDs = ids
		gotSources = sources
		gotNoRecurse = noRecurse
		return []string{"/tmp/fetched/com.example.tweak.deb"}, nil
	}
	u.Run = func(_ context.Context, opts *app.Options) error {
		gotFiles = append(gotFiles, opts.Files...)
		return nil
	}
	u.Start()
	out := u.Out.(*bytes.Buffer).String()

	if len(gotIDs) != 1 || gotIDs[0] != "com.example.tweak" {
		t.Errorf("bundle id not wired to the resolver: %v", gotIDs)
	}
	if len(gotSources) != 1 || gotSources[0] != "https://repo.chariz.com" {
		t.Errorf("repo source prompt not wired to the resolver: %v", gotSources)
	}
	if !gotNoRecurse {
		t.Error("dependencies=no must reach the resolver as noRecurse")
	}
	if len(gotFiles) != 1 || gotFiles[0] != "/tmp/fetched/com.example.tweak.deb" {
		t.Errorf("fetched file not wired into the injection set: %v", gotFiles)
	}
	for _, want := range []string{"fetched 1 .deb(s)", "your tweaked app is ready!"} {
		if !strings.Contains(out, want) {
			t.Errorf("fetch flow missing %q; output:\n%s", want, out)
		}
	}
}

// TestTUICacheFlowEmpty drives menu 9 with an empty cache: it reports the
// folder and the emptiness, and never asks to clear anything.
func TestTUICacheFlowEmpty(t *testing.T) {
	u := NewForTest(strings.NewReader("9\nq\n"), &bytes.Buffer{})
	u.CacheUsage = func() (string, int, int64, error) {
		return "/tmp/fake-cache", 0, 0, nil
	}
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	for _, want := range []string{"cache folder: /tmp/fake-cache", "it's empty right now"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-cache flow missing %q; output:\n%s", want, out)
		}
	}
}

// TestTUICacheFlowClear drives menu 9 with a populated cache and answers
// "clear the whole cache": the injected CacheClear is called and the flow
// reports the removal count.
func TestTUICacheFlowClear(t *testing.T) {
	u := NewForTest(strings.NewReader("9\n2\nq\n"), &bytes.Buffer{})
	u.CacheUsage = func() (string, int, int64, error) {
		return "/tmp/fake-cache", 3, 4_500_000, nil
	}
	cleared := 0
	u.CacheClear = func() (int, error) {
		cleared = 3
		return 3, nil
	}
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	if cleared != 3 {
		t.Errorf("CacheClear not invoked from the menu (cleared=%d)", cleared)
	}
	for _, want := range []string{"3 cached tweak(s), 4.3 MB", "cache cleared", "removed 3 cached .deb(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("cache-clear flow missing %q; output:\n%s", want, out)
		}
	}
}

// TestTUIInjectEditsIdentity drives menu 1 answering yes to the identity
// question and filling in a bundle id, display name, and icon, and asserts
// all three land on the options handed to Run.
func TestTUIInjectEditsIdentity(t *testing.T) {
	var got *app.Options
	out := runUI(t, "1\napp.ipa\nTweak.dylib\n/tmp/out.ipa\ny\ncom.new.id\nMy App\n/tmp/newicon.png\nn\nn\nn\nq\n", func(_ context.Context, opts *app.Options) error {
		got = opts
		return nil
	})
	if got == nil {
		t.Fatal("Run was never called; output:\n" + out)
	}
	if got.BundleID != "com.new.id" {
		t.Errorf("bundle id not wired from prompt, got %q", got.BundleID)
	}
	if got.Name != "My App" {
		t.Errorf("display name not wired from prompt, got %q", got.Name)
	}
	if got.Icon != "/tmp/newicon.png" {
		t.Errorf("icon not wired from prompt, got %q", got.Icon)
	}
	if !strings.Contains(out, "your tweaked app is ready!") {
		t.Errorf("success line missing; output:\n%s", out)
	}
}

func TestTUIDecryptFlowPromptsAndRoutes(t *testing.T) {
	// Menu 10, no saved session: login prompts, app id, output-path
	// question (pick 1 = new folder), then the download runs.
	u := NewForTest(strings.NewReader(
		"10\nme@example.com\nhunter2\n310633997\n1\n/tmp/out\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	var saved decrypt.Prefs
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/tmp/out/com.example_1.0.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) { return decrypt.Prefs{AskMode: decrypt.AskModeAsk}, nil }
	u.SaveDecryptPrefs = func(p decrypt.Prefs) error { saved = p; return nil }
	u.HasSavedAuth = func() bool { return false }

	u.Start()
	out := u.Out.(*bytes.Buffer).String()

	if got.AppleID != "me@example.com" || got.Password != "hunter2" {
		t.Errorf("credentials not wired: %+v", got)
	}
	if got.AppID != "310633997" {
		t.Errorf("app id = %q, want 310633997", got.AppID)
	}
	if got.OutputDir != "/tmp/out" {
		t.Errorf("output dir = %q, want /tmp/out", got.OutputDir)
	}
	if saved.OutputDir != "/tmp/out" || saved.AskMode != decrypt.AskModeAsk {
		t.Errorf("saved prefs = %+v, want /tmp/out + ask", saved)
	}
	for _, want := range []string{"your Apple ID", "password", "what app?", "pick 1, 2, or 3", "downloaded to /tmp/out/com.example_1.0.ipa"} {
		if !strings.Contains(out, want) {
			t.Errorf("decrypt flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIDecryptNeverAskAgain(t *testing.T) {
	// Saved session (kept) + never-ask mode: the output question is skipped.
	u := NewForTest(strings.NewReader(
		"10\n1\n310633997\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/saved/com.example_1.0.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) {
		return decrypt.Prefs{OutputDir: "/saved", AskMode: decrypt.AskModeNever}, nil
	}
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }

	u.Start()
	out := u.Out.(*bytes.Buffer).String()

	if got.OutputDir != "/saved" {
		t.Errorf("output dir = %q, want the saved /saved", got.OutputDir)
	}
	if got.AppleID != "" {
		t.Errorf("saved session should skip credentials, got AppleID %q", got.AppleID)
	}
	if strings.Contains(out, "where should the output") {
		t.Error("never-ask mode still asked where the output goes")
	}
}

func TestTUIDecryptReuseLastDir(t *testing.T) {
	// Saved session kept, then choice 2 reuses the saved directory.
	u := NewForTest(strings.NewReader(
		"10\n1\n310633997\n2\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/last/com.example_1.0.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) {
		return decrypt.Prefs{OutputDir: "/last", AskMode: decrypt.AskModeAsk}, nil
	}
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }

	u.Start()

	if got.OutputDir != "/last" {
		t.Errorf("output dir = %q, want the reused /last", got.OutputDir)
	}
}

func TestTUIDecryptChangeAccount(t *testing.T) {
	// Saved session exists but the user picks "change account": credentials
	// are asked for and the new ones reach the download.
	u := NewForTest(strings.NewReader(
		"10\n2\nnew@example.com\nnewpw\n310633997\n1\n/tmp/out\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/tmp/out/com.example_1.0.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) { return decrypt.Prefs{AskMode: decrypt.AskModeAsk}, nil }
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }
	u.SavedAppleID = func() (string, bool) { return "old@example.com", true }

	u.Start()

	if got.AppleID != "new@example.com" || got.Password != "newpw" {
		t.Errorf("change-account creds not wired: %+v", got)
	}
}

func TestTUIDecryptLogout(t *testing.T) {
	// "Log out of xkvm" forgets the session, then asks for fresh credentials
	// so the download can still go ahead.
	loggedOut := false
	u := NewForTest(strings.NewReader(
		"10\n3\nnew@example.com\nnewpw\n310633997\n1\n/tmp/out\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/tmp/out/com.example_1.0.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) { return decrypt.Prefs{AskMode: decrypt.AskModeAsk}, nil }
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }
	u.SavedAppleID = func() (string, bool) { return "old@example.com", true }
	u.Logout = func() error { loggedOut = true; return nil }

	u.Start()

	if !loggedOut {
		t.Error("logout option did not call Logout")
	}
	if got.AppleID != "new@example.com" {
		t.Errorf("post-logout login not wired: AppleID = %q", got.AppleID)
	}
}
