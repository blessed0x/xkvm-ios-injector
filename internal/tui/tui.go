// Package tui is the friendly, menu-driven way to use xkvm. It drives the
// same internal/app functions the CLI uses — no separate code path — but
// every feature, and every expert flag behind it, is reachable through
// categorized arrow-key menus. Each option carries a short explanation plus
// an example that shows while it is highlighted, so new users learn what
// they are enabling as they go. The CLI flags stay the expert / AI /
// batch path; this is the human path.
//
// Zero dependencies: ANSI colors, box characters, a block spinner, and a
// raw-mode arrow picker (ioctl via build-tagged files). When stdin is not a
// terminal (pipes, tests) everything degrades to a line protocol: answers
// piped line by line, colors off, no animation.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/app"
	"github.com/xscope0/xkvm-ios-injector/internal/cyanfile"
	"github.com/xscope0/xkvm-ios-injector/internal/decrypt"
	"github.com/xscope0/xkvm-ios-injector/internal/fetch"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
	"github.com/xscope0/xkvm-ios-injector/internal/patch"
)

// UI holds the interactive session state.
type UI struct {
	In      *bufio.Reader
	Out     io.Writer
	Err     io.Writer
	Color   bool // ANSI colors on
	Animate bool // animated banner + spinner + arrow pickers on
	InTerm  bool // stdin is a real terminal (arrow keys usable)
	// eof is set once stdin runs out (piped input, Ctrl-D). The menu loop
	// checks it so scripted/closed input exits instead of re-prompting
	// forever; flow helpers already terminate on empty answers.
	eof bool
	// Run is injectable (tests capture instead of touching real apps).
	Run func(ctx context.Context, opts *app.Options) error
	// Fetch resolves tweak bundle ids to local .deb paths. Injectable so tests
	// don't hit the network; the real path is fetch.ResolveVersion.
	Fetch func(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string) ([]string, error)
	// FetchPinned is Fetch with per-id Canister version pins (what the
	// version picker drives). Defaults to fetch.ResolveVersion.
	FetchPinned func(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, pinned map[string]string) ([]string, error)
	// FetchVersions lists the available versions of one Canister package
	// (the arrow picker's data source). Defaults to fetch.Versions.
	FetchVersions func(ctx context.Context, id string) ([]fetch.PkgVersion, error)
	// CacheUsage reports the fetch cache (directory, deb count, total bytes),
	// CachePrune removes entries older than the 7-day TTL, and CacheClear
	// removes every entry. Injectable so tests stay off the real user cache
	// dir; the defaults are fetch.CacheUsage / PruneExpired / ClearCache.
	CacheUsage func() (dir string, debs int, bytes int64, err error)
	CachePrune func() (removed int, err error)
	CacheClear func() (removed int, err error)
	// Decrypt downloads an App Store app by Apple ID. Injectable so tests
	// stub the network; the real implementation is app.RunDecrypt.
	Decrypt func(ctx context.Context, o app.DecryptOptions) (string, error)
	// DecryptPrefs / SaveDecryptPrefs persist the output-directory choice
	// (ask / reuse / never ask again). Injectable so tests don't touch the
	// real user config dir; the defaults are decrypt.LoadPrefs/SavePrefs.
	DecryptPrefs     func() (decrypt.Prefs, error)
	SaveDecryptPrefs func(decrypt.Prefs) error
	// HasSavedAuth reports whether a signed-in Apple ID session exists,
	// SavedAppleID names it, and Logout forgets it. Injectable so tests stay
	// off the real user config dir; defaults are decrypt.HasSavedAuth /
	// decrypt.SavedAppleID / decrypt.Logout.
	HasSavedAuth func() bool
	SavedAppleID func() (string, bool)
	Logout       func() error
}

// New returns a UI for the current terminal. Color, animation and arrow
// pickers are enabled only when both stdin and stdout are terminals AND the
// NO_COLOR convention isn't set.
func New() *UI {
	stdoutTerm := isTerminal(os.Stdout)
	stdinTerm := isTerminal(os.Stdin)
	return &UI{
		In:      bufio.NewReader(os.Stdin),
		Out:     os.Stdout,
		Err:     os.Stderr,
		Color:   stdoutTerm && os.Getenv("NO_COLOR") == "",
		Animate: stdoutTerm,
		InTerm:  stdinTerm,
		Run:     app.Run,
		Fetch:   nil, // the real path lives in fetchTweaks
		FetchPinned: func(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, pinned map[string]string) ([]string, error) {
			return fetch.ResolveVersion(ctx, ids, sources, noRecurse, cacheDir, nil, pinned)
		},
		FetchVersions: func(ctx context.Context, id string) ([]fetch.PkgVersion, error) {
			return fetch.Versions(ctx, id, nil)
		},
		CacheUsage:       fetch.CacheUsage,
		CachePrune:       fetch.PruneExpired,
		CacheClear:       fetch.ClearCache,
		Decrypt:          app.RunDecrypt,
		DecryptPrefs:     decrypt.LoadPrefs,
		SaveDecryptPrefs: decrypt.SavePrefs,
		HasSavedAuth:     decrypt.HasSavedAuth,
		SavedAppleID:     decrypt.SavedAppleID,
		Logout:           decrypt.Logout,
	}
}

// NewForTest builds a non-animated, line-protocol UI reading from r and
// writing to w.
func NewForTest(r io.Reader, w io.Writer) *UI {
	return &UI{
		In:               bufio.NewReader(r),
		Out:              w,
		Err:              w,
		Color:            false,
		Animate:          false,
		InTerm:           false,
		Run:              app.Run,
		CacheUsage:       fetch.CacheUsage,
		CachePrune:       fetch.PruneExpired,
		CacheClear:       fetch.ClearCache,
		Decrypt:          app.RunDecrypt,
		DecryptPrefs:     decrypt.LoadPrefs,
		SaveDecryptPrefs: decrypt.SavePrefs,
		HasSavedAuth:     decrypt.HasSavedAuth,
		SavedAppleID:     decrypt.SavedAppleID,
		Logout:           decrypt.Logout,
	}
}

// Start shows the intro, then runs the categorized menu loop until the user
// picks Quit (or input ends).
func (u *UI) Start() {
	u.intro()
	for {
		if !u.pickTool() {
			u.say(anGreen, "bye! see you next time")
			return
		}
		u.pressEnterToContinue()
	}
}

// category groups the features of the menu.
type category struct {
	name string
	desc string
	ex   string
	list []feature
}

// feature is one runnable menu entry with docs shown while highlighted.
type feature struct {
	name string
	desc string
	ex   string
	run  func()
}

