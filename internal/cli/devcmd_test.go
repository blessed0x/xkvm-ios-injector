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
	devs      []device.Dev
	info      device.Info
	apps      []device.App
	launched  []string
	omegaRuns int
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
func (s *stubDevHandler) Restart(ctx context.Context, udid string) error { return nil }
func (s *stubDevHandler) OmegaRestore(ctx context.Context, udid string, progress func(float64)) error {
	s.omegaRuns++
	if progress != nil {
		progress(100)
	}
	return nil
}
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

func runOmega(t *testing.T, args []string, in string) (string, *stubDevHandler, error) {
	t.Helper()
	stub := &stubDevHandler{
		devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
		info: device.Info{Name: "Phone", ProductVersion: "18.5"},
	}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })
	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if in != "" {
		cmd.SetIn(strings.NewReader(in))
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), stub, err
}

func TestOmegaHardBlocksUnsupportedIOS(t *testing.T) {
	_, stub, err := runOmega(t, []string{"omega", "--ios", "27.1"}, "")
	if err == nil {
		t.Fatal("iOS 27 must hard-block")
	}
	if device.ExitCode(err) != 66 {
		t.Errorf("hard block should exit 66, got %d (%v)", device.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "hard block") {
		t.Errorf("no hard-block wording: %v", err)
	}
	if stub.omegaRuns != 0 {
		t.Error("restore must never run when blocked")
	}
}

func TestOmegaUntestedWarnsAndNeedsContinue(t *testing.T) {
	out, stub, err := runOmega(t, []string{"omega", "--ios", "24.0"}, "CONTINUE\n")
	if err != nil {
		t.Fatalf("typed CONTINUE should proceed: %v", err)
	}
	if !strings.Contains(out, "caution") {
		t.Errorf("untested warning missing: %q", out)
	}
	if stub.omegaRuns != 1 {
		t.Errorf("restore runs = %d, want 1", stub.omegaRuns)
	}
}

func TestOmegaRefusesWithoutContinue(t *testing.T) {
	out, stub, err := runOmega(t, []string{"omega", "--ios", "18.5"}, "nah\n")
	if err == nil {
		t.Fatal("refusal must abort")
	}
	if stub.omegaRuns != 0 {
		t.Error("restore ran without confirmation")
	}
	_ = out
}

func TestOmegaContinueIsCaseInsensitive(t *testing.T) {
	_, stub, err := runOmega(t, []string{"omega", "--ios", "18.5"}, "continue\n")
	if err != nil {
		t.Fatalf("lowercase continue should pass: %v", err)
	}
	if stub.omegaRuns != 1 {
		t.Error("restore not run")
	}
}

func TestOmegaYesSkipsGate(t *testing.T) {
	_, stub, err := runOmega(t, []string{"omega", "--ios", "18.5", "--yes"}, "")
	if err != nil {
		t.Fatalf("--yes should skip the gate: %v", err)
	}
	if stub.omegaRuns != 1 {
		t.Error("restore not run")
	}
}

func TestOmegaDetectsVersionFromDevice(t *testing.T) {
	_, stub, err := runOmega(t, []string{"omega", "--yes"}, "")
	if err != nil {
		t.Fatalf("device-detected version (18.5) should be supported: %v", err)
	}
	if stub.omegaRuns != 1 {
		t.Error("restore not run")
	}
}

// TestOmegaPolicyBeforeDevice is the regression guard for the ordering bug:
// an explicit --ios must hard-block even when NO device is attached (the
// stub lists zero devices) — policy is pure math and must fire first.
func TestOmegaPolicyBeforeDevice(t *testing.T) {
	stub := &stubDevHandler{} // zero devices
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })
	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"omega", "--ios", "27.1"})
	err := cmd.Execute()
	if err == nil || device.ExitCode(err) != 66 {
		t.Fatalf("device-less hard block: err=%v", err)
	}
	if stub.omegaRuns != 0 {
		t.Error("restore must never run")
	}
}
