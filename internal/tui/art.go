// Art and color helpers for the xkvm TUI. Dependency-free: ANSI basic colors
// (work in every terminal), block characters for the spinner, and the same
// XKVM logo that opens the README.
package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blessed0x/xkvm-ios-injector/internal/app"
)

// Color tokens are semantic keys, not raw codes: the actual escape
// sequences live in a per-UI palette (soft, low-saturation 256-color set on
// capable terminals, truecolor where COLORTERM says so, plain ANSI 16 as
// the floor). anBold/anDim/anInvert/anReset stay literal — they combine
// with any palette entry.
const (
	cRed     = "red"     // errors
	cGreen   = "green"   // success / toggled-on
	cYellow  = "yellow"  // warnings, gentle amber
	cBlue    = "blue"    // info accents
	cMagenta = "magenta" // section titles
	cCyan    = "cyan"    // lists and accepted accents
	cWhite   = "white"   // body text
	anBold   = "\x1b[1m"
	anReset  = "\x1b[0m"
)

// palette256 is the calm 256-color set (dusty, not neon — easy on the
// eyes): rose, sage, sand, steel, lavender, teal, ivory.
var palette256 = map[string]string{
	cRed: "\x1b[38;5;174m", cGreen: "\x1b[38;5;114m", cYellow: "\x1b[38;5;179m",
	cBlue: "\x1b[38;5;110m", cMagenta: "\x1b[38;5;140m", cCyan: "\x1b[38;5;109m",
	cWhite: "\x1b[38;5;253m", anBold: "\x1b[1m", anDim: "\x1b[2m", anInvert: "\x1b[7m",
}

// paletteRGB is the same palette in 24-bit for truecolor terminals — even
// softer and consistent across themes.
var paletteRGB = map[string]string{
	cRed: "\x1b[38;2;208;127;116m", cGreen: "\x1b[38;2;159;191;143m", cYellow: "\x1b[38;2;212;176;106m",
	cBlue: "\x1b[38;2;143;168;200m", cMagenta: "\x1b[38;2;176;154;200m", cCyan: "\x1b[38;2;143;184;176m",
	cWhite: "\x1b[38;2;216;212;204m", anBold: "\x1b[1m", anDim: "\x1b[2m", anInvert: "\x1b[7m",
}

// palette16 is the no-256 floor (same tokens, basic colors).
var palette16 = map[string]string{
	cRed: "\x1b[31m", cGreen: "\x1b[32m", cYellow: "\x1b[33m",
	cBlue: "\x1b[34m", cMagenta: "\x1b[35m", cCyan: "\x1b[36m",
	cWhite: "\x1b[37m", anBold: "\x1b[1m", anDim: "\x1b[2m", anInvert: "\x1b[7m",
}

// buildPalette picks the escape set for this UI.
func buildPalette() map[string]string {
	switch {
	case strings.Contains(strings.ToUpper(os.Getenv("COLORTERM")), "TRUECOLOR") || strings.Contains(strings.ToUpper(os.Getenv("COLORTERM")), "24BIT"):
		return paletteRGB
	case strings.Contains(os.Getenv("TERM"), "256"):
		return palette256
	default:
		return palette16
	}
}

// escapeSeq resolves a token (or "a+b" combination) to its escape codes.
func (u *UI) escapeSeq(code string) string {
	if code == "" || u.palette == nil {
		return ""
	}
	if strings.Contains(code, "+") {
		var out string
		for _, part := range strings.Split(code, "+") {
			out += u.escapeSeq(part)
		}
		return out
	}
	if seq, ok := u.palette[code]; ok {
		return seq
	}
	return code
}

// spinFrames is the block-art spinner: a bar that grows, then shrinks.
var spinFrames = []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█", "▉", "▊", "▋", "▌", "▍", "▎"}

// paint wraps s in code (a palette token or raw escape) when color is on.
func (u *UI) paint(code, s string) string {
	if !u.Color || s == "" {
		return s
	}
	if seq := u.escapeSeq(code); seq != "" {
		return seq + s + anReset
	}
	return s
}

