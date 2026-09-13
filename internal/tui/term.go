package tui

import "os"

// isTerminal reports whether w is a character device (a terminal). Used to
// decide whether color and animation are safe. This is the portable check
// without pulling in golang.org/x/term — it matches what most CLIs do on
// Unix; on Windows the mode bits read differently, so color simply stays off.
func isTerminal(w *os.File) bool {
	fi, err := w.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
