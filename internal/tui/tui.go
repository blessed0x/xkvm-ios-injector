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
	"github.com/xscope0/xkvm-ios-injector/internal/log"
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
}

// New returns a UI for the current terminal. Color and animation are enabled
// only when stdout is a terminal AND the NO_COLOR convention isn't set.
func New() *UI {
	term := isTerminal(os.Stdout)
	return &UI{
		In:      bufio.NewReader(os.Stdin),
		Out:     os.Stdout,
		Err:     os.Stderr,
		Color:   term && os.Getenv("NO_COLOR") == "",
		Animate: term,
		Run:     app.Run,
	}
}

// NewForTest builds a non-animated UI reading from r and writing to w.
func NewForTest(r io.Reader, w io.Writer) *UI {
	return &UI{
		In:    bufio.NewReader(r),
		Out:   w,
		Err:   w,
		Color: false,
		Run:   app.Run,
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
		default:
			u.say(anYellow, "hmm, I didn't get that. try a number from the list!")
		}
		u.pressEnterToContinue()
	}
}

// menu prints the friendly menu card.
func (u *UI) menu() {
	fmt.Fprintln(u.Out)
	u.title("pick a tool")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  1. "+anBold+"inject")+"  —  put a tweak (dylib / deb / .cyan) into an app or .ipa")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  2. "+anBold+"extract")+" —  pull tweaks OUT of an app so you can reuse them")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  3. "+anBold+"convert")+" —  change a tweak package format (deb / rootless / roothide)")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  4. "+anBold+"build")+"   —  wrap a dylib into a shareable .deb or .cyan file")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  5. "+anBold+"check")+"   —  make sure an app's tweaks won't crash (missing files?)")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  6. "+anBold+"help")+"    —  plain-language guide + real command examples")
	fmt.Fprintln(u.Out, u.paint(anCyan, "  7. "+anBold+"about")+"   —  what xkvm is and why it exists")
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
	a := u.readLine(prompt + " [Y/n] ")
	switch strings.ToLower(a) {
	case "n", "no":
		return false
	default:
		return true
	}
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
// plus the error. When fn fails without logging anything itself (a plain
// returned error), the error text is folded into the captured output so the
// "what happened" panel always says why.
func (u *UI) runSpinning(msg string, fn func() error) (string, error) {
	var out string
	var err error
	u.spin(msg, func() error {
		out, err = captureLogs(fn)
		return err
	})
	if err != nil && strings.TrimSpace(out) == "" {
		out = err.Error()
	}
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
		u.say(anYellow, "  that didn't work — check the lines above, they usually say why.")
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
		line := u.readLine("pick one (number) — or leave empty to cancel: ")
		if line == "" {
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

// requirePath loops until a non-empty path is given (or EOF → cancel).
func (u *UI) requirePath(question, def string) string {
	for {
		p := u.pathPrompt(question, def)
		if p != "" {
			return p
		}
		u.say(anYellow, "  that needs a path — try again, or press ctrl-c to stop.")
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
	outPath := u.pathPrompt("where should the result go? (leave empty for: <app>-tweaked.ipa)", "")
	opts := &app.Options{
		Input:     appPath,
		Output:    outPath,
		Files:     []string{tweak},
		Fakesign:  true, // every sideload path needs a signature
		Overwrite: true,
	}
	if u.confirm("should I also inject the bundled 'sideload fixes' (helps apps work on devices without a jailbreak)?") {
		opts.Patch = true
	}
	fmt.Fprintln(u.Out)
	out, err := u.runSpinning("injecting "+shortName(tweak)+" into "+shortName(appPath)+"...", func() error {
		return u.Run(context.Background(), opts)
	})
	u.showResult(out, err, "your tweaked app is ready!")
}

// flowExtract is menu 2: pull tweaks out of an app.
func (u *UI) flowExtract() {
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
		out, err := u.runSpinning("converting to rootless...", func() error {
			return app.Rootless(in, out, false, false)
		})
		u.showResult(out, err, "rootless .deb ready — installable on Dopamine/ellekit setups!")
	case 1:
		out, err := u.runSpinning("converting to roothide...", func() error {
			return app.Roothide(in, out, false, "")
		})
		u.showResult(out, err, "roothide .deb ready!")
	}
}

// flowBuild is menu 4: wrap a dylib into a .deb or generate a .cyan config.
func (u *UI) flowBuild() {
	i := u.pickOne("build something shareable", []string{
		"wrap a dylib into a .deb  (classic jailbreak package)",
		"generate a .cyan config  (a recipe other people can apply)",
	})
	if i < 0 {
		return
	}
	switch i {
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
		"  • extract — take tweaks OUT of an app, to reuse them",
		"  • convert — change a package for a different jailbreak",
		"  • build   — package a dylib for sharing",
		"  • check   — find missing files before you install",
		"",
		"the same tools on the command line",
		"  ────────────────────────────────",
		"  xkvm -i App.ipa -f MyTweak.dylib -o App-Tweaked.ipa   # inject",
		"  xkvm extract -i App.ipa -o tweaks/                   # extract",
		"  xkvm rootless -i tweak.deb -o tweak-rootless.deb     # convert",
		"  xkvm debify -i MyTweak.dylib -o MyTweak.deb          # build a .deb",
		"  xkvm check -i App-Tweaked.ipa                        # check for trouble",
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