// moon is the intro scene: a crescent moon with a starfield and soft
// gradient shading, drawn with block characters so it works on any
// terminal and reads at any size.
var moon = []string{
	"               ·                          ✧",
	"     ✧                       .",
	"                 ▄▄▄▄▓▓▄▄▄▄",
	"       .       ▄▓▓▓██░░░░░░░▀▄",
	"             ▄▓▓██░░░░░  ✧  ░░▀▄",
	"            ▄▓██░░░░░  ✵      ░░▀▄",
	"  ✧        ▄▓█░░░░░░            ░▐█▄",
	"           ▐█▌░░░░░░            ░░██",
	"            ▀█▄░░░░░          ░░▄█▀",
	"             ▀██▄░░░░░      ░░▄█▀",
	"      .        ▀▀████▓▄▄▄▄▄██▀▀",
	"                 ✧            ✦",
	"        ·                        ✧",
}

// moonColors is the per-line gradient for the moon scene (light rim →
// shaded body) as 256-color codes; "term" rows reuse the palette tokens.
var moonColors = []string{
	"accent", "", "\x1b[38;5;254m", "\x1b[38;5;252m", "\x1b[38;5;250m",
	"\x1b[38;5;248m", "\x1b[38;5;244m", "\x1b[38;5;240m", "\x1b[38;5;238m",
	"\x1b[38;5;236m", "\x1b[38;5;238m", "accent", "accent",
}

// intro shows the animated moon scene with the wordmark, then a short
// control card. Piped runs get a single plain line (CI, scripts).
func (u *UI) intro() {
	for i, ln := range moon {
		out := ln
		star := ""
		if u.Color {
			star = u.escapeSeq(cYellow)
			if star == "" {
				star = u.escapeSeq(cWhite)
			}
			switch moonColors[i%len(moonColors)] {
			case "accent":
				out = u.escapeSeq(cCyan) + out
			case "":
			default:
				out = moonColors[i%len(moonColors)] + out
			}
			out = strings.ReplaceAll(out, "✧", star+"✧"+anReset)
			out = strings.ReplaceAll(out, "✦", u.escapeSeq(cMagenta)+"✦"+anReset)
			out = strings.ReplaceAll(out, "✵", star+"✵"+anReset)
			out = strings.ReplaceAll(out, "·", u.escapeSeq(cWhite)+"·"+anReset)
			out += anReset
		}
		fmt.Fprintln(u.Out, out)
		if u.Animate {
			time.Sleep(28 * time.Millisecond)
		}
	}
	fmt.Fprintln(u.Out, u.paint(anBold+cMagenta, "   x k v m"))
	fmt.Fprintln(u.Out, u.paint(cCyan, "   the friendly way to tweak your iOS apps"))
	fmt.Fprintln(u.Out, u.paint(cWhite, "   v"+app.Version+" · menu for humans, the same flags for scripts and AI"))
	fmt.Fprintln(u.Out)
	if !u.Animate {
		return
	}
	u.growBar("warming up the toolbox", 550*time.Millisecond)
}

// growBar draws a filling █░ bar on one line, then clears it.
func (u *UI) growBar(msg string, total time.Duration) {
	steps := 14
	delay := total / time.Duration(steps)
	for i := 1; i <= steps; i++ {
		bar := strings.Repeat("█", i) + strings.Repeat("░", steps-i)
		fmt.Fprintf(u.Out, "\r%s %s   ", u.paint(cCyan, "["+bar+"]"), msg)
		time.Sleep(delay)
	}
	fmt.Fprintf(u.Out, "\r%s\r", strings.Repeat(" ", 80))
}

// spin runs fn while animating a block spinner with msg. When u.Animate is
// off it prints a plain "msg..." line instead (pipes, tests).
func (u *UI) spin(msg string, fn func() error) error {
	if !u.Animate {
		fmt.Fprintln(u.Out, u.paint(cCyan, msg+"..."))
		return fn()
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	ticker := time.NewTicker(90 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case err := <-done:
			fmt.Fprintf(u.Out, "\r%s\r", strings.Repeat(" ", 80))
			return err
		case <-ticker.C:
			f := spinFrames[i%len(spinFrames)]
			fmt.Fprintf(u.Out, "\r%s %s   ", u.paint(cGreen, f), msg)
			i++
		}
	}
}
