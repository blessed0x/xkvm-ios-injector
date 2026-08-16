package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/xscope0/xkvm-ios-injector/internal/device"
)

type stubDevHandler struct {
	devs     []device.Dev
	info     device.Info
	apps     []device.App
	launched []string
}

func (s *stubDevHandler) Close() error                                   { return nil }
func (s *stubDevHandler) List(ctx context.Context) ([]device.Dev, error) { return s.devs, nil }
func (s *stubDevHandler) Pair(ctx context.Context, udid string, sup *device.Supervised) error {
	return nil
}
func (s *stubDevHandler) Info(ctx context.Context, udid string) (device.Info, error) {
	return s.info, nil
}
func (s *stubDevHandler) Battery(ctx context.Context, udid string) (device.Battery, error) {
	return device.Battery{CurrentCapacity: 93, IsCharging: true}, nil
}
func (s *stubDevHandler) Apps(ctx context.Context, udid string, system bool) ([]device.App, error) {
	return s.apps, nil
}
func (s *stubDevHandler) Install(ctx context.Context, udid, path string) error { return nil }
func (s *stubDevHandler) Uninstall(ctx context.Context, udid, bundleID string) error {
	return nil
}
func (s *stubDevHandler) Launch(ctx context.Context, udid, bundleID string, env map[string]string, args []string) (uint64, error) {
	s.launched = append(s.launched, bundleID)
	return 4242, nil
}
func (s *stubDevHandler) Kill(ctx context.Context, udid string, pid uint64) error { return nil }
func (s *stubDevHandler) Syslog(ctx context.Context, udid string, w io.Writer) error {
	return nil
}
func (s *stubDevHandler) Restart(ctx context.Context, udid string) error  { return nil }
func (s *stubDevHandler) Shutdown(ctx context.Context, udid string) error { return nil }

func runDeviceCmd(t *testing.T, sub string, args ...string) (string, error) {
	t.Helper()
	stub := &stubDevHandler{
		devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
		info: device.Info{Name: "Phone", ProductVersion: "18.1", BuildVersion: "22B83"},
		apps: []device.App{{BundleID: "com.x.app", Name: "Xapp", Path: "/var/x"}},
	}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{sub}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestDeviceListShowsUDIDAndTransport(t *testing.T) {
	out, err := runDeviceCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "AAAA") || !strings.Contains(out, "USB") {
		t.Errorf("list output missing device info: %q", out)
	}
}

func TestDeviceInfoJSON(t *testing.T) {
	out, err := runDeviceCmd(t, "info", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got device.Info
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("info --json is not valid JSON: %v\n%s", jerr, out)
	}
	if got.Name != "Phone" || got.ProductVersion != "18.1" {
		t.Errorf("wrong info: %+v", got)
	}
}

func TestDeviceAppsJSONIsAnArray(t *testing.T) {
	out, err := runDeviceCmd(t, "apps", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []device.App
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("apps --json is not valid JSON: %v\n%s", jerr, out)
	}
	if len(got) != 1 || got[0].BundleID != "com.x.app" {
		t.Errorf("wrong apps: %+v", got)
	}
}

func TestDeviceLaunchForwardsBundleAndPrintsPID(t *testing.T) {
	stub := &stubDevHandler{devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}}}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"launch", "com.x.app", "--env", "DYLD_TEST=1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(stub.launched) != 1 || stub.launched[0] != "com.x.app" {
		t.Errorf("launch not forwarded: %v", stub.launched)
	}
	if !strings.Contains(out.String(), "pid 4242") {
		t.Errorf("pid missing from output: %q", out.String())
	}
}

func TestDeviceInstallRequiresAPath(t *testing.T) {
	out, err := runDeviceCmd(t, "install")
	if err == nil {
		t.Fatal("install with no path must fail")
	}
	if out == "" {
		t.Error("usage error should print something")
	}
}

func TestParseEnvRejectsMalformed(t *testing.T) {
	if _, err := parseEnv([]string{"GOOD=1"}); err != nil {
		t.Errorf("valid entry rejected: %v", err)
	}
	if _, err := parseEnv([]string{"NOPE"}); err == nil {
		t.Error("missing '=' accepted")
	}
	if _, err := parseEnv([]string{"A=1", "A=2"}); err == nil {
		t.Error("duplicate key accepted")
	}
}

func TestDeviceOptionsTimeoutsFlow(t *testing.T) {
	// The factory must receive the --timeout flag into Options.
	captured := device.Options{}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler {
		captured = o
		return &stubDevHandler{devs: []device.Dev{{UDID: "AAAA"}}}
	}
	t.Cleanup(func() { deviceHandler = orig })

	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"info", "--timeout", "3s"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Timeout != 3*time.Second {
		t.Errorf("--timeout not forwarded: %v", captured.Timeout)
	}
}
