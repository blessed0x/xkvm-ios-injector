//go:build windows

package tui

import "os"

// rawTerm on Windows is a no-op: the console stays line-buffered, so the
// pickers fall back to their number/name line protocol (arrows need a VT
// input stream, which the stdlib syscall set doesn't expose without extra
// kernel32 incantations; typed numbers cover the same ground).
func rawTerm(f *os.File) (func(), error) {
	return func() {}, nil
}