// categories builds the menu tree.
func (u *UI) categories() []category {
	return []category{
		{
			name: "apps", desc: "work on an app: put tweaks in, make sure it won't crash, or grab it from the App Store",
			ex: "the main workflow starts here",
			list: []feature{
				{"inject", "put one or more tweaks (dylib / .deb / framework / bundle / .cyan) into an app and re-sign everything into a ready-to-sideload file", "Tweak.dylib + App.ipa → App-Tweaked.ipa", u.flowInject},
				{"check", "make sure every tweak inside an app can find the files it needs before you install — can search folders/repos and write a fixed copy", "check App-Tweaked.ipa, fix what's missing", u.flowCheck},
				{"decrypt", "download an app you own from the App Store by Apple ID so you can tweak it (sign in once; session remembered)", "apps.apple.com link or 310633997 → .ipa", u.flowDecrypt},
			},
		},
		{
			name: "tweaks", desc: "work with tweak files: reuse them, download them by bundle id, package them for sharing",
			ex: "everything about the tweaks themselves",
			list: []feature{
				{"extract", "pull tweaks back out of an app or a .deb — the placement manifest remembers where each one lived for automatic re-injection", "App.ipa → extracted/ (re-inject later with inject)", u.flowExtract},
				{"fetch", "find and download a tweak by its bundle id from the Canister / MobileAPT repos, dependencies included — pick the version interactively", "com.example.tweak → tweak.deb + deps", u.flowFetch},
				{"build", "wrap a dylib into a .deb package, or create / validate a shareable .cyan recipe", "Tweak.dylib → Tweak.deb · recipe.cyan", u.flowBuild},
				{"cache", "see and tidy the local download cache (reused so repeat fetches skip the network, auto-cleaned after 7 days)", "clear old entries or everything", u.flowCache},
			},
		},
		{
			name: "convert", desc: "change a tweak package format so it fits your jailbreak's layout",
			ex: "rootful ↔ rootless ↔ roothide, plus Xina-style",
			list: []feature{
				{"convert", "convert one .deb between rootful / rootless / roothide / Xina layouts — byte-faithful ports of the ecosystem tools", "tweak.deb → tweak-rootless.deb (Dopamine/ellekit)", u.flowConvert},
			},
		},
		{
			name: "info", desc: "help, the command-line equivalents, and what xkvm is",
			ex: "no app files touched here",
			list: []feature{
				{"help", "a plain-language guide plus the same commands an expert would run — every menu action has a CLI twin", "xkvm -i App.ipa -f Tweak.dylib", u.help},
				{"about", "what xkvm is, which tools it merges, and why it's a single Go binary", "cyan / Azule / Derootifier heritage", u.about},
			},
		},
	}
}

// pickTool runs the two-level category → feature picker and executes the
// chosen flow. Returns false when the user quit (or input ended) at the
// category level; true otherwise (flows run, feature-level back stays in
// the loop).
func (u *UI) pickTool() bool {
	cats := u.categories()
	var catChoices []Choice
	for _, c := range cats {
		catChoices = append(catChoices, Choice{Name: c.name, Desc: c.desc, Ex: c.ex})
	}
	ci := u.chooseLevel(catChoices, "what are we doing today? (pick a category)")
	if ci < 0 {
		return false // q / blank at the top level = quit
	}
	cat := cats[ci]
	var feats []Choice
	for _, f := range cat.list {
		feats = append(feats, Choice{Name: f.name, Desc: f.desc, Ex: f.ex})
	}
	fi := u.chooseLevel(feats, "pick a tool: ")
	if fi < 0 {
		return true // back to categories, not quit
	}
	cat.list[fi].run()
	return true
}

// chooseLevel wraps choose for the menu levels: single-select semantics.
func (u *UI) chooseLevel(choices []Choice, hint string) int {
	if len(choices) == 0 {
		return -1
	}
	picked := u.choose(hint, choices, false)
	if len(picked) == 0 {
		return -1
	}
	return picked[0]
}

// flowIntro opens every flow with a consistent card: what it does, why,
// and a real example — the same "teach why" pattern as the option panels.
func (u *UI) flowIntro(name, what, example string) {
	fmt.Fprintln(u.Out)
	u.title(name)
	for _, ln := range wrap(what, 72) {
		u.say(anWhite, "  "+ln)
	}
	if example != "" {
		u.say(anCyan, "  ex: "+example)
	}
	fmt.Fprintln(u.Out)
}

// say prints a colored message.
func (u *UI) say(color, msg string) {
	fmt.Fprintln(u.Out, u.paint(color, msg))
}

// title prints a centered-ish banner line.
func (u *UI) title(t string) {
	fmt.Fprintln(u.Out, u.paint(anBold+anMagenta, "── "+t+" "+strings.Repeat("─", max(0, 56-len(t)))))
}

// readLine prints prompt (if non-empty) and reads one line from stdin,
// returning it trimmed of whitespace. On EOF it returns "" (the menu loop
// treats empty as "re-prompt", but flow helpers use EOF as cancel).
func (u *UI) readLine(prompt string) string {
	if prompt != "" {
		fmt.Fprint(u.Out, u.paint(anBold, prompt))
	}
	line, err := u.In.ReadString('\n')
	if err != nil && len(line) == 0 {
		u.eof = true
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(line, "\n"))
}

// askYesNo asks a yes/no question with an explicit default: an empty answer
// (or anything that isn't a clear yes/no) resolves to def. Used for options
// that are OFF by default on the command line (thin, tweakinject, pkgmirror)
// so the menu doesn't silently turn them on.
func (u *UI) askYesNo(prompt string, def bool) bool {
	suffix := " [y/N] "
	if def {
		suffix = " [Y/n] "
	}
	a := u.readLine(prompt + suffix)
	switch strings.ToLower(a) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// pressEnterToContinue waits for Enter unless input is not interactive.
func (u *UI) pressEnterToContinue() {
	if !u.Animate {
		return
	}
	u.readLine(u.paint(anBold, "\n(press enter to go back to the menu) "))
}

// captureLogs runs fn with the log package redirected into a buffer, then
// returns what was logged (so the TUI can show results after the spinner).
func captureLogs(fn func() error) (string, error) {
	var buf bytes.Buffer
	log.SetWriters(&buf, &buf)
	err := fn()
	log.SetWriters(os.Stdout, os.Stderr)
	return buf.String(), err
}

// runSpinning runs fn under the block spinner, returning captured log output
// plus the error. The reason for a failure is always printed by showResult,
// so nothing is folded into the captured output here.
func (u *UI) runSpinning(msg string, fn func() error) (string, error) {
	var out string
	var err error
	u.spin(msg, func() error {
		out, err = captureLogs(fn)
		return err
	})
	return out, err
}

// showResult prints the captured log (if any) with a small header, then a
// success or failure line.
func (u *UI) showResult(out string, err error, okMsg string) {
	trimmed := strings.TrimSpace(out)
	if trimmed != "" {
		fmt.Fprintln(u.Out)
		u.title("what happened")
		fmt.Fprintln(u.Out, trimmed)
	}
	fmt.Fprintln(u.Out)
	if err != nil {
		u.say(anRed, "[fail] "+okMsg)
		u.say(anRed, "  why: "+err.Error())
		u.say(anYellow, "  that didn't work — check the lines above for the full story.")
	} else {
		u.say(anGreen, "[ok] "+okMsg)
	}
}

// isCancel reports whether a typed answer means "leave this flow" (q, quit,
// back, cancel, menu, exit). Used so users who don't know the menu loop can
// bail out of any question without losing their place.
func isCancel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "q", "quit", "exit", "back", "cancel", "menu":
		return true
	}
	return false
}

