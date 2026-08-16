package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// anDim / anInvert extend the base palette for the picker panels.
const (
	anDim    = "\x1b[2m"
	anInvert = "\x1b[7m"
)

// Choice is one selectable option. Desc (why you'd pick it) and Ex (a short
// real example) are shown in a footer panel while the option is
// highlighted, so new users learn what each switch does before they hit it.
type Choice struct {
	Name string // short label
	Desc string // 1-2 lines: what this does and why you'd want it
	Ex   string // short example ("" = none)
	Def  bool   // multi-select: pre-toggled on, matching the CLI's default
}

// navigate reads one keypress from the raw terminal: arrow keys decode as
// 'A'/'B' (up/down), everything else passes through as the raw byte. The
// second return reports whether the key was an escape sequence.
func navigate(f *os.File) (byte, bool) {
	var b [3]byte
	if n, err := f.Read(b[:1]); err != nil || n == 0 {
		return 0, false
	}
	if b[0] == 27 { // ESC or arrow sequence
		if n, err := f.Read(b[1:3]); err == nil && n == 2 && b[1] == '[' {
			return b[2], true
		}
		return 27, true
	}
	return b[0], false
}

// matchChoice resolves a typed answer to a choice index: exact number,
// exact name (case-insensitive), or an unambiguous name prefix. The second
// return is false when nothing matched.
func matchChoice(choices []Choice, line string) (int, bool) {
	line = strings.ToLower(strings.TrimSpace(line))
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(choices) {
		return n - 1, true
	}
	hit, count := -1, 0
	for i, c := range choices {
		name := strings.ToLower(strings.TrimSpace(c.Name))
		if name == line || (len(line) >= 2 && strings.HasPrefix(name, line)) {
			hit, count = i, count+1
		}
	}
	return hit, count == 1
}

// wrap breaks s into lines of at most width runes (ASCII-oriented; the
// panels stay tidy even when a desc exceeds the box).
func wrap(s string, width int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, word := range strings.Fields(s) {
		if len(out) == 0 {
			out = append(out, word)
			continue
		}
		last := out[len(out)-1]
		if len(last)+1+len(word) <= width {
			out[len(out)-1] = last + " " + word
		} else {
			out = append(out, word)
		}
	}
	return out
}

// choose asks one question. multi=false: returns the picked index (nil on
// cancel). multi=true: toggles and returns every selected index, starting
// from the Def-toggled set.
//
// Interactive terminals get the arrow-key picker (↑/↓ move, space toggles in
// multi mode, 1-9 jump, enter confirms, q cancels, a footer panel explains
// the highlighted option). Piped stdin (tests, CI) gets the line protocol:
//
//	single:  "<number>" | "<name>"           → pick; blank/q → cancel
//	multi:   "3 7 !2" on one line            → toggle on/off; blank → keep
//	         the Def toggles; q → cancel
func (u *UI) choose(question string, choices []Choice, multi bool) []int {
	if u.InTerm && u.Animate {
		return u.chooseRaw(question, choices, multi)
	}
	return u.chooseLine(question, choices, multi)
}

// chooseLine is the piped/scripted path and also the tests' protocol.
func (u *UI) chooseLine(question string, choices []Choice, multi bool) []int {
	fmt.Fprintln(u.Out)
	u.title(question)
	for i, c := range choices {
		mark := " "
		if multi && c.Def {
			mark = "x"
		}
		fmt.Fprintf(u.Out, "  %s%d. %s  [%s]\n", u.paint(cCyan, ""), i+1, c.Name, mark)
		if c.Desc != "" {
			fmt.Fprintf(u.Out, "       %s\n", u.paint(anDim, c.Desc))
		}
	}
	if multi {
		return u.chooseLineMulti(choices)
	}
	for {
		line := u.readLine("pick one (number or name), leave empty or q to go back: ")
		if line == "" || isCancel(line) {
			return nil
		}
		if i, ok := matchChoice(choices, line); ok {
			return []int{i}
		}
		u.say(cYellow, "  try a number from the list!")
	}
}

// chooseLineMulti reads one line of space-separated tokens. "N" toggles a
// choice on, "!N" toggles it off, a blank line returns the Def toggles.
func (u *UI) chooseLineMulti(choices []Choice) []int {
	sel := map[int]bool{}
	for i, c := range choices {
		if c.Def {
			sel[i] = true
		}
	}
	line := u.readLine("pick all you want (space-separated numbers, !N to un-pick, empty = finish, q = cancel): ")
	if line == "" {
		return u.sortedSel(sel)
	}
	if isCancel(line) {
		return nil
	}
	for _, tok := range strings.Fields(line) {
		off := strings.HasPrefix(tok, "!")
		tok = strings.TrimPrefix(tok, "!")
		if i, ok := matchChoice(choices, tok); ok {
			if off {
				delete(sel, i)
			} else {
				sel[i] = true
			}
		}
	}
	return u.sortedSel(sel)
}

