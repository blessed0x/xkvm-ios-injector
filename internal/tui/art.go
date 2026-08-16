// Art and color helpers for the xkvm TUI. Dependency-free: ANSI basic colors
// (work in every terminal), block characters for the spinner, and the same
// XKVM logo that opens the README.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/xscope0/xkvm-ios-injector/internal/app"
)

// ANSI color codes (basic 16-color — safe everywhere).
const (
	anRed     = "\x1b[31m"
	anGreen   = "\x1b[32m"
	anYellow  = "\x1b[33m"
	anBlue    = "\x1b[34m"
	anMagenta = "\x1b[35m"
	anCyan    = "\x1b[36m"
	anWhite   = "\x1b[37m"
	anBold    = "\x1b[1m"
	anReset   = "\x1b[0m"
)

// logo is the xkvm mark (mirrors the README banner). A raw string so every
// backslash survives verbatim.
const logo = ` ___    ___ ___  __    ___      ___ _____ ______
|\  \  /  /|\  \|\  \ |\  \    /  /|\   _ \  _   \
\ \  \/  / | \  \/  /|\ \  \  /  / | \  \\\__\ \  \
 \ \    / / \ \   ___  \ \  \/  / / \ \  \\|__| \  \
  /     \/   \ \  \\ \ \  \    / /   \ \  \    \ \  \
 /  /\   \    \ \__\\ \__\ \__/ /     \ \__\    \ \__\
/__/ /\ __\    \|__| \|__|\|__|/       \|__|     \|__|
|__|/ \|__|`

// logoRainbow is the per-line color cycle for the banner.
var logoRainbow = []string{anRed, anYellow, anGreen, anCyan, anBlue, anMagenta, anWhite, anYellow}

// spinFrames is the block-art spinner: a bar that grows, then shrinks.
var spinFrames = []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█", "▉", "▊", "▋", "▌", "▍", "▎"}

// paint wraps s in code (a color/bold escape) when color is on.
func (u *UI) paint(code, s string) string {
	if !u.Color || code == "" {
		return s
	}
	return code + s + anReset
}

// animateBanner prints the logo with a short color cascade, then a growing
// block bar, so the tool feels alive. The animation only runs when
// u.Animate is set (a real terminal); otherwise the banner prints instantly.
func (u *UI) animateBanner() {
	lines := strings.Split(logo, "\n")
	for i, ln := range lines {
		col := ""
		if u.Color {
			col = logoRainbow[i%len(logoRainbow)]
		}
		out := ln
		if u.Color {
			if col == "" {
				col = anWhite
			}
			out = col + ln + anReset
		}
		fmt.Fprintln(u.Out, out)
		if u.Animate {
			time.Sleep(35 * time.Millisecond)
		}
	}
	fmt.Fprintln(u.Out, u.paint(anBold, "  the friendly way to tweak your iOS apps"))
	fmt.Fprintln(u.Out)
	if u.Animate {
		u.growBar("warming up the toolbox", 550*time.Millisecond)
	}
}

// growBar draws a filling █░ bar on one line, then clears it.
func (u *UI) growBar(msg string, total time.Duration) {
	steps := 14
	delay := total / time.Duration(steps)
	for i := 1; i <= steps; i++ {
		bar := strings.Repeat("█", i) + strings.Repeat("░", steps-i)
		fmt.Fprintf(u.Out, "\r%s %s   ", u.paint(anCyan, "["+bar+"]"), msg)
		time.Sleep(delay)
	}
	fmt.Fprintf(u.Out, "\r%s\r", strings.Repeat(" ", 80))
}

// spin runs fn while animating a block spinner with msg. When u.Animate is
// off it prints a plain "msg..." line instead (pipes, tests).
func (u *UI) spin(msg string, fn func() error) error {
	if !u.Animate {
		fmt.Fprintln(u.Out, u.paint(anCyan, msg+"..."))
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
			fmt.Fprintf(u.Out, "\r%s %s   ", u.paint(anGreen, f), msg)
			i++
		}
	}
}

// intro shows the animated banner plus a short control card. Piped runs get
// a single plain line instead (CI, scripts).
func (u *UI) intro() {
	u.animateBanner()
	fmt.Fprintln(u.Out, u.paint(anBold+anCyan, "  v"+app.Version))
	u.say(anWhite, "  everything works both ways: this menu for humans, the same flags on the command line for scripts and AI.")
	fmt.Fprintln(u.Out)
	if !u.Animate {
		return
	}
	fmt.Fprintln(u.Out, "  "+u.paint(anBold, "how to drive it"))
	fmt.Fprintln(u.Out, "  ↑/↓ or j/k move · 1-9 jumps · enter picks · q backs out · each highlighted option explains itself")
	fmt.Fprintln(u.Out)
	u.growBar("warming up the toolbox", 550*time.Millisecond)
}