// pathPrompt asks for a path, defaulting to def if the user enters nothing.
func (u *UI) pathPrompt(question, def string) string {
	suffix := ""
	if def != "" {
		suffix = fmt.Sprintf(" [%s]", def)
	}
	p := u.readLine(question + suffix + " ")
	if p == "" {
		return def
	}
	return p
}

// requirePath loops until a non-empty path is given, backing out to the menu
// on a second empty answer (or an immediate q / quit / back / cancel).
func (u *UI) requirePath(question, def string) string {
	return u.requireAnswer(question, def, "path")
}

// requireText is requirePath for non-path answers (bundle ids, names).
func (u *UI) requireText(question string) string {
	return u.requireAnswer(question, "", "answer")
}

// requireAnswer drives the shared required-input loop used for both paths and
// plain text: the first empty answer gets a nudge, the second one in a row
// sends the user back to the menu, and q / quit / back / cancel does the same
// immediately. Returns "" on cancel.
func (u *UI) requireAnswer(question, def, noun string) string {
	empties := 0
	for {
		p := u.pathPrompt(question, def)
		if isCancel(p) {
			u.say(anYellow, "ok, back to the menu.")
			return ""
		}
		if p != "" {
			return p
		}
		empties++
		if empties == 1 {
			u.say(anYellow, "  that needs a "+noun+". type it, or press enter again to go back to the menu.")
			continue
		}
		u.say(anYellow, "  ok, back to the menu.")
		return ""
	}
}

// loopPaths collects any number of paths: first asks question (empty/cancel
// yields nil), then repeats moreQuestion until an empty answer.
func (u *UI) loopPaths(question, moreQuestion string) []string {
	first := u.pathPrompt(question, "")
	if isCancel(first) {
		return nil
	}
	if first == "" {
		return nil
	}
	out := []string{first}
	for {
		more := u.pathPrompt(moreQuestion, "")
		if isCancel(more) {
			return out
		}
		if more == "" {
			return out
		}
		out = append(out, more)
	}
}

