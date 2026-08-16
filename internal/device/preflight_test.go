package device

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTransportRemediationPerOS(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"darwin", "usbmuxd runs by default on macOS"},
		{"linux", "sudo apt install usbmuxd"},
		{"linux", "/var/run/usbmuxd"},
		{"windows", "127.0.0.1:27015"},
		{"windows", "Apple Devices"},
		{"plan9", "is usbmuxd running"},
	}
	for _, c := range cases {
		got := transportRemediation(c.goos)
		if !strings.Contains(got, c.want) {
			t.Errorf("transportRemediation(%q) = %q, want substring %q", c.goos, got, c.want)
		}
	}
}

func TestSplitTransportAddress(t *testing.T) {
	for addr, want := range map[string][2]string{
		"unix:///var/run/usbmuxd": {"unix", "/var/run/usbmuxd"},
		"tcp://127.0.0.1:27015":   {"tcp", "127.0.0.1:27015"},
	} {
		scheme, address := splitTransportAddress(addr)
		if scheme != want[0] || address != want[1] {
			t.Errorf("splitTransportAddress(%q) = %q, %q; want %q, %q", addr, scheme, address, want[0], want[1])
		}
	}
}

func TestBuildTransportDiagReachable(t *testing.T) {
	d := BuildTransportDiag("linux", "unix:///tmp/xkvm-usbmuxd",
		func(ctx context.Context, scheme, address string) error { return nil }, time.Second)
	if !d.Reachable {
		t.Errorf("reachable probe reported not reachable: %+v", d)
	}
	if d.Scheme != "unix" || d.Address != "unix:///tmp/xkvm-usbmuxd" {
		t.Errorf("wrong parsed endpoint: %+v", d)
	}
	if !strings.Contains(d.Remediation, "usbmuxd") {
		t.Errorf("missing remediation: %+v", d)
	}
}

func TestBuildTransportDiagUnreachable(t *testing.T) {
	d := BuildTransportDiag("windows", "tcp://127.0.0.1:27015",
		func(ctx context.Context, scheme, address string) error {
			if scheme != "tcp" {
				t.Errorf("probe got scheme %q, want tcp", scheme)
			}
			return errors.New("connection refused")
		}, time.Second)
	if d.Reachable {
		t.Errorf("unreachable probe reported reachable: %+v", d)
	}
	if d.Detail == "" {
		t.Errorf("unreachable probe missing detail: %+v", d)
	}
	if !strings.Contains(d.Remediation, "Apple Devices") {
		t.Errorf("windows remediation not surfaced: %+v", d)
	}
}

func TestTransportDiagUsesGoIosEndpoint(t *testing.T) {
	// The wrapper must resolve through go-ios so USBMUXD_SOCKET_ADDRESS is
	// honored and per-OS defaults stay in one place.
	d := ProbeTransport()
	if d.Scheme != "unix" && d.Scheme != "tcp" {
		t.Errorf("unexpected transport scheme %q (address %q)", d.Scheme, d.Address)
	}
	if want := map[string]string{"darwin": "unix", "linux": "unix", "windows": "tcp"}[runtime.GOOS]; d.Scheme != want {
		t.Errorf("default scheme for %s = %q, want %q", runtime.GOOS, d.Scheme, want)
	}
	if d.OS != runtime.GOOS {
		t.Errorf("diag.OS = %q, want %q", d.OS, runtime.GOOS)
	}
}
