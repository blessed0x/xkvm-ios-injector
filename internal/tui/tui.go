// Package tui is the friendly, menu-driven way to use xkvm. It drives the
// same internal/app functions the CLI uses — no separate code path — but
// asks plain-language questions, animates work with a block spinner, and
// explains what each tool does before it runs.
//
// Zero dependencies: ANSI colors, block characters, and a bufio prompt loop.
// When stdin is not a terminal (pipes, tests) it degrades gracefully: no
// animation, and menu input can be piped line by line.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/app"
	"github.com/xscope0/xkvm-ios-injector/internal/cyanfile"
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
	Animate bool // animated banner + spinner on
	// Run is injectable (tests capture instead of touching real apps).
	Run func(ctx context.Context, opts *app.Options) error
	// Fetch resolves tweak bundle ids to local .deb paths. Injectable so tests
	// don't hit the network; the real resolver is fetch.Resolve.
	Fetch func(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string) ([]string, error)
	// CacheUsage reports the fetch cache (directory, deb count, total bytes),
	// CachePrune removes entries older than the 7-day TTL, and CacheClear
	// removes every entry. Injectable so tests stay off the real user cache
	// dir; the defaults are fetch.CacheUsage / PruneExpired / ClearCache.
	CacheUsage func() (dir string, debs int, bytes int64, err error)
	CachePrune func() (removed int, err error)
	CacheClear func() (removed int, err error)
}

// New returns a UI for the current terminal. Color and animation are enabled
// only when stdout is a terminal AND the NO_COLOR convention isn't set.
func New() *UI {
	term := isTerminal(os.Stdout)
	return &UI{
		In:         bufio.NewReader(os.Stdin),
		Out:        os.Stdout,
		Err:        os.Stderr,
		Color:      term && os.Getenv("NO_COLOR") == "",
		Animate:    term,
		Run:        app.Run,
		CacheUsage: fetch.CacheUsage,
		CachePrune: fetch.PruneExpired,
		CacheClear: fetch.ClearCache,
	}
}

// NewForTest builds a non-animated UI reading from r and writing to w.
func NewForTest(r io.Reader, w io.Writer) *UI {
	return &UI{
		In:         bufio.NewReader(r),
		Out:        w,
		Err:        w,
		Color:      false,
		Run:        app.Run,
		CacheUsage: fetch.CacheUsage,
		CachePrune: fetch.PruneExpired,
		CacheClear: fetch.ClearCache,
	}
}

// Start shows the animated banner, then runs the menu loop until the user
// picks Quit (or input ends).
func (u *UI) Start() {
	u.animateBanner()
	u.pressEnterToContinue()
	for {
		u.menu()
		choice := u.readLine("\nwhat do you want to do? (type a number, or q to quit) ")
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "q", "quit", "exit":
			u.say(anGreen, "bye! see you next time")
			return
		case "1", "inject", "i":
			u.flowInject()
		case "2", "extract", "x":
			u.flowExtract()
		case "3", "convert", "c":
			u.flowConvert()
		case "4", "build", "b":
			u.flowBuild()
		case "5", "check", "k":
			u.flowCheck()
		case "6", "help", "h":
			u.help()
		case "7", "about", "a":
			u.about()
		case "8", "fetch", "f":
			u.flowFetch()
		case "9", "cache":
			u.flowCache()
		default:
			u.say(anYellow, "hmm, I didn't get that. try a number from the list, or q to quit.")
		}
		u.pressEnterToContinue()
	}
}

// menu prints the friendly menu card.
func (u *UI) menu() {
	fmt.Fprintln(u.Out)
	u.title("pick a tool")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  1. "+anBold+"inject")+"  —  put a tweak (dylib / deb / .cyan) into an app or .ipa")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  2. "+anBold+"extract")+" —  pull tweaks OUT of an app or a .deb so you can reuse them")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  3. "+anBold+"convert")+" —  change a tweak package format (rootful / rootless / roothide)")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  4. "+anBold+"build")+"   —  wrap a dylib into a shareable .deb or .cyan file")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  5. "+anBold+"check")+"   —  make sure an app's tweaks won't crash (missing files?)")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  6. "+anBold+"help")+"    —  plain-language guide + real command examples")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  7. "+anBold+"about")+"   —  what xkvm is and why it exists")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  8. "+anBold+"fetch")+"   —  download a tweak by its bundle id from the repos")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  9. "+anBold+"cache")+"   —  see and tidy the folder where downloaded tweaks are stored")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  q. "+anBold+"quit")+"   —  leave xkvm")
}