// flowInject is the apps menu's inject feature.
func (u *UI) flowInject() {
	u.flowIntro("inject",
		"you give xkvm an app and one or more tweaks, and it wires them in, signs everything, and hands back a ready-to-sideload file.",
		"Tweak.dylib + App.ipa → App-Tweaked.ipa")
	appPath := u.requirePath("which app do you want to tweak? (path to .ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	files := u.loopPaths("which tweak should I inject? (a .dylib, .deb, .framework, .bundle, or folder)", "add another tweak? (path, or leave empty to stop)")
	if len(files) == 0 {
		u.say(anYellow, "no tweaks to inject — add at least one file.")
		return
	}
	u.runInject(appPath, files)
}

// injectOptions are the expert toggles of runInject; every boolean root
// flag has a row here (plus the dynamic compatibility-patch registry) so
// the menu is never the "lite" version of the CLI.
var injectOptions = []Choice{
	{"fakesign every binary", "sign each Mach-O so the app installs and runs without a jailbreak signer — needed for AltStore/Feather/TrollStore-style sideloading", "-s (menu default: on)", true},
	{"overwrite the output file", "write over an existing output without asking — handy when re-running the same build; off keeps xkvm asking before clobbering", "--overwrite (menu default: on)", true},
	{"add the sideload fixes", "inject the bundled repair dylibs for App Store apps: keychain/keybag fixes and crash-guards for non-jailbroken devices", "--patch; implies fakesign", false},
	{"use the real ElleKit runtime", "rewrite old hooking spellings to @rpath/ElleKit.framework and ship the actual ElleKit — the modern hooking engine most new tweaks target", "--ellekit", false},
	{"thin binaries to arm64", "strip every Mach-O down to the arm64 slice: smaller file, installs only on 64-bit devices (all modern ones)", "-q", false},
	{"remove supported-device limits", "drop the UISupportedDevices list so the app installs on any device instead of a whitelisted model set", "-u", false},
	{"remove watch apps", "delete the bundled watchOS app(s) so the package is smaller and installs can't trip on them", "-w", false},
	{"enable documents support", "adds UIFileSharingEnabled + LSSupportsOpeningDocumentsInPlace so the app shows up in the Files app", "-d", false},
	{"remove all app extensions", "strip every PlugIns/watch/extension binary — the aggressive cleanup for packages that fail on extension entitlements", "-e", false},
	{"remove only encrypted extensions", "strip just the extension binaries that are FairPlay-encrypted (the ones that can't be re-signed anyway)", "-g (implied by -e)", false},
	{"ignore the encryption check", "skip the main-binary encryption warning and inject anyway — for already-decrypted or TrollStore-targeting inputs", "--ignore-encrypted", false},
}

// injectCustoms are the per-value customizations of runInject.
var injectCustoms = []Choice{
	{"bundle id", "change the app's bundle id so it doesn't clash with the App Store copy on the same device", "-b com.example.app", false},
	{"display name", "change the name shown under the icon", "-n \"My App\"", false},
	{"app version", "change the visible version string", "-v 1.2.3", false},
	{"minimum OS version", "raise or lower the minimum iOS the app needs", "-m 15.0", false},
	{"icon", "swap the app icon for a new image (xkvm derives the 120/152px variants)", "-k icon.png", false},
	{"merge a plist", "merge extra keys from a plist file into the app's Info.plist", "-l patch.plist", false},
	{"entitlements", "add or modify entitlements on the main binary from a plist file", "-x entitlements.plist", false},
	{"store country code", "country code used for store lookups (rarely needed)", "-C us", false},
}

// compressChoices present the -c levels with plain-word speed/size notes.
func compressChoices() []Choice {
	labels := []string{
		"level 0 — fastest, biggest file",
		"level 1",
		"level 2 — quick, bigger file",
		"level 3",
		"level 4",
		"level 5",
		"level 6 — balanced (the default)",
		"level 7",
		"level 8 — slow, smaller file",
		"level 9 — slowest, smallest file",
	}
	out := make([]Choice, 0, len(labels))
	for i, l := range labels {
		out = append(out, Choice{Name: l, Desc: "ipa zip compression level: higher = smaller file, slower repack", Def: i == 6})
	}
	return out
}

// runInject collects the output path, .cyan recipes, customizations, expert
// toggles, compression level and per-dylib placement, then runs the
// injection. Shared by flowInject and flowFetch (which hands over the .debs
// it downloaded).
func (u *UI) runInject(appPath string, files []string) {
	if len(files) == 0 {
		u.say(anYellow, "no tweaks to inject — add at least one file.")
		return
	}
	outPath := u.pathPrompt("where should the result go? (leave empty for: <app>-tweaked.ipa)", "")
	opts := &app.Options{
		Input:     appPath,
		Output:    outPath,
		Files:     files,
		Fakesign:  true, // the menu default; togglable in expert options
		Overwrite: true,
		Compress:  6,
	}

	// .cyan recipes (the -z flag).
	cyans := u.loopPaths("apply a .cyan recipe too? (path to .cyan, or leave empty for none)", "another recipe? (leave empty to stop)")
	opts.Cyans = append(opts.Cyans, cyans...)

	// Per-value customizations: multi-pick, then one prompt per pick.
	picked := u.choose("customize the app? (space to pick several, enter to continue)", injectCustoms, true)
	if picked == nil {
		return
	}
	for _, i := range picked {
		c := injectCustoms[i]
		v := u.requireAnswer("give me the "+c.Name+"? (leave empty to skip)", "", c.Name)
		if v == "" {
			return
		}
		switch c.Name {
		case "bundle id":
			opts.BundleID = v
		case "display name":
			opts.Name = v
		case "app version":
			opts.Version = v
		case "minimum OS version":
			opts.MinimumOS = v
		case "icon":
			opts.Icon = v
		case "merge a plist":
			opts.PlistMerge = v
		case "entitlements":
			opts.Entitlements = v
		case "store country code":
			opts.Country = v
		}
	}

	// Expert toggles + the dynamic compatibility-patch registry.
	extras := append([]Choice{}, injectOptions...)
	for _, pname := range patch.Names() {
		extras = append(extras, Choice{
			Name: "compatibility patch: " + pname, Desc: "apply the bundled compatibility patch named " + pname + " (apple fixed-screen / liquid glass / fullscreen semantics)",
			Ex: "--" + pname, Def: false,
		})
	}
	picked = u.choose("expert options (space toggles; fakesign + overwrite start ON like the menu always did)", extras, true)
	if picked == nil {
		return
	}
	toggled := map[string]bool{}
	for _, i := range picked {
		toggled[extras[i].Name] = true
	}
	opts.Fakesign = toggled[injectOptions[0].Name]
	opts.Overwrite = toggled[injectOptions[1].Name]
	opts.Patch = toggled[injectOptions[2].Name]
	opts.ElleKit = toggled[injectOptions[3].Name]
	opts.Thin = toggled[injectOptions[4].Name]
	opts.RemoveSupportedDevices = toggled[injectOptions[5].Name]
	opts.NoWatch = toggled[injectOptions[6].Name]
	opts.EnableDocuments = toggled[injectOptions[7].Name]
	opts.RemoveExtensions = toggled[injectOptions[8].Name]
	opts.RemoveEncrypted = toggled[injectOptions[9].Name]
	opts.IgnoreEncrypted = toggled[injectOptions[10].Name]
	for j := 11; j < len(extras); j++ {
		if toggled[extras[j].Name] {
			pname := strings.TrimPrefix(extras[j].Name, "compatibility patch: ")
			opts.Patches = append(opts.Patches, pname)
		}
	}

	// Compression level (the -c flag). Values are the interface here: the
	// line protocol accepts the level digit itself (0-9); the arrow picker
	// preselects the default (6).
	opts.Compress = u.compressPick()
	if opts.Compress < 0 {
		return
	}

	// Per-dylib app-root placement (the --root-dylib flag).
	for _, f := range files {
		if !strings.HasSuffix(strings.ToLower(f), ".dylib") {
			continue
		}
		if u.askYesNo("keep "+shortName(f)+" at the app root instead of Frameworks/? (yes for tweaks that load files next to themselves — e.g. the Regram-family dlopen tweaks)", false) {
			opts.RootDylibs = append(opts.RootDylibs, f)
		}
	}

	fmt.Fprintln(u.Out)
	out, err := u.runSpinning("injecting "+shortName(files[0])+" into "+shortName(appPath)+"...", func() error {
		return u.Run(context.Background(), opts)
	})
	u.showResult(out, err, "your tweaked app is ready!")
}

// compressPick asks for the zip level and returns the picked value (0-9),
// or -1 on cancel. Typing a digit selects that LEVEL (matching -c on the
// command line); the arrow picker preselects level 6.
func (u *UI) compressPick() int {
	choices := compressChoices()
	if u.InTerm && u.Animate {
		p := u.choose("how should the .ipa be packed?", choices, false)
		if len(p) == 0 {
			return -1
		}
		return p[0]
	}
	for {
		line := u.readLine("how should the .ipa be packed? (level 0-9, or leave empty=6) ")
		if line == "" {
			return 6
		}
		if isCancel(line) {
			return -1
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 0 && n <= 9 {
			return n
		}
		if i, ok := matchChoice(choices, line); ok {
			return i
		}
		u.say(anYellow, "  pick a level from 0 to 9!")
	}
}

// flowExtract is the tweaks menu's extract feature.
func (u *UI) flowExtract() {
	u.flowIntro("extract",
		"xkvm opens an app (or a .deb) and copies the tweaks inside to a folder, writing a placement manifest so re-injecting them later lands each file exactly where it was.",
		"App.ipa → extracted/ with xkvm-manifest.json")
	choices := []Choice{
		{"from an app (.ipa / .tipa / .app)", "open a tweaked app and pull its dylibs, frameworks and bundles out", "App.ipa → extracted/", false},
		{"from a .deb package", "unpack a tweak package into its payload files and placement notes", "Tweak.deb → extracted/", false},
	}
	picked := u.choose("what do you want to pull tweaks from?", choices, false)
	if len(picked) == 0 {
		return
	}
	outDir := u.pathPrompt("where should the tweaks go? (leave empty for: ./extracted)", "extracted")
	if picked[0] == 1 {
		debPath := u.requirePath("which .deb should I open?", "")
		if debPath == "" {
			return
		}
		out, err := u.runSpinning("opening "+shortName(debPath)+"...", func() error {
			return app.Undeb(debPath, outDir)
		})
		u.showResult(out, err, "tweaks extracted to "+outDir+" — inject them with the apps menu!")
		return
	}
	appPath := u.requirePath("which app should I look inside? (.ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	out, err := u.runSpinning("peeking inside "+shortName(appPath)+"...", func() error {
		return app.ExtractArtifacts(appPath, outDir)
	})
	u.showResult(out, err, "tweaks extracted to "+outDir+" — inject them with the apps menu!")
}

// flowConvert is the convert menu's feature: all four conversions with
// their expert flags, each explained before it runs.
func (u *UI) flowConvert() {
	u.flowIntro("convert",
		"xkvm repackages tweaks between the layouts different jailbreaks use — byte-faithful ports of rootless-patcher, RootHidePatcher, Derootifier and Xinam1ne's converter.",
		"rootful Tweak.deb → rootless Tweak.deb (Dopamine/ellekit)")
	choices := []Choice{
		{"rootful → rootless", "the modern jailbreak layout: tweaks live in var/jb, converted the way rootless-patcher does it (Dopamine, ellekit setups)", "Tweak.deb → Tweak-rootless.deb", false},
		{"rootless → roothide", "the RootHidePatcher layout that hides the jailbreak from app detection, including its auto-patch symlink mechanism", "Tweak-rootless.deb → Tweak-roothide.deb", false},
		{"rootful → rootless (Xina style)", "the Xinam1ne/Xina layout with short symlink-form paths (older Xina jailbreaks)", "Tweak.deb → Tweak-xina.deb", false},
		{"rootless → rootful", "convert back to the classic /Library layout (unc0ver, checkra1n and old tweaks)", "Tweak-rootless.deb → Tweak.deb", false},
	}
	picked := u.choose("convert which way?", choices, false)
	if len(picked) == 0 {
		return
	}
	in := u.requirePath("which .deb should I convert?", "")
	if in == "" {
		return
	}
	out := u.pathPrompt("output .deb path? (leave empty for: <name>-converted.deb)", "")
	switch picked[0] {
	case 0:
		thin := u.askYesNo("thin the binaries to arm64 only? (smaller package; 64-bit-device-only)", false)
		tweakInject := u.askYesNo("use the modern TweakInject layout? (DynamicLibraries → var/jb/usr/lib/TweakInject, Derootifier conventions)", false)
		outLog, err := u.runSpinning("converting to rootless...", func() error {
			return app.Rootless(in, out, thin, tweakInject)
		})
		u.showResult(outLog, err, "rootless .deb ready — installable on Dopamine/ellekit setups!")
	case 1:
		pkgmirror := u.askYesNo("use pkgmirror mode? (mirror the package to var/mobile/Library/pkgmirror — roothide's modern install path)", false)
		mode := u.pathPrompt("patch mode? (auto or dynamic — leave empty for auto)", "auto")
		if mode == "" {
			mode = "auto"
		}
		outLog, err := u.runSpinning("converting to roothide...", func() error {
			return app.Roothide(in, out, pkgmirror, mode)
		})
		u.showResult(outLog, err, "roothide .deb ready!")
	case 2:
		outLog, err := u.runSpinning("converting to Xina-style rootless...", func() error {
			return app.RootlessXina(in, out)
		})
		u.showResult(outLog, err, "Xina-style rootless .deb ready!")
	case 3:
		outLog, err := u.runSpinning("converting back to rootful...", func() error {
			return app.Rootful(in, out)
		})
		u.showResult(outLog, err, "rootful .deb ready — classic jailbreaks (unc0ver, checkra1n)!")
	}
}

// flowBuild is the tweaks menu's build feature: debify, cgen, cyan-check.
func (u *UI) flowBuild() {
	u.flowIntro("build",
		"wrap tweaks for sharing: a dylib into a .deb package, a recipe .cyan others can apply, or validate a .cyan before you apply it anywhere.",
		"Tweak.dylib → Tweak.deb · recipe.cyan · check recipe.cyan")
	choices := []Choice{
		{"wrap a dylib into a .deb", "build a classic MobileSubstrate package around a dylib — how tweaks have shipped since the beginning", "Tweak.dylib → Tweak.deb", false},
		{"generate a .cyan recipe", "package tweak(s) into a shareable .cyan config others can apply in one go", "-f Tweak.dylib → recipe.cyan", false},
		{"check a .cyan recipe", "validate a .cyan file and report every problem before anything gets applied", "cyan-check recipe.cyan", false},
	}
	picked := u.choose("build or check what?", choices, false)
	if len(picked) == 0 {
		return
	}
	switch picked[0] {
	case 0:
		u.debifyFlow()
	case 1:
		u.cgenFlow()
	case 2:
		u.cyanCheckFlow()
	}
}

// debifyFlow wraps a dylib into a .deb with the expert fields.
func (u *UI) debifyFlow() {
	dylib := u.requirePath("which dylib should I wrap?", "")
	if dylib == "" {
		return
	}
	out := u.pathPrompt("output .deb path? (leave empty for: <name>.deb)", "")
	extra := u.choose("package details (space to pick several)", []Choice{
		{"target app bundle ids", "bake a Filter/Bundles plist so the tweak only loads in these apps", "--bundle-id com.instagram.ios", false},
		{"extra payload files", "ship more files inside the .deb, written as src:dest pairs", "--resource icon.png:usr/share/icon.png", false},
		{"custom dependencies", "replace the default Depends entry (mobilesubstrate) with your own list", "--depends com.another.tweak", false},
	}, true)
	if extra == nil {
		return
	}
	opts := app.DebifyOptions{Input: dylib, Output: out}
	have := map[int]bool{}
	for _, i := range extra {
		have[i] = true
	}
	if have[0] {
		opts.BundleIDs = append(opts.BundleIDs, strings.Fields(u.readLine("app bundle id(s) to target? (space-separated, or leave empty to skip) "))...)
	}
	if have[1] {
		opts.Resources = append(opts.Resources, strings.Fields(u.readLine("payload files, src:dest pairs? (space-separated, or leave empty to skip) "))...)
	}
	if have[2] {
		opts.Depends = append(opts.Depends, strings.Fields(u.readLine("Depends entries? (space-separated; empty keeps the mobilesubstrate default) "))...)
	}
	outDeb, err := u.runSpinning("wrapping into a .deb...", func() error {
		return app.Debify(opts)
	})
	u.showResult(outDeb, err, "your .deb is ready!")
}

// cgenFlow generates a .cyan recipe with all the generator flags.
func (u *UI) cgenFlow() {
	files := u.loopPaths("which tweak should the recipe include? (.dylib / .deb / folder)", "another item? (leave empty to stop)")
	if len(files) == 0 {
		return
	}
	out := u.pathPrompt("output .cyan path? (leave empty for: tweak.cyan)", "tweak.cyan")
	if isCancel(out) {
		return
	}
	opts := cyanfile.GenerateOptions{Output: out, Files: files}
	extra := u.choose("recipe extras (space to pick several)", []Choice{
		{"app name override", "bake a display-name change into the recipe so applying it renames the app", "-n \"My App\"", false},
		{"fakesign", "bake fakesign into the recipe so every apply re-signs all binaries", "-s", false},
		{"ElleKit runtime", "bake the ElleKit runtime in so applies use the modern hooking engine", "--ellekit", false},
		{"compatibility patches", "bake one or more compatibility patch names into the recipe", "--patch force-fullscreen", false},
		{"app-root placement", "mark included dylibs to live at the app root (@executable_path) instead of Frameworks/", "--root-dylib Tweak.dylib", false},
	}, true)
	if extra == nil {
		return
	}
	have := map[int]bool{}
	for _, i := range extra {
		have[i] = true
	}
	if have[0] {
		if n := u.readLine("app name to bake? (leave empty to skip) "); n != "" {
			opts.Name = n
		}
	}
	if have[1] {
		opts.Fakesign = true
	}
	if have[2] {
		opts.ElleKit = true
	}
	if have[3] {
		var patchChoices []Choice
		for _, pname := range patch.Names() {
			patchChoices = append(patchChoices, Choice{Name: pname, Desc: "bake the bundled compatibility patch: " + pname})
		}
		picked := u.choose("which patches to bake? (space toggles)", patchChoices, true)
		if picked == nil {
			return
		}
		for _, i := range picked {
			opts.Patches = append(opts.Patches, patchChoices[i].Name)
		}
	}
	if have[4] {
		for _, f := range files {
			if !strings.HasSuffix(strings.ToLower(f), ".dylib") {
				continue
			}
			if u.askYesNo("place "+shortName(f)+" at the app root (@executable_path)?", false) {
				opts.RootDylibs = append(opts.RootDylibs, f)
			}
		}
	}
	outCyan, err := u.runSpinning("writing the recipe...", func() error {
		return cyanfile.Generate(opts)
	})
	u.showResult(outCyan, err, ".cyan recipe ready — share it, or apply it with inject!")
}

// cyanCheckFlow validates a .cyan without applying it.
func (u *UI) cyanCheckFlow() {
	cyan := u.requirePath("which .cyan file should I check?", "")
	if cyan == "" {
		return
	}
	out, err := u.runSpinning("checking "+shortName(cyan)+"...", func() error {
		known := map[string]bool{}
		for _, n := range patch.Names() {
			known[n] = true
		}
		issues, err := cyanfile.Validate(cyan, known)
		if err != nil {
			return err
		}
		errs, warns := 0, 0
		for _, is := range issues {
			if is.Level == cyanfile.IssueError {
				errs++
				log.Errorf("problem: %s", is.Message)
			} else {
				warns++
				log.Warnf("warning: %s", is.Message)
			}
		}
		log.Infof("%d problem(s), %d warning(s)", errs, warns)
		if errs > 0 {
			return fmt.Errorf("%d problem(s) found — fix the recipe, then check it again", errs)
		}
		return nil
	})
	u.showResult(out, err, "that .cyan checks out — safe to apply!")
}

// flowFetch is the tweaks menu's fetch feature: resolve bundle ids (with an
// arrow-key version picker per id), fetch dependencies, then offer to
// inject what was downloaded.
func (u *UI) flowFetch() {
	u.flowIntro("fetch",
		"xkvm looks tweaks up by bundle id in the Canister / MobileAPT repos, downloads each with its dependencies, and asks which version you want — latest, or any older one.",
		"com.hbang.watermarks → tweak.deb + dependencies")
	ids := u.loopPaths("which tweak do you want? (its bundle id, e.g. com.hax0r.tweak)", "another tweak id? (leave empty to stop)")
	if len(ids) == 0 {
		return
	}
	var sources []string
	if src := u.pathPrompt("any repo to check first? (a URL like https://repo.chariz.com — optional)", ""); src != "" {
		sources = strings.Fields(src)
	}
	noRecurse := !u.askYesNo("also download each tweak's dependencies? (recommended)", true)
	outDir := u.pathPrompt("where should the downloaded .debs go? (leave empty for: ./fetched)", "fetched")

	// Version picker: one list per id, latest preselected. Injectable
	// FetchVersions feeds it (tests stub it; nil skips the stage).
	var pinned map[string]string
	if u.FetchVersions != nil {
		for _, id := range ids {
			vs, err := u.FetchVersions(context.Background(), id)
			if err != nil {
				if u.Fetch != nil {
					continue // injected resolver without a version source: skip
				}
				continue // repo said nothing; the resolve below reports it
			}
			choices := []Choice{{Name: "latest (recommended)", Desc: "the newest version the repos ship — what the command line picks by default", Ex: id, Def: true}}
			for _, v := range vs {
				if v.Latest {
					continue
				}
				choices = append(choices, Choice{Name: "version " + v.Version, Desc: "download exactly " + v.Version + " of " + id, Ex: "pinned: " + v.Version})
			}
			picked := u.choose("which version of "+id+" do you want?", choices, false)
			if picked == nil {
				return
			}
			if picked[0] > 0 {
				if pinned == nil {
					pinned = map[string]string{}
				}
				pinned[id] = strings.TrimPrefix(choices[picked[0]].Name, "version ")
			}
		}
	}

	var fetched []string
	out, err := u.runSpinning("looking up "+strings.Join(ids, ", ")+"...", func() error {
		paths, ferr := u.fetchTweaks(context.Background(), ids, sources, noRecurse, outDir, pinned)
		if ferr != nil {
			return ferr
		}
		fetched = paths
		return nil
	})
	if err != nil {
		u.showResult(out, err, "nothing fetched")
		return
	}
	u.showResult(out, nil, fmt.Sprintf("fetched %d .deb(s) to %s — inject them with the apps menu!", len(fetched), outDir))
	if len(fetched) == 0 || !u.askYesNo("want to inject this tweak into an app right now?", true) {
		return
	}
	appPath := u.requirePath("which app should I tweak? (.ipa / .tipa / .app)", "")
	if appPath != "" {
		u.runInject(appPath, fetched)
	}
}

// fetchTweaks resolves bundle ids to local .deb paths, honoring version
// pins through FetchPinned when set, then the injectable Fetch, then the
// real Canister/MobileAPT resolver.
func (u *UI) fetchTweaks(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string, pinned map[string]string) ([]string, error) {
	switch {
	case len(pinned) > 0 && u.FetchPinned != nil:
		return u.FetchPinned(ctx, ids, sources, noRecurse, cacheDir, pinned)
	case u.Fetch != nil:
		return u.Fetch(ctx, ids, sources, noRecurse, cacheDir)
	default:
		return fetch.ResolveVersion(ctx, ids, sources, noRecurse, cacheDir, nil, pinned)
	}
}

// flowCheck is the apps menu's check feature: the merge-completeness check,
// with an optional auto-fix pass (the CLI's `check --fix`).
func (u *UI) flowCheck() {
	u.flowIntro("check",
		"xkvm opens an app and makes sure every tweak can find the files it needs (two tiers: every linked dependency must resolve, plus known runtime-dlopen signatures are heuristically matched) — so you don't find out by crashing on the phone.",
		"check -i App.ipa, then fix what's missing")
	appPath := u.requirePath("which app should I check? (.ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	if !u.askYesNo("want me to try to fix missing files automatically? (searches the folders you name + the online repos, then writes a fixed copy)", false) {
		out, err := u.runSpinning("checking "+shortName(appPath)+"...", func() error {
			return app.CheckBundle(appPath)
		})
		u.showResult(out, err, "all clear — every tweak can find what it needs!")
		return
	}
	var fixDirs []string
	if first := u.pathPrompt("which folder should I search for the missing files? (leave empty to skip folders)", ""); first != "" {
		fixDirs = append(fixDirs, first)
		for {
			more := u.pathPrompt("add another search folder? (leave empty to stop)", "")
			if more == "" {
				break
			}
			fixDirs = append(fixDirs, more)
		}
	}
	allowFetch := u.askYesNo("also search the online repos if the folders don't have the file? (needs internet)", true)
	outPath := u.pathPrompt("where should the fixed copy go? (leave empty for: <name>-fixed.ipa)", "")
	fmt.Fprintln(u.Out)
	out, err := u.runSpinning("checking "+shortName(appPath)+" and fixing what's missing...", func() error {
		return app.CheckAndFix(appPath, outPath, fixDirs, app.FixOptions{
			AllowFetch: allowFetch,
			AutoYes:    true, // the menu already confirmed the fix pass
		})
	})
	u.showResult(out, err, "all fixed — every tweak can now find what it needs!")
}

// help prints the plain-language guide with real commands.
func (u *UI) help() {
	u.flowIntro("help",
		"every menu action has a command-line twin for experts, scripts, and AI agents — the same engine runs both.",
		"xkvm -i App.ipa -f Tweak.dylib -o App-Tweaked.ipa")
	guide := []string{
		"  what xkvm can do for you",
		"  ────────────────────────",
		"  • inject  — add one or more tweaks to an app (the main job)",
		"  • extract — take tweaks OUT of an app or a .deb, to reuse them",
		"  • convert — change a package for a different jailbreak (rootful / rootless / roothide / Xina)",
		"  • build   — debify a dylib, or generate / validate a .cyan recipe",
		"  • check   — find missing files before you install",
		"  • fetch   — download a tweak from the repos by its bundle id (pick the version)",
		"  • cache   — see the folder where fetched tweaks are kept, and tidy it",
		"  • decrypt — download an app from the App Store with your Apple ID",
		"",
		"the same tools on the command line",
		"  ────────────────────────────────",
		"  xkvm -i App.ipa -f MyTweak.dylib -o App-Tweaked.ipa        # inject",
		"  xkvm -i App.ipa --fetch com.example.tweak                  # fetch by id",
		"  xkvm extract -i App.ipa -o tweaks/                        # extract (app)",
		"  xkvm undeb -i tweak.deb -o tweaks/                        # extract (.deb)",
		"  xkvm rootless -i tweak.deb -o tweak-rootless.deb [--thin] [--tweakinject]",
		"  xkvm rootless --xina -i tweak.deb -o tweak-xina.deb       # Xina style",
		"  xkvm rootful -i tweak-rootless.deb -o tweak.deb           # back to rootful",
		"  xkvm roothide -i tweak-rootless.deb -o tweak-roothide.deb [--pkgmirror] [--mode auto]",
		"  xkvm debify -i MyTweak.dylib -o MyTweak.deb [--bundle-id com.x.y] [--resource a:b] [--depends x]",
		"  xkvm cgen -o recipe.cyan -f MyTweak.dylib [-n Name] [-s] [--ellekit] [--patch force-fullscreen]",
		"  xkvm check -i App.ipa                     # report only",
		"  xkvm check -i App.ipa --fix -o App-Fixed.ipa [--fix-dir dir/] [--yes] [--no-fetch]",
		"  xkvm cyan-check recipe.cyan               # validate a recipe",
		"  xkvm cache                                # show the fetch cache",
		"  xkvm cache --clear                        # empty the fetch cache",
		"  xkvm decrypt 310633997 --apple-id me@x.com --password …  # download an app",
		"  xkvm decrypt --logout                     # forget the saved login",
		"  xkvm --help                               # every flag, explained",
		"",
		"good to know",
		"  ───────────",
		"  • a .dylib is the actual tweak program; a .deb is a package that",
		"    carries one or more dylibs plus info about where they go",
		"  • fakesign signs the app so a device without a jailbreak accepts it",
		"    (the menu turns this on by default — the CLI needs -s)",
		"  • --patch adds the bundled sideload fixes that keep apps from",
		"    crashing on non-jailbroken devices",
		"  • key hints: ↑/↓ move, space toggles, 1-9 jump, enter confirms, q backs out",
	}
	for _, ln := range guide {
		if strings.HasPrefix(ln, "  •") {
			fmt.Fprintln(u.Out, u.paint(anCyan, ln))
		} else {
			fmt.Fprintln(u.Out, ln)
		}
	}
}

// about explains what xkvm is.
func (u *UI) about() {
	u.flowIntro("about xkvm",
		"a from-scratch Go rewrite that merges the ecosystem's tweak tooling into one fast static binary.",
		"")
	about := []string{
		"xkvm merges three popular iOS tools:",
		"",
		"  • cyan / pyzule-rw — inject tweaks into apps and .ipa files",
		"  • Azule            — fetch tweaks from repos, decrypt App Store apps",
		"  • the deb converters (Derootifier, RootHidePatcher, rootless-patcher,",
		"    Xinam1ne) — repackage tweaks for modern jailbreaks",
		"",
		"why Go? the original tools were Python, and grew slow and fragile.",
		"xkvm is one fast, statically-typed binary that does all of it — with",
		"no Python, no extra runtime, no 'it works on my machine'.",
		"",
		"the name: xkvm (pronounced like 'ex-kay-vee-em') comes from 'xKVM',",
		"the codename for this toolbox.",
	}
	for _, ln := range about {
		fmt.Fprintln(u.Out, ln)
	}
}

// flowCache is the tweaks menu's cache feature: the visible face of the
// persistent fetch cache (7-day TTL, default-dir-only pruning).
func (u *UI) flowCache() {
	u.flowIntro("cache",
		"xkvm keeps downloaded tweaks in a local folder so fetching the same tweak twice doesn't download it again. Entries older than 7 days are removed automatically on the next fetch.",
		"cache --clear empties everything at once")
	dir, debs, bytes, err := u.CacheUsage()
	if err != nil {
		u.say(anRed, "couldn't read the cache: "+err.Error())
		return
	}
	u.say(anCyan, "cache folder: "+dir)
	if debs == 0 {
		u.say(anGreen, "it's empty right now — fetch something and it'll show up here.")
		return
	}
	u.say(anCyan, fmt.Sprintf("%d cached tweak(s), %s", debs, fetch.HumanBytes(bytes)))

	choices := []Choice{
		{"remove only the old entries", "delete entries 7 days or older — the same prune a fetch does automatically", "prune the TTL-expired .debs", false},
		{"clear the whole cache", "delete every cached tweak — a fetch after this downloads everything fresh", "start over from an empty folder", false},
	}
	picked := u.choose("what should I do with the cache?", choices, false)
	if len(picked) == 0 {
		return
	}
	var out string
	var werr error
	switch picked[0] {
	case 0:
		out, werr = u.runSpinning("removing old entries...", func() error {
			n, rerr := u.CachePrune()
			if rerr != nil {
				return rerr
			}
			log.Infof("removed %d expired entry/ies", n)
			return nil
		})
		u.showResult(out, werr, "old entries cleaned up!")
	case 1:
		out, werr = u.runSpinning("clearing the cache...", func() error {
			n, rerr := u.CacheClear()
			if rerr != nil {
				return rerr
			}
			log.Infof("removed %d cached .deb(s)", n)
			return nil
		})
		u.showResult(out, werr, "cache cleared — the next fetch starts fresh.")
	}
}

// flowDecrypt is the apps menu's decrypt feature: download an App Store app
// by Apple ID (the ipatool / PancakeStore flow) so it can be tweaked.
func (u *UI) flowDecrypt() {
	u.flowIntro("decrypt",
		"xkvm signs into the App Store with your Apple ID and downloads an app's IPA — the same flow ipatool and PancakeStore use. Sign in once; the session is remembered on this machine.",
		"310633997 or an apps.apple.com link → App.id.ipa")
	u.say(anYellow, "the app you want must:")
	u.say(anWhite, "  • have been purchased with the same Apple ID you sign in with")
	u.say(anWhite, "  • not be installed on your device (offloading works)")
	u.say(anWhite, "  • have valid versions to download (some apps don't)")
	u.say(anYellow, "if an app can't be downloaded or crashes after launch, xkvm can't fix it.")
	fmt.Fprintln(u.Out)

	appleID, password := "", ""
	if u.HasSavedAuth() {
		q := "signed in"
		if id, ok := u.SavedAppleID(); ok {
			q = "signed in as " + id
		}
		choices := []Choice{
			{"keep using this Apple ID", "reuse the saved session — no password prompt", "the saved login stays", false},
			{"change to a different Apple ID", "sign in with another account and replace the saved session", "switch accounts", false},
			{"log out of xkvm", "forget the saved login on this machine, then sign in again", "remove the stored session", false},
		}
		switch picked := u.choose(q, choices, false); {
		case len(picked) == 0:
			return
		case picked[0] == 0: // keep — the saved session is used as-is
		case picked[0] == 1: // change account
			appleID, password = u.askCredentials()
			if appleID == "" {
				return
			}
		case picked[0] == 2: // log out, then sign in fresh
			if err := u.Logout(); err != nil {
				u.say(anRed, "couldn't log out: "+err.Error())
			} else {
				u.say(anGreen, "signed out. the saved login is gone.")
			}
			appleID, password = u.askCredentials()
			if appleID == "" {
				return
			}
		}
	} else {
		appleID, password = u.askCredentials()
		if appleID == "" {
			return
		}
	}

	appInput := u.requireText("what app? paste the App Store link, the numeric id, or a bundle id")
	if appInput == "" {
		return
	}
	version := u.readLine("specific app version? (external version id, e.g. 8612345678 — leave empty for the latest) ")

	// Output directory: ask unless the user chose "never ask again".
	prefs, _ := u.DecryptPrefs()
	outDir := prefs.OutputDir
	if prefs.AskMode == decrypt.AskModeReuse || prefs.AskMode == decrypt.AskModeNever {
		if outDir == "" {
			outDir = "."
		}
	} else {
		choices := []Choice{
			{"pick a folder", "choose where this download goes (and remember the folder for next time)", "output folder: e.g. ~/Downloads", false},
			{"reuse the last one", "save into the folder you used before (or this folder if none yet)", outDir, false},
			{"never ask again (always use the same folder)", "always save into the remembered folder without the question", outDir, false},
		}
		picked := u.choose("where should the output .ipa go?", choices, false)
		if len(picked) == 0 {
			return
		}
		switch picked[0] {
		case 0:
			dir := u.pathPrompt("output folder", ".")
			if isCancel(dir) {
				return
			}
			outDir = dir
			prefs.AskMode = decrypt.AskModeAsk
		case 1:
			if outDir == "" {
				outDir = "."
			}
			prefs.AskMode = decrypt.AskModeAsk
		case 2:
			if outDir == "" {
				outDir = "."
			}
			prefs.AskMode = decrypt.AskModeNever
		}
		prefs.OutputDir = outDir
		_ = u.SaveDecryptPrefs(prefs)
	}

	var path string
	out, err := u.runSpinning("downloading from the App Store...", func() error {
		p, derr := u.Decrypt(context.Background(), app.DecryptOptions{
			AppleID:   appleID,
			Password:  password,
			AppID:     appInput,
			Version:   version,
			OutputDir: outDir,
		})
		if derr != nil {
			return derr
		}
		path = p
		return nil
	})
	if err != nil {
		u.showResult(out, err, "nothing downloaded")
		u.say(anYellow, "  tip: if the saved sign-in expired, pick 'change to a different Apple ID' next time and sign in again.")
		return
	}
	u.showResult(out, nil, "downloaded to "+path+" — inject tweaks into it with the apps menu.")
}

// askCredentials prompts for an Apple ID and password. The password is stored
// on this machine so the next download skips the login step.
func (u *UI) askCredentials() (string, string) {
	appleID := u.requireText("your Apple ID (email)")
	if appleID == "" {
		return "", ""
	}
	password := u.requireText("your Apple ID password (stored on this machine for reuse)")
	if password == "" {
		return "", ""
	}
	return appleID, password
}

// shortName returns a path's final component, trimmed to ~40 chars.
func shortName(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "?"
	}
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		p = p[i+1:]
	}
	if len(p) > 40 {
		p = p[:37] + "..."
	}
	return p
}
