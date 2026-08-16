package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/device"
)

type stubDev struct {
	pairCalls   int
	pairErr     error
	info        device.Info
	infoErr     error
	battery     device.Battery
	apps        []device.App
	appsErr     error
	installs    []string
	launchCalls []string
	launchPID   uint64
	launchErr   error
	kills       []uint64
	syslogLines string
}

func (s *stubDev) Close() error { return nil }
func (s *stubDev) List(ctx context.Context) ([]device.Dev, error) {
	return []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}}, nil
}
func (s *stubDev) Pair(ctx context.Context, udid string, sup *device.Supervised) error {
	s.pairCalls++
	return s.pairErr
}
func (s *stubDev) Info(ctx context.Context, udid string) (device.Info, error) {
	return s.info, s.infoErr
}
func (s *stubDev) Battery(ctx context.Context, udid string) (device.Battery, error) {
	return s.battery, nil
}
func (s *stubDev) Apps(ctx context.Context, udid string, system bool) ([]device.App, error) {
	return s.apps, s.appsErr
}
func (s *stubDev) Install(ctx context.Context, udid, path string) error {
	s.installs = append(s.installs, path)
	return nil
}
func (s *stubDev) Uninstall(ctx context.Context, udid, bundleID string) error { return nil }
func (s *stubDev) Launch(ctx context.Context, udid, bundleID string, env map[string]string, args []string) (uint64, error) {
	s.launchCalls = append(s.launchCalls, bundleID)
	return s.launchPID, s.launchErr
}
func (s *stubDev) Kill(ctx context.Context, udid string, pid uint64) error {
	s.kills = append(s.kills, pid)
	return nil
}
func (s *stubDev) Syslog(ctx context.Context, udid string, w io.Writer) error {
	io.WriteString(w, s.syslogLines)
	<-ctx.Done()
	return nil
}
func (s *stubDev) Restart(ctx context.Context, udid string) error  { return nil }
func (s *stubDev) Shutdown(ctx context.Context, udid string) error { return nil }

func devUI(t *testing.T, s *stubDev, input string) string {
	t.Helper()
	u := NewForTest(strings.NewReader(input), &bytes.Buffer{})
	u.Device = s
	u.Start()
	return u.Out.(*bytes.Buffer).String()
}

// The device category is 4th: apps, tweaks, convert, device, info.
// Its features in order: 1 pair, 2 info, 3 battery, 4 apps, 5 install,
// 6 launch, 7 kill, 8 syslog.
func TestTUIDevCategoryAppears(t *testing.T) {
	out := devUI(t, &stubDev{}, "4\nq\n")
	for _, want := range []string{"device", "pair", "syslog", "stream a built .ipa", "talk to a plugged-in iPhone"} {
		if !strings.Contains(out, want) {
			t.Errorf("device category text missing %q", want)
		}
	}
}

func TestTUIDevPairSuccess(t *testing.T) {
	s := &stubDev{info: device.Info{Name: "Phone", ProductVersion: "26.1"}}
	out := devUI(t, s, "4\n1\nq\n")
	if !strings.Contains(out, "paired AAAA") {
		t.Errorf("pair success not shown:\n%s", out)
	}
	if s.pairCalls != 1 {
		t.Errorf("pair called %d times, want 1", s.pairCalls)
	}
}

func TestTUIDevInfoCard(t *testing.T) {
	s := &stubDev{info: device.Info{Name: "Phone", ProductVersion: "26.1", BuildVersion: "23B85"}}
	out := devUI(t, s, "4\n2\nq\n")
	for _, want := range []string{"device   Phone", "ios      26.1 (23B85)"} {
		if !strings.Contains(out, want) {
			t.Errorf("info card missing %q:\n%s", want, out)
		}
	}
}

func TestTUIDevAppsLaunchPick(t *testing.T) {
	s := &stubDev{
		apps:      []device.App{{BundleID: "com.a.one", Name: "One", Path: "/var/one"}, {BundleID: "com.b.two", Name: "Two", Path: "/var/two"}},
		launchPID: 99,
	}
	// category 4, feature apps(4), pick app 1st, action launch(1)
	out := devUI(t, s, "4\n4\n1\n1\nq\n")
	if !strings.Contains(out, "pid 99") {
		t.Errorf("launch pid not shown:\n%s", out)
	}
	if len(s.launchCalls) != 1 || s.launchCalls[0] != "com.a.one" {
		t.Errorf("wrong launch: %v", s.launchCalls)
	}
}

func TestTUIDevInstallForwardsPath(t *testing.T) {
	s := &stubDev{}
	out := devUI(t, s, "4\n5\n/tmp/App.ipa\nq\n")
	if !strings.Contains(out, "installed") {
		t.Errorf("install success not shown:\n%s", out)
	}
	if len(s.installs) != 1 || s.installs[0] != "/tmp/App.ipa" {
		t.Errorf("install not forwarded: %v", s.installs)
	}
}

func TestTUIDevKillRejectsNonNumeric(t *testing.T) {
	out := devUI(t, &stubDev{}, "4\n7\nabc\nq\n")
	if !strings.Contains(out, "abc is not a pid") {
		t.Errorf("non-numeric pid not rejected:\n%s", out)
	}
}

func TestTUIDevSyslogStreamsAndStopsOnEnter(t *testing.T) {
	s := &stubDev{syslogLines: "Dec 31 12:00:00 Phone SpringBoard[1] <Notice>: boot\n"}
	out := devUI(t, s, "4\n8\n\nq\n")
	for _, want := range []string{"boot", "log stream stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("syslog flow missing %q:\n%s", want, out)
		}
	}
}

func TestTUIDevNoHandlerShowsCLIHint(t *testing.T) {
	u := NewForTest(strings.NewReader("4\n1\nq\n"), &bytes.Buffer{})
	u.Device = nil
	u.Start()
	out := u.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "isn't wired into this build") || !strings.Contains(out, "xkvm device") {
		t.Errorf("CLI hint missing:\n%s", out)
	}
}

func TestTUIDevPairSurfacesTrustError(t *testing.T) {
	s := &stubDev{pairErr: &device.Error{Kind: device.KindPermission, Op: "pair",
		Err:         io.EOF,
		Remediation: "tap \"Trust\" on the device screen, then pair again"}}
	out := devUI(t, s, "4\n1\nq\n")
	if !strings.Contains(out, "Trust") {
		t.Errorf("pair remediation missing:\n%s", out)
	}
}