// say prints a colored message.
func (u *UI) say(color, msg string) {
	fmt.Fprintln(u.Out, u.paint(color, msg))
}

// title prints a centered-ish banner line.
func (u *UI) title(t string) {
	fmt.Fprintln(u.Out, u.paint(anBold+anMagenta, "── "+t+" "+strings.Repeat("─", 56-len(t))))
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
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(line, "\n"))
}

// confirm asks a yes/no question; the default (empty answer) is yes.
func (u *UI) confirm(prompt string) bool {
	return u.askYesNo(prompt, true)
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

// pickOne presents numbered options and returns the chosen index.
func (u *UI) pickOne(question string, options []string) int {
	fmt.Fprintln(u.Out)
	u.title(question)
	for i, o := range options {
		fmt.Fprintf(u.Out, "  %s%d. %s%s\n", u.paint(anCyan, ""), i+1, o, u.paint(anCyan, ""))
	}
	for {
		line := u.readLine("pick one (number), q to quit, or leave empty to cancel: ")
		if line == "" || isCancel(line) {
			return -1
		}
		for i := range options {
			if line == fmt.Sprintf("%d", i+1) || strings.EqualFold(line, options[i]) {
				return i
			}
		}
		u.say(anYellow, "  try a number from the list!")
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

// flowInject is menu 1: inject tweak(s) into an app/ipa.
func (u *UI) flowInject() {
	u.title("inject")
	u.say(anWhite, "you give xkvm an app (.ipa / .tipa / .app) and a tweak, and it puts the")
	u.say(anWhite, "tweak inside, signs everything, and hands back a ready-to-sideload file.")
	fmt.Fprintln(u.Out)
	appPath := u.requirePath("which app do you want to tweak? (path to .ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	tweak := u.requirePath("which tweak do you want to inject? (a .dylib, .deb, .cyan, .bundle, or folder)", "")
	if tweak == "" {
		return
	}
	u.runInject(appPath, []string{tweak})
}

// runInject collects the output path and option questions, then runs the
// injection. Shared by flowInject and flowFetch (which hands over the .debs
// it downloaded).
func (u *UI) runInject(appPath string, files []string) {
	outPath := u.pathPrompt("where should the result go? (leave empty for: <app>-tweaked.ipa)", "")
	opts := &app.Options{
		Input:     appPath,
		Output:    outPath,
		Files:     files,
		Fakesign:  true, // every sideload path needs a signature
		Overwrite: true,
	}
	if u.confirm("change the app's bundle id, name, or icon? (useful when re-signing so apps don't clash)") {
		if v := u.pathPrompt("new bundle id? (leave empty to keep the current one)", ""); v != "" {
			opts.BundleID = v
		}
		if v := u.pathPrompt("new display name? (leave empty to keep the current one)", ""); v != "" {
			opts.Name = v
		}
		if v := u.pathPrompt("path to a new icon image? (leave empty to keep the current one)", ""); v != "" {
			opts.Icon = v
		}
	}
	if u.confirm("should I also inject the bundled 'sideload fixes' (helps apps work on devices without a jailbreak)?") {
		opts.Patch = true
	}
	if u.confirm("use the real ElleKit runtime for hooking? (the modern hooking engine; pick this if a tweak asks for ellekit)") {
		opts.ElleKit = true
	}
	if len(files) == 1 && strings.HasSuffix(strings.ToLower(files[0]), ".dylib") {
		if u.confirm("keep this tweak at the app root instead of in Frameworks/? (yes, for dylibs that load files next to themselves)") {
			opts.RootDylibs = append(opts.RootDylibs, files[0])
		}
	}
	fmt.Fprintln(u.Out)
	out, err := u.runSpinning("injecting "+shortName(files[0])+" into "+shortName(appPath)+"...", func() error {
		return u.Run(context.Background(), opts)
	})
	u.showResult(out, err, "your tweaked app is ready!")
}

// flowExtract is menu 2: pull tweaks out of an app or a .deb.
func (u *UI) flowExtract() {
	i := u.pickOne("what do you want to pull tweaks from?", []string{
		"an app (.ipa / .tipa / .app)",
		"a .deb package",
	})
	if i < 0 {
		return
	}
	if i == 1 {
		u.title("extract from a .deb")
		u.say(anWhite, "xkvm opens a tweak .deb and copies the dylibs and bundles inside to a")
		u.say(anWhite, "folder — so you can reuse them in an app, or re-inject them elsewhere.")
		fmt.Fprintln(u.Out)
		debPath := u.requirePath("which .deb should I open?", "")
		if debPath == "" {
			return
		}
		outDir := u.pathPrompt("where should the tweaks go? (leave empty for: ./extracted)", "extracted")
		out, err := u.runSpinning("opening "+shortName(debPath)+"...", func() error {
			return app.Undeb(debPath, outDir)
		})
		u.showResult(out, err, "tweaks extracted to "+outDir+" — re-inject them with menu 1!")
		return
	}
	u.title("extract")
	u.say(anWhite, "xkvm opens an app and copies the tweaks inside (dylibs, frameworks,")
	u.say(anWhite, "bundles) to a folder — so you can reuse them in another app.")
	fmt.Fprintln(u.Out)
	appPath := u.requirePath("which app should I look inside? (.ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	outDir := u.pathPrompt("where should the tweaks go? (leave empty for: ./extracted)", "extracted")
	out, err := u.runSpinning("peeking inside "+shortName(appPath)+"...", func() error {
		return app.ExtractArtifacts(appPath, outDir)
	})
	u.showResult(out, err, "tweaks extracted to "+outDir+" — re-inject them with menu 1!")
}

// flowConvert is menu 3: package format conversions.
func (u *UI) flowConvert() {
	opts := []string{
		"rootful .deb  →  rootless .deb  (modern jailbreaks: Dopamine, ellekit)",
		"rootless .deb →  roothide .deb  (the roothide jailbreak)",
		"rootful .deb  →  rootless .deb, Xina style  (short symlink paths)",
		"rootless .deb →  rootful .deb  (back to the classic layout)",
	}
	i := u.pickOne("convert a tweak package", opts)
	if i < 0 {
		return
	}
	in := u.requirePath("which .deb should I convert?", "")
	if in == "" {
		return
	}
	out := u.pathPrompt("output .deb path? (leave empty for: <name>-converted.deb)", "")
	switch i {
	case 0:
		// The CLI's --thin / --tweakinject, both off by default.
		thin := u.askYesNo("thin the binaries to arm64 only?", false)
		tweakInject := u.askYesNo("use the modern TweakInject layout? (DynamicLibraries → var/jb/usr/lib/TweakInject)", false)
		out, err := u.runSpinning("converting to rootless...", func() error {
			return app.Rootless(in, out, thin, tweakInject)
		})
		u.showResult(out, err, "rootless .deb ready — installable on Dopamine/ellekit setups!")
	case 1:
		// The CLI's --pkgmirror / --mode.
		pkgmirror := u.askYesNo("use pkgmirror mode? (symlink-based patches, roothide's modern install path)", false)
		mode := u.pathPrompt("patch mode? (auto or dynamic — leave empty for auto)", "auto")
		if mode == "" {
			mode = "auto"
		}
		out, err := u.runSpinning("converting to roothide...", func() error {
			return app.Roothide(in, out, pkgmirror, mode)
		})
		u.showResult(out, err, "roothide .deb ready!")
	case 2:
		out, err := u.runSpinning("converting to Xina-style rootless...", func() error {
			return app.RootlessXina(in, out)
		})
		u.showResult(out, err, "Xina-style rootless .deb ready!")
	case 3:
		out, err := u.runSpinning("converting back to rootful...", func() error {
			return app.Rootful(in, out)
		})
		u.showResult(out, err, "rootful .deb ready — classic jailbreaks (unc0ver, checkra1n)!")
	}
}

// flowBuild is menu 4: wrap a dylib into a .deb or generate a .cyan config.
func (u *UI) flowBuild() {
	i := u.pickOne("build or check something", []string{
		"wrap a dylib into a .deb  (classic jailbreak package)",
		"generate a .cyan config  (a recipe other people can apply)",
		"check a .cyan config  (find problems before applying it)",
	})
	if i < 0 {
		return
	}
	switch i {
	case 2:
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
	case 0:
		dylib := u.requirePath("which dylib should I wrap?", "")
		if dylib == "" {
			return
		}
		out := u.pathPrompt("output .deb path? (leave empty for: <name>.deb)", "")
		bundleID := u.pathPrompt("which app should it work in? (its bundle id, e.g. com.instagram.ios — optional)", "")
		var bids []string
		if bundleID != "" {
			bids = []string{bundleID}
		}
		outDeb, err := u.runSpinning("wrapping into a .deb...", func() error {
			return app.Debify(app.DebifyOptions{Input: dylib, Output: out, BundleIDs: bids})
		})
		u.showResult(outDeb, err, "your .deb is ready!")
	case 1:
		dylib := u.requirePath("which tweak should the recipe include?", "")
		if dylib == "" {
			return
		}
		out := u.pathPrompt("output .cyan path? (leave empty for: tweak.cyan)", "tweak.cyan")
		outCyan, err := u.runSpinning("writing the recipe...", func() error {
			return cyanfile.Generate(cyanfile.GenerateOptions{Output: out, Files: []string{dylib}})
		})
		u.showResult(outCyan, err, ".cyan recipe ready — share it, or apply it with menu 1!")
	}
}

// flowFetch is menu 8: download a tweak by its bundle id from the
// Canister/MobileAPT repos, then offer to inject it into an app.
func (u *UI) flowFetch() {
	u.title("fetch")
	u.say(anWhite, "xkvm looks up a tweak by its bundle id (like com.example.tweak) in the")
	u.say(anWhite, "Canister / MobileAPT repos, downloads the .deb and its dependencies,")
	u.say(anWhite, "and puts them in a folder you choose.")
	fmt.Fprintln(u.Out)
	id := u.requireText("which tweak do you want? (its bundle id, e.g. com.hax0r.tweak)")
	if id == "" {
		return
	}
	// The CLI's --apt-source, one URL per line (space-separated is fine).
	var sources []string
	if src := u.pathPrompt("any repo to check first? (a URL like https://repo.chariz.com — optional)", ""); src != "" {
		sources = strings.Fields(src)
	}
	// The CLI's --no-recurse, inverted into a friendly question: dependencies
	// are fetched by default, matching the CLI.
	noRecurse := !u.askYesNo("also download this tweak's dependencies?", true)
	outDir := u.pathPrompt("where should the downloaded .debs go? (leave empty for: ./fetched)", "fetched")
	var fetched []string
	out, err := u.runSpinning("looking up "+id+"...", func() error {
		paths, ferr := u.fetchTweaks(context.Background(), []string{id}, sources, noRecurse, outDir)
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
	u.showResult(out, nil, fmt.Sprintf("fetched %d .deb(s) to %s — re-inject them with menu 1!", len(fetched), outDir))
	if len(fetched) == 0 || !u.confirm("want to inject this tweak into an app right now?") {
		return
	}
	appPath := u.requirePath("which app should I tweak? (.ipa / .tipa / .app)", "")
	if appPath != "" {
		u.runInject(appPath, fetched)
	}
}

// fetchTweaks resolves bundle ids to local .deb paths, using the injectable
// Fetch when set and the real Canister/MobileAPT resolver otherwise.
func (u *UI) fetchTweaks(ctx context.Context, ids, sources []string, noRecurse bool, cacheDir string) ([]string, error) {
	if u.Fetch != nil {
		return u.Fetch(ctx, ids, sources, noRecurse, cacheDir)
	}
	return fetch.Resolve(ctx, ids, sources, noRecurse, cacheDir, nil)
}

// flowCheck is menu 5: the merge-completeness check.
func (u *UI) flowCheck() {
	u.title("check")
	u.say(anWhite, "xkvm looks at every file inside an app and makes sure each tweak can")
	u.say(anWhite, "find everything it needs — so you don't find out by crashing.")
	fmt.Fprintln(u.Out)
	appPath := u.requirePath("which app should I check? (.ipa / .tipa / .app)", "")
	if appPath == "" {
		return
	}
	out, err := u.runSpinning("checking "+shortName(appPath)+"...", func() error {
		return app.CheckBundle(appPath)
	})
	u.showResult(out, err, "all clear — every tweak can find what it needs!")
}

// help prints the plain-language guide with real commands.
func (u *UI) help() {
	u.title("help — in plain words")
	guide := []string{
		"",
		"xkvm is a toolbox for iOS apps. tweaks are small programs that change how",
		"an app behaves. xkvm puts them inside apps (.ipa files) and converts",
		"tweak packages between the formats different jailbreaks use.",
		"",
		"  what xkvm can do for you",
		"  ────────────────────────",
		"  • inject  — add a tweak to an app (the main job)",
		"  • extract — take tweaks OUT of an app or a .deb, to reuse them",
		"  • convert — change a package for a different jailbreak (rootful / rootless / roothide)",
		"  • build   — package a dylib for sharing",
		"  • check   — find missing files before you install",
		"  • cyan-check — find problems in a .cyan recipe before you apply it",
		"  • fetch   — download a tweak from the repos by its bundle id",
		"  • cache   — see the folder where fetched tweaks are kept (reused so",
		"    you don't download twice, auto-cleaned after 7 days), and tidy it",
		"",
		"the same tools on the command line",
		"  ────────────────────────────────",
		"  xkvm -i App.ipa -f MyTweak.dylib -o App-Tweaked.ipa   # inject",
		"  xkvm -i App.ipa --fetch com.example.tweak -o App-Tweaked.ipa   # fetch a tweak by id",
		"  xkvm extract -i App.ipa -o tweaks/                   # extract from an app",
		"  xkvm undeb -i tweak.deb -o tweaks/                   # extract from a .deb",
		"  xkvm rootless -i tweak.deb -o tweak-rootless.deb     # convert to rootless",
		"  xkvm rootless --xina -i tweak.deb -o tweak-xina.deb  # convert to rootless, Xina style",
		"  xkvm rootful -i tweak-rootless.deb -o tweak.deb      # convert back to rootful",
		"  xkvm debify -i MyTweak.dylib -o MyTweak.deb          # build a .deb",
		"  xkvm check -i App-Tweaked.ipa                        # check for trouble",
		"  xkvm cyan-check recipe.cyan                          # check a recipe",
		"  xkvm cache                                          # show the fetch cache folder",
		"  xkvm cache --clear                                  # empty the fetch cache",
		"  xkvm --help                                          # every flag, explained",
		"",
		"good to know",
		"  ───────────",
		"  • a .dylib is the actual tweak program; a .deb is a package that",
		"    carries one or more dylibs + info about where they go",
		"  • --fakesign signs the app so a device without a jailbreak accepts it",
		"    (xkvm turns this on automatically in the menu)",
		"  • --patch adds the bundled sideload fixes that keep apps from",
		"    crashing on non-jailbroken devices",
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
	u.title("about xkvm")
	about := []string{
		"",
		"xkvm is a from-scratch Go rewrite that merges three popular iOS tools:",
		"",
		"  • cyan / pyzule-rw — inject tweaks into apps and .ipa files",
		"  • Azule            — fetch tweaks from repos, decrypt App Store apps",
		"  • the deb converters (Derootifier, RootHidePatcher, rootless-patcher)",
		"    — repackage tweaks for modern jailbreaks",
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

// flowCache is menu 9: show the persistent fetch cache and tidy it. This is
// the visible face of the smart dependency solver — the folder where
// downloaded tweaks are kept so a repeat fetch is served from disk instead
// of the network, auto-cleaned after 7 days.
func (u *UI) flowCache() {
	u.title("cache")
	u.say(anWhite, "xkvm keeps downloaded tweaks in a folder on this computer, so fetching")
	u.say(anWhite, "the same tweak twice doesn't download it again. entries older than 7 days")
	u.say(anWhite, "are removed automatically the next time xkvm fetches.")
	fmt.Fprintln(u.Out)

	dir, debs, bytes, err := u.CacheUsage()
	if err != nil {
		u.say(anRed, "couldn't read the cache: "+err.Error())
		return
	}
	u.say(anCyan, "cache folder: "+dir)
	if debs == 0 {
		u.say(anGreen, "it's empty right now — fetch something (menu 8) and it'll show up here.")
		return
	}
	u.say(anCyan, fmt.Sprintf("%d cached tweak(s), %s", debs, fetch.HumanBytes(bytes)))

	i := u.pickOne("what should I do with the cache?", []string{
		"remove only the old entries (7 days or older)",
		"clear the whole cache",
	})
	if i < 0 {
		return
	}
	var out string
	var werr error
	switch i {
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
