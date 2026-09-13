//go:build linux

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// rawTerm: same as the darwin one, with the Linux termios ioctl spellings.
func rawTerm(f *os.File) (func(), error) {
	fd := f.Fd()
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	raw.Iflag &^= syscall.ICRNL | syscall.IXON
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	restore := func() {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&old)))
	}
	return restore, nil
}
