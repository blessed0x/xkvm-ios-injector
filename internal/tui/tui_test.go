package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/app"
	"github.com/xscope0/xkvm-ios-injector/internal/decrypt"
	"github.com/xscope0/xkvm-ios-injector/internal/fetch"
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

// withRun builds a UI capturing the Options passed to Run.
func withRun(t *testing.T, input string) (string, *app.Options) {
	t.Helper()
	u := NewForTest(strings.NewReader(input), &bytes.Buffer{})
	var got *app.Options
	u.Run = func(_ context.Context, opts *app.Options) error {
		got = opts
		return nil
	}
	u.Start()
	return u.Out.(*bytes.Buffer).String(), got
}

func TestTUIIntroAndCategories(t *testing.T) {
	out := runUI(t, "q\n", nil)
	for _, want := range []string{
		"what are we doing today? (pick a category)",
		"apps", "tweaks", "convert", "info",
		"bye! see you next time",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIHelpScreen(t *testing.T) {
	out := runUI(t, "4\n1\nq\n", nil)
	for _, want := range []string{
		"help", "every menu action has a command-line twin", "xkvm -i App.ipa -f MyTweak.dylib",
		"xkvm check -i App.ipa --fix", "fakesign signs the app",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help screen missing %q", want)
		}
	}
}

func TestTUIAboutScreen(t *testing.T) {
	out := runUI(t, "4\n2\nq\n", nil)
	for _, want := range []string{"about xkvm", "cyan / pyzule-rw", "Azule", "Derootifier"} {
		if !strings.Contains(out, want) {
			t.Errorf("about screen missing %q", want)
		}
	}
}

func TestTUIMenuDispatchInjects(t *testing.T) {
	// apps -> inject -> app + one dylib -> defaults the rest of the way:
	// no more files, default output, no .cyan, no customizations, expert
	// defaults (fakesign + overwrite on), compress 6, dylib not at root.
	out, got := withRun(t, "1\n1\napp.ipa\nTweak.dylib\n\n\n\n\n\n6\nn\nq\n")
	if got == nil {
		t.Fatal("Run was never called; output:\n" + out)
	}
	if got.Input != "app.ipa" || len(got.Files) != 1 || got.Files[0] != "Tweak.dylib" {
		t.Errorf("options not wired from prompts: %+v", got)
	}
	if !got.Fakesign {
		t.Errorf("fakesign should default on: %+v", got)
	}
	if !got.Overwrite {
		t.Errorf("overwrite should default on: %+v", got)
	}
	if got.Patch || got.ElleKit || got.Thin || got.IgnoreEncrypted {
		t.Errorf("expert flags should stay off when unanswered: %+v", got)
	}
	if len(got.RootDylibs) != 0 {
		t.Errorf("root-dylibs should stay empty when declined: %+v", got)
	}
	if got.Compress != 6 {
		t.Errorf("compress should default to 6, got %d", got.Compress)
	}
	if got.BundleID != "" || got.Name != "" || got.Icon != "" {
		t.Errorf("customizations should stay empty when unanswered: %+v", got)
	}
	if !strings.Contains(out, "your tweaked app is ready!") {
		t.Errorf("success line missing; output:\n%s", out)
	}
	if !strings.Contains(out, "expert options") || !strings.Contains(out, "fakesign every binary") {
		t.Errorf("expert options list should render; output:\n%s", out)
	}
}

func TestTUIInjectCustomizationAndExtras(t *testing.T) {
	// apps -> inject; a .cyan recipe; pick bundle-id(1)+display-name(2);
	// extras: patch on (3), fakesign OFF (!0); compress 9; root-dylib yes.
	out, got := withRun(t,
		"1\n1\napp.ipa\nTweak.dylib\n\n\n"+
			"rec.cyan\n\n"+
			"1 2\ncom.x.y\nMy App\n"+
			"3 !1\n"+
			"9\n"+
			"y\n"+
			"q\n")
	if got == nil {
		t.Fatal("Run was never called; output:\n" + out)
	}
	if len(got.Cyans) != 1 || got.Cyans[0] != "rec.cyan" {
		t.Errorf("cyans not wired: %+v", got.Cyans)
	}
	if got.BundleID != "com.x.y" || got.Name != "My App" {
		t.Errorf("customizations not wired: bundle=%q name=%q", got.BundleID, got.Name)
	}
	if !got.Patch {
		t.Errorf("patch (token 3) should be on: %+v", got)
	}
	if got.Fakesign {
		t.Errorf("fakesign should be OFF after !0: %+v", got)
	}
	if !got.Overwrite {
		t.Errorf("overwrite default should survive token override: %+v", got)
	}
	if got.Compress != 9 {
		t.Errorf("compress should be 9, got %d", got.Compress)
	}
	if len(got.RootDylibs) != 1 || got.RootDylibs[0] != "Tweak.dylib" {
		t.Errorf("root-dylibs not wired: %+v", got.RootDylibs)
	}
}

func TestTUIInjectCancelAtCompressNeverRuns(t *testing.T) {
	// q at the compression picker cancels the flow before Run.
	out, got := withRun(t, "1\n1\napp.ipa\nTweak.dylib\n\n\n\n\n\nq\nq\n")
	if got != nil {
		t.Errorf("Run must not be called when the user cancels; got %+v", got)
	}
	if !strings.Contains(out, "bye") {
		t.Errorf("should return to menu after cancel; output:\n%s", out)
	}
}

func TestTUIExtractFlowBothBranches(t *testing.T) {
	// tweaks -> extract -> from app
	out := runUI(t, "2\n1\n1\nextracted\napp.ipa\nq\n", nil)
	for _, want := range []string{
		"what do you want to pull tweaks from?",
		"from an app (.ipa / .tipa / .app)",
		"which app should I look inside?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("extract app branch missing %q; output:\n%s", want, out)
		}
	}
	// tweaks -> extract -> from deb
	out = runUI(t, "2\n1\n2\nextracted\ntweak.deb\nq\n", nil)
	for _, want := range []string{
		"from a .deb package",
		"which .deb should I open?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("extract deb branch missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIConvertRootlessPrompts(t *testing.T) {
	// convert -> convert -> rootful->rootless, then thin/tweakinject asks.
	out := runUI(t, "3\n1\n1\nin.deb\n\nn\nn\nq\n", nil)
	for _, want := range []string{
		"convert which way?",
		"rootful → rootless",
		"thin the binaries to arm64 only?",
		"modern TweakInject layout?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("convert flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUICheckFixFlowPromptsAndRoutes(t *testing.T) {
	// apps -> check with the fix pass: fix-dir + fetch + output prompts
	// must render, and the flow must route to CheckAndFix (its distinctive
	// "does not exist" error, rather than CheckBundle's report-only path).
	out := runUI(t, "1\n2\napp.ipa\ny\n/opt/tweaks\n\ny\n\nq\n", nil)
	for _, want := range []string{
		"want me to try to fix missing files automatically?",
		"which folder should I search for the missing files?",
		"add another search folder?",
		"also search the online repos",
		"where should the fixed copy go?",
		"all fixed — every tweak can now find what it needs!",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check-fix flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUICheckReportOnly(t *testing.T) {
	// apps -> check, fix declined: routes to the report-only path.
	out := runUI(t, "1\n2\napp.ipa\nn\nq\n", nil)
	if !strings.Contains(out, "all clear — every tweak can find what it needs!") {
		t.Errorf("report-only result missing; output:\n%s", out)
	}
}

func TestTUIDebifyFlowExtras(t *testing.T) {
	// tweaks -> build -> debify, with bundle ids + depends extras.
	out := runUI(t, "2\n3\n1\nTweak.dylib\n\n1 3\ncom.instagram.ios com.twitter.ios\nnope dep2\nq\n", nil)
	for _, want := range []string{
		"build or check what?",
		"wrap a dylib into a .deb",
		"package details",
		"target app bundle ids",
		"custom dependencies",
		"your .deb is ready!", // failure path prints the line with [fail]
	} {
		if !strings.Contains(out, want) {
			t.Errorf("debify flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUICgenAndCyanCheckPrompts(t *testing.T) {
	// tweaks -> build -> cgen: files loop, name + patch extras cancel early
	// to avoid writing a real recipe.
	out := runUI(t, "2\n3\n2\nTweak.dylib\n\n\nn\nq\n", nil)
	for _, want := range []string{
		"generate a .cyan recipe",
		"which tweak should the recipe include?",
		"recipe extras",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cgen flow missing %q; output:\n%s", want, out)
		}
	}
	// tweaks -> build -> cyan-check on a bogus path routes to the validator.
	out = runUI(t, "2\n3\n3\nnope.cyan\nq\n", nil)
	if !strings.Contains(out, "that .cyan checks out") {
		t.Errorf("cyan-check result line missing; output:\n%s", out)
	}
}

func TestTUIFetchVersionPickerPins(t *testing.T) {
	u := NewForTest(strings.NewReader("2\n2\ncom.x.tweak\n\n\nY\n\n2\nn\nq\n"), &bytes.Buffer{})
	var pinned map[string]string
	u.FetchVersions = func(_ context.Context, id string) ([]fetch.PkgVersion, error) {
		if id != "com.x.tweak" {
			t.Errorf("versions requested for %q", id)
		}
		return []fetch.PkgVersion{
			{Version: "2.0.0", Latest: true},
			{Version: "1.5.0", Latest: false},
		}, nil
	}
	u.FetchPinned = func(_ context.Context, ids, _ []string, _ bool, dir string, p map[string]string) ([]string, error) {
		pinned = p
		return []string{dir + "/com.x.tweak-1.5.0.deb"}, nil
	}
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	if pinned == nil || pinned["com.x.tweak"] != "1.5.0" {
		t.Errorf("picker must pin the chosen version; got %+v", pinned)
	}
	for _, want := range []string{
		"which version of com.x.tweak do you want?",
		"latest (recommended)",
		"version 1.5.0",
		"fetched 1 .deb(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fetch picker output missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIFetchNoPickerWhenUnavailable(t *testing.T) {
	// Without FetchVersions the flow skips the picker entirely and resolves
	// through the plain Fetch hook.
	u := NewForTest(strings.NewReader("2\n2\ncom.x.tweak\n\n\nY\n\nN\nq\n"), &bytes.Buffer{})
	var fetched []string
	u.Fetch = func(_ context.Context, ids, _ []string, _ bool, dir string) ([]string, error) {
		fetched = ids
		return []string{dir + "/t.deb"}, nil
	}
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	if len(fetched) != 1 || fetched[0] != "com.x.tweak" {
		t.Errorf("plain Fetch should resolve the id; got %+v", fetched)
	}
	if strings.Contains(out, "which version of") {
		t.Errorf("version picker should be skipped without FetchVersions; output:\n%s", out)
	}
}

func TestTUIFetchHandsOverToInject(t *testing.T) {
	u := NewForTest(strings.NewReader(
		"2\n2\ncom.x.tweak\n\n\nY\n\nY\napp.ipa\n\n\n\n\n6\nn\nq\n"), &bytes.Buffer{})
	var runOpts *app.Options
	u.Run = func(_ context.Context, opts *app.Options) error {
		runOpts = opts
		return nil
	}
	u.Fetch = func(_ context.Context, ids, _ []string, _ bool, dir string) ([]string, error) {
		return []string{dir + "/tweak.deb"}, nil
	}
	u.Start()
	if runOpts == nil {
		t.Fatal("fetch handover never reached inject")
	}
	if runOpts.Input != "app.ipa" || len(runOpts.Files) != 1 || !strings.HasSuffix(runOpts.Files[0], "tweak.deb") {
		t.Errorf("handover options wrong: %+v", runOpts)
	}
}

func TestTUICacheFlowStubs(t *testing.T) {
	u := NewForTest(strings.NewReader("2\n4\n2\nq\n"), &bytes.Buffer{})
	u.CacheUsage = func() (string, int, int64, error) { return "/tmp/xkvm-cache", 3, 9000, nil }
	var cleared int
	u.CacheClear = func() (int, error) { cleared = 5; return 5, nil }
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"cache folder: /tmp/xkvm-cache",
		"3 cached tweak(s)",
		"what should I do with the cache?",
		"clear the whole cache",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cache flow missing %q; output:\n%s", want, out)
		}
	}
	if cleared != 5 {
		t.Errorf("CacheClear stub should have run; got %d", cleared)
	}
}

func TestTUIDecryptFlowWithVersionAndFolder(t *testing.T) {
	u := NewForTest(strings.NewReader(
		"1\n3\nyou@example.com\nsecretpw\n310633997\n8612345678\n1\n/tmp/out\nq\n"), &bytes.Buffer{})
	var got app.DecryptOptions
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		got = o
		return "/tmp/out/App.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) { return decrypt.Prefs{AskMode: decrypt.AskModeAsk}, nil }
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return false }
	u.Start()
	if got.AppleID != "you@example.com" || got.Password != "secretpw" {
		t.Errorf("credentials not wired: %+v", got)
	}
	if got.AppID != "310633997" || got.Version != "8612345678" || got.OutputDir != "/tmp/out" {
		t.Errorf("decrypt options not wired: %+v", got)
	}
	out := u.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"where should the output .ipa go?",
		"pick a folder",
		"downloaded to /tmp/out/App.ipa",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("decrypt flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIDecryptSavedAuthChoices(t *testing.T) {
	// Signed in: keep (1) uses the saved session — no credentials asked.
	u := NewForTest(strings.NewReader("1\n3\n1\n310633997\n\nq\n"), &bytes.Buffer{})
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		if o.AppleID != "" {
			t.Errorf("keeping the saved session must not re-ask credentials: %+v", o)
		}
		return "/tmp/out/App.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) {
		return decrypt.Prefs{AskMode: decrypt.AskModeNever, OutputDir: "/tmp/out"}, nil
	}
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }
	u.SavedAppleID = func() (string, bool) { return "you@example.com", true }
	u.Logout = func() error { return nil }
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	for _, want := range []string{"signed in as you@example.com", "keep using this Apple ID", "signed in"} {
		if !strings.Contains(out, want) {
			t.Errorf("saved-auth flow missing %q; output:\n%s", want, out)
		}
	}
}

func TestTUIDecryptLogoutThenSignIn(t *testing.T) {
	u := NewForTest(strings.NewReader("1\n3\n3\nnew@example.com\nnewpw\n310633997\n\n1\n/tmp/x\nq\n"), &bytes.Buffer{})
	loggedOut := false
	u.Decrypt = func(_ context.Context, o app.DecryptOptions) (string, error) {
		if o.AppleID != "new@example.com" {
			t.Errorf("logout path must use the fresh credentials: %+v", o)
		}
		return "/tmp/x/App.ipa", nil
	}
	u.DecryptPrefs = func() (decrypt.Prefs, error) { return decrypt.Prefs{AskMode: decrypt.AskModeAsk}, nil }
	u.SaveDecryptPrefs = func(decrypt.Prefs) error { return nil }
	u.HasSavedAuth = func() bool { return true }
	u.SavedAppleID = func() (string, bool) { return "old@example.com", true }
	u.Logout = func() error { loggedOut = true; return nil }
	u.Start()
	if !loggedOut {
		t.Error("logout choice must call Logout before re-signing in")
	}
	out := u.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "signed out. the saved login is gone.") {
		t.Errorf("logout confirmation missing; output:\n%s", out)
	}
}

func TestTUIWrongMenuAnswerGuidesBack(t *testing.T) {
	out := runUI(t, "99\nq\n", nil)
	if !strings.Contains(out, "try a number from the list!") {
		t.Errorf("invalid pick should explain itself; output:\n%s", out)
	}
}

func TestChooseLineSingleProtocol(t *testing.T) {
	u := NewForTest(strings.NewReader("2\n"), &bytes.Buffer{})
	got := u.choose("pick a thing", []Choice{
		{"alpha", "first", "a", false},
		{"beta", "second", "b", false},
	}, false)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("number pick: %+v", got)
	}
	u = NewForTest(strings.NewReader("beta\n"), &bytes.Buffer{})
	got = u.choose("pick a thing", []Choice{
		{"alpha", "first", "a", false},
		{"beta", "second", "b", false},
	}, false)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("name pick: %+v", got)
	}
	u = NewForTest(strings.NewReader("q\n"), &bytes.Buffer{})
	if got := u.choose("pick a thing", []Choice{{"alpha", "d", "e", false}}, false); got != nil {
		t.Errorf("q cancels, got %+v", got)
	}
	// Unambiguous prefix works too.
	u = NewForTest(strings.NewReader("be\n"), &bytes.Buffer{})
	got = u.choose("pick a thing", []Choice{
		{"alpha", "first", "a", false},
		{"beta", "second", "b", false},
	}, false)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("prefix pick: %+v", got)
	}
}

func TestChooseLineMultiProtocol(t *testing.T) {
	choices := []Choice{
		{"one", "d", "", true},
		{"two", "d", "", false},
		{"three", "d", "", false},
	}
	// Blank returns the defaults.
	u := NewForTest(strings.NewReader("\n"), &bytes.Buffer{})
	if got := u.choose("multi", choices, true); len(got) != 1 || got[0] != 0 {
		t.Errorf("blank keeps defaults, got %+v", got)
	}
	// Tokens add; !N removes a default.
	u = NewForTest(strings.NewReader("2 !1\n"), &bytes.Buffer{})
	if got := u.choose("multi", choices, true); len(got) != 1 || got[0] != 1 {
		t.Errorf("tokens should toggle, got %+v", got)
	}
	// q cancels (nil, not defaults).
	u = NewForTest(strings.NewReader("q\n"), &bytes.Buffer{})
	if got := u.choose("multi", choices, true); got != nil {
		t.Errorf("q cancels multi, got %+v", got)
	}
}

func TestCompressChoicesStayInOrder(t *testing.T) {
	cs := compressChoices()
	if len(cs) != 10 {
		t.Fatalf("want 10 levels, got %d", len(cs))
	}
	if cs[0].Name != "level 0 — fastest, biggest file" || cs[9].Name != "level 9 — slowest, smallest file" {
		t.Errorf("level labels wrong: %+v / %+v", cs[0].Name, cs[9].Name)
	}
	if !cs[6].Def {
		t.Error("level 6 should be the default")
	}
}

func TestPanelPadNeverNegative(t *testing.T) {
	u := NewForTest(strings.NewReader(""), &bytes.Buffer{})
	// A desc wider than the panel must not panic.
	s := u.panelPad(Choice{Name: "x", Desc: strings.Repeat("word ", 99)}, 40, 4)
	if !strings.Contains(s, "why this one") {
		t.Errorf("panel should render, got %q", s[:80])
	}
}

var _ = errors.New
