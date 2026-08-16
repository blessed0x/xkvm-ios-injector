//go:build darwin

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// rawTerm puts the terminal into raw (char-by-char, no echo) mode and
// returns a restore func. Zero-dependency: a direct ioctl on the stdlib
// syscall termios values — the same dance stty raw does. Only used for the
// arrow-key pickers; everything else stays line-buffered.
func rawTerm(f *os.File) (func(), error) {
	fd := f.Fd()
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	raw.Iflag &^= syscall.ICRNL | syscall.IXON
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	restore := func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCSETA), uintptr(unsafe.Pointer(&old)))
	}
	return restore, nil
}
