// Package log implements xkvm's console output conventions, mirroring cyan's
// terminal style: [*] info, [?] warning, [!] error, [<] prompt.
package log

import (
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	mu     sync.Mutex
	silent bool

	infoWriter  io.Writer = os.Stdout
	errorWriter io.Writer = os.Stderr
)

// SetSilent suppresses all non-error output.
func SetSilent(v bool) {
	mu.Lock()
	defer mu.Unlock()
	silent = v
}

// SetWriters redirects output (used by tests).
func SetWriters(info, errw io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	infoWriter = info
	errorWriter = errw
}

// Infof prints an informational message as "[*] ...".
func Infof(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !silent {
		fmt.Fprintf(infoWriter, "[*] %s\n", fmt.Sprintf(format, a...))
	}
}

// Warnf prints a warning as "[?] ...".
func Warnf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !silent {
		fmt.Fprintf(infoWriter, "[?] %s\n", fmt.Sprintf(format, a...))
	}
}

// Errorf prints an error to stderr as "[!] ...". Always shown, even when silent.
func Errorf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(errorWriter, "[!] %s\n", fmt.Sprintf(format, a...))
}

// Promptf prints a user prompt as "[<] ...".
func Promptf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !silent {
		fmt.Fprintf(infoWriter, "[<] %s\n", fmt.Sprintf(format, a...))
	}
}

// Rawf writes to the info stream with NO level prefix, as a single line or
// multi-line block. Used for output whose bytes must match an upstream
// format exactly (e.g. the fixed-paths banner, which is byte-identical to
// patch.sh's echo -e output). Still honors SetWriters and SetSilent.
func Rawf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if !silent {
		fmt.Fprintln(infoWriter, fmt.Sprintf(format, a...))
	}
}
