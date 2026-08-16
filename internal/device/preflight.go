package device

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios"
)

// transportPreflight probes the usbmuxd endpoint go-ios will dial. It is a
// variable so tests can substitute a fake without racing; production uses
// defaultTransportPreflight.
var transportPreflight = defaultTransportPreflight

// defaultTransportPreflight checks scheme://address reachability the same way
// go-ios dials it: a unix socket is probed with stat (the daemon holds it
// open), a tcp endpoint with a short dial.
func defaultTransportPreflight(ctx context.Context, scheme, address string) error {
	switch scheme {
	case "unix":
		if _, err := os.Stat(address); err != nil {
			return err
		}
		return nil
	case "tcp":
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	default:
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, scheme, address)
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}
}

// TransportDiag reports whether the usbmuxd endpoint for the current host
// is reachable, plus the per-OS remediation when it is not.
type TransportDiag struct {
	OS          string // host OS: darwin, linux, or windows
	Address     string // go-ios endpoint, e.g. unix:///var/run/usbmuxd
	Scheme      string // unix or tcp
	Reachable   bool   // the endpoint answered/stat'd within the budget
	Detail      string // probe error, when not reachable
	Remediation string // what to do, per OS
}

// splitTransportAddress parses go-ios' scheme://address form.
func splitTransportAddress(addr string) (scheme, address string) {
	if i := strings.Index(addr, "://"); i >= 0 {
		return addr[:i], addr[i+3:]
	}
	return "", addr
}

// BuildTransportDiag runs a probe against a resolved endpoint so the logic
// is testable without hardware or a specific host OS.
func BuildTransportDiag(osName, addr string, probe func(ctx context.Context, scheme, address string) error, timeout time.Duration) TransportDiag {
	scheme, address := splitTransportAddress(addr)
	diag := TransportDiag{
		OS:          osName,
		Address:     addr,
		Scheme:      scheme,
		Reachable:   true,
		Remediation: transportRemediation(osName),
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := probe(ctx, scheme, address); err != nil {
		diag.Reachable = false
		diag.Detail = err.Error()
	}
	return diag
}

// ProbeTransport probes the endpoint go-ios will actually use, honoring
// USBMUXD_SOCKET_ADDRESS, with a short 2s budget.
func ProbeTransport() TransportDiag {
	return BuildTransportDiag(runtime.GOOS, ios.GetUsbmuxdSocket(), transportPreflight, 2*time.Second)
}

// transportRemediation is the per-OS "make usbmuxd reachable" hint, free of
// the go-ios dependency so it is trivially table-testable.
func transportRemediation(osName string) string {
	switch osName {
	case "darwin":
		return "usbmuxd runs by default on macOS — replug the cable, unlock the device, and tap Trust, then retry"
	case "linux":
		return "start usbmuxd first — Debian/Ubuntu: sudo apt install usbmuxd; Arch: sudo pacman -S usbmuxd; then replug the cable (socket: /var/run/usbmuxd)"
	case "windows":
		return "Windows needs Apple's usbmuxd on 127.0.0.1:27015 — install iTunes or the Apple Devices app (Microsoft Store), trust the device, then retry"
	default:
		return "is usbmuxd running? start it, then retry"
	}
}

// ErrTransport wraps a failing TransportDiag as a KindConnection error so
// `xkvm device doctor` follows the device exit-code contract while still
// carrying the per-OS remediation.
func ErrTransport(d TransportDiag) error {
	detail := d.Detail
	if detail == "" {
		detail = "usbmuxd is not reachable"
	}
	return &Error{Kind: KindConnection, Op: "doctor", Err: errors.New(detail), Remediation: d.Remediation}
}