func (u *UI) sortedSel(sel map[int]bool) []int {
	// Always non-nil: callers treat nil as "user cancelled", and an empty
	// selection (nothing toggled) is a valid "take the defaults" answer.
	out := make([]int, 0, len(sel))
	for i := range sel {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// panelText renders the explainer box for the highlighted choice: wrapped
// description + example line, padded so every option's panel has the same
// height (the raw redraw math relies on it).
func panelText(c Choice, width int) []string {
	lines := append([]string{}, wrap(c.Desc, width-4)...)
	if c.Ex != "" {
		lines = append(lines, "")
		lines = append(lines, wrap("ex: "+c.Ex, width-4)...)
	}
	return lines
}

// chooseRaw is the interactive arrow-key picker. It renders the list once,
// then redraws in place on every keypress (fix-height panel keeps the
// cursor math exact), and restores the terminal when done.
func (u *UI) chooseRaw(question string, choices []Choice, multi bool) []int {
	restore, err := rawTerm(os.Stdin)
	if err != nil {
		// Fall back to the line protocol if the terminal can't go raw.
		return u.chooseLine(question, choices, multi)
	}
	defer restore()

	cursor := 0
	for i, c := range choices {
		if c.Def {
			cursor = i
			break
		}
	}
	sel := map[int]bool{}
	for i, c := range choices {
		if multi && c.Def {
			sel[i] = true
		}
	}

	panelW := 58
	maxH := 1
	for _, c := range choices {
		if h := len(panelText(c, panelW)); h > maxH {
			maxH = h
		}
	}
	// Layout: blank + title + N options + panel (top, title row, maxH text
	// rows, bottom) + legend.
	lines := 1 + 1 + len(choices) + (maxH + 3) + 1

	render := func() {
		var b strings.Builder
		b.WriteString("\n")
		b.WriteString(u.titleText(question) + "\n")
		for i, c := range choices {
			arrow := "  "
			if i == cursor {
				arrow = "▸ "
			}
			num := u.paint(cCyan, fmt.Sprintf("%2d. ", i+1))
			if multi {
				mark := u.paint(anDim, "[○]")
				if sel[i] {
					mark = u.paint(cGreen, "[●]")
				}
				if i == cursor {
					b.WriteString(arrow + num + u.paint(anBold, c.Name) + "  " + mark + "\n")
				} else {
					b.WriteString(arrow + num + c.Name + "  " + mark + "\n")
				}
				continue
			}
			if i == cursor {
				b.WriteString(arrow + num + u.paint(anBold+anInvert, c.Name) + "\n")
			} else {
				b.WriteString(arrow + num + c.Name + "\n")
			}
		}
		b.WriteString(u.panelPad(choices[cursor], panelW, maxH))
		b.WriteString(legend(multi))
		fmt.Fprint(u.Out, b.String())
	}

	render()
	for {
		k, _ := navigate(os.Stdin)
		switch k {
		case 'A', 'k': // up
			if cursor > 0 {
				cursor--
			}
		case 'B', 'j': // down
			if cursor < len(choices)-1 {
				cursor++
			}
		case ' ': // toggle (multi only)
			if multi {
				if sel[cursor] {
					delete(sel, cursor)
				} else {
					sel[cursor] = true
				}
			}
		case 'C': // right = down (some keypads)
			if cursor < len(choices)-1 {
				cursor++
			}
		case 'D': // left = up
			if cursor > 0 {
				cursor--
			}
		case 'q', 'Q', 27: // cancel
			fmt.Fprintln(u.Out)
			return nil
		case '\r', '\n': // confirm
			if multi {
				return u.sortedSel(sel)
			}
			return []int{cursor}
		default:
			if k >= '1' && k <= '9' {
				if n := int(k - '0'); n <= len(choices) {
					cursor = n - 1
				}
			}
		}
		fmt.Fprintf(u.Out, "\x1b[%dA", lines)
		render()
	}
}

// legend is the single footer line explaining the controls.
func legend(multi bool) string {
	if multi {
		return "  ↑/↓ move · space toggle · 1-9 jump · enter ok · q back\n"
	}
	return "  ↑/↓ move · 1-9 jump · enter ok · q back\n"
}

// panelPad renders the fixed-height explainer box for choice c. Every row
// is exactly the same width (body rows: "   │ " + text padded to the inner
// width + " │"), so the box borders align no matter how a description wraps.
func (u *UI) panelPad(c Choice, width, wantLines int) string {
	top := "   ┌" + strings.Repeat("─", width+2) + "┐\n"
	body := panelRow(u, anBold+cCyan, "why this one", width)
	rows := panelText(c, width)
	for i := 0; i < wantLines; i++ {
		txt := ""
		if i < len(rows) {
			txt = rows[i]
		}
		body += panelRow(u, cWhite, txt, width)
	}
	bot := "   └" + strings.Repeat("─", width+2) + "┘\n"
	return top + body + bot
}

// panelRow is one aligned panel line: fixed prefix, padded text, border.
func panelRow(u *UI, color, txt string, width int) string {
	if len(txt) > width {
		txt = txt[:width]
	}
	return "   │ " + u.paint(color, txt) + strings.Repeat(" ", width-len(txt)) + " │\n"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// titleText is title() without the trailing newline handling differences —
// used by the raw renderer so line counting stays exact.
func (u *UI) titleText(t string) string {
	return u.paint(anBold+cMagenta, "── "+t+" "+strings.Repeat("─", max(0, 56-len(t))))
}
