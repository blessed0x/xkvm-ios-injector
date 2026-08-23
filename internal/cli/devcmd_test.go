package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xscope0/xkvm-ios-injector/internal/device"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

type stubDevHandler struct {
	devs         []device.Dev
	info         device.Info
	apps         []device.App
	launched     []string
	omegaRuns    int
	syslogFilter device.LogFilter
	shot         []byte
	events       []device.WatchEvent
	devMode      device.DevMode
	forwardPorts [2]uint16
	clipboardSet string
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
func (s *stubDevHandler) Syslog(ctx context.Context, udid string, w io.Writer, filter device.LogFilter) error {
	s.syslogFilter = filter
	return nil
}
func (s *stubDevHandler) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	return s.shot, nil
}
func (s *stubDevHandler) Watch(ctx context.Context, fn func(device.WatchEvent) error) error {
	for _, ev := range s.events {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (s *stubDevHandler) DevModeStatus(ctx context.Context, udid string) (device.DevMode, error) {
	return s.devMode, nil
}
func (s *stubDevHandler) Forward(ctx context.Context, udid string, hostPort, phonePort uint16) (io.Closer, error) {
	s.forwardPorts = [2]uint16{hostPort, phonePort}
	// Production returns the live listener immediately; the CLI owns the
	// wait-on-ctx side of the contract.
	return nopCloser{}, nil
}
func (s *stubDevHandler) PasteboardGet(ctx context.Context, udid string) (string, bool, error) {
	return "clipboard text", true, nil
}
func (s *stubDevHandler) PasteboardSet(ctx context.Context, udid, text string) error {
	s.clipboardSet = text
	return nil
}

type nopCloser struct{}

func (nopCloser) Close() error                                           { return nil }
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
	return runDeviceCmdStub(t, nil, sub, args...)
}

// runDeviceCmdStub runs one device subcommand against the given handler
// (a default stub when nil) and returns its output.
func runDeviceCmdStub(t *testing.T, stub *stubDevHandler, sub string, args ...string) (string, error) {
	t.Helper()
	if stub == nil {
		stub = &stubDevHandler{
			devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
			info: device.Info{Name: "Phone", ProductVersion: "18.1", BuildVersion: "22B83"},
			apps: []device.App{{BundleID: "com.x.app", Name: "Xapp", Path: "/var/x"}},
		}
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
	out, stub, err := runOmega(t, []string{"omega", "--ios", "28.1"}, "CONTINUE\n")
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

func TestDeviceDoctorUnreachable(t *testing.T) {
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "tcp://127.0.0.1:1") // nothing listens here
	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"doctor"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("doctor succeeded against an unreachable transport")
	}
	if device.ExitCode(err) != 69 {
		t.Errorf("doctor exit code = %d, want 69", device.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "127.0.0.1:27015") && !strings.Contains(err.Error(), "usbmuxd") {
		t.Errorf("doctor error missing transport hint: %v", err)
	}
}

func TestDeviceDoctorReachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket fixtures are not meaningful on windows")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "usbmuxd")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Setenv("USBMUXD_SOCKET_ADDRESS", "unix://"+sock)

	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"doctor"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "transport ready") {
		t.Errorf("doctor success output missing ready line: %q", out.String())
	}
}

func TestDeviceSyslogPassesFilter(t *testing.T) {
	stub := &stubDevHandler{devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}}}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	cmd := newDeviceCmd()
	cmd.SetArgs([]string{"syslog", "--process", "Spring", "--contains", "crash"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stub.syslogFilter.Process != "Spring" || stub.syslogFilter.Contains != "crash" {
		t.Fatalf("filter flags must reach the handler, got %+v", stub.syslogFilter)
	}
}

func TestDeviceScreenshotWritesPNG(t *testing.T) {
	dir := t.TempDir()
	stub := &stubDevHandler{
		devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
		shot: append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 16)...),
	}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	out := filepath.Join(dir, "shot.png")
	if _, err := runDeviceCmdStub(t, stub, "screenshot", out); err != nil {
		t.Fatal(err)
	}
	data, rerr := os.ReadFile(out)
	if rerr != nil || len(data) != len(stub.shot) {
		t.Fatalf("screenshot file missing or truncated: %v", rerr)
	}

	// No path argument: a timestamped default appears in the CWD.
	before := time.Now().Format("20060102")
	cwd, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(cwd) })
	path, err := runDeviceCmdCapturePath(t, stub, "screenshot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(path), "screenshot-"+before+"-") || !strings.HasSuffix(path, ".png") {
		t.Fatalf("default screenshot name off: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default-path screenshot not written: %v", err)
	}
}

// runDeviceCmdCapturePath runs the command and returns the "saved <path>"
// log target from the output.
func runDeviceCmdCapturePath(t *testing.T, stub *stubDevHandler, sub string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	log.SetWriters(&out, &out)
	t.Cleanup(func() { log.SetWriters(os.Stderr, os.Stderr) })
	cmd.SetArgs(append([]string{sub}, args...))
	if err := cmd.Execute(); err != nil {
		return "", err
	}
	line := out.String()
	i := strings.Index(line, "saved ")
	if i < 0 {
		return "", errors.New("no saved line in output: " + line)
	}
	return strings.TrimSpace(line[i+len("saved "):]), nil
}

func TestDeviceWatchStreamsEvents(t *testing.T) {
	stub := &stubDevHandler{
		devs:   []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
		events: []device.WatchEvent{{UDID: "BBBB", Attached: true}, {UDID: "AAAA", Attached: false}},
	}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	out, err := runDeviceCmdStub(t, stub, "watch")
	if err != nil && !strings.Contains(err.Error(), "context") {
		t.Fatal(err)
	}
	if !strings.Contains(out, "attached BBBB") || !strings.Contains(out, "detached AAAA") {
		t.Fatalf("watch events missing from output: %q", out)
	}
}

func TestDeviceDevModeStatesRender(t *testing.T) {
	cases := []struct {
		name string
		dm   device.DevMode
		want string
	}{
		{"on", device.DevMode{Reported: true, Enabled: true}, "developer mode: ON"},
		{"off", device.DevMode{Reported: true}, "developer mode: OFF"},
		{"pre16", device.DevMode{}, "not gated on this iOS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubDevHandler{
				devs:    []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}},
				devMode: tc.dm,
			}
			orig := deviceHandler
			deviceHandler = func(o device.Options) device.Handler { return stub }
			t.Cleanup(func() { deviceHandler = orig })
			out, err := runDeviceCmdStub(t, stub, "devmode")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want %q in %q", tc.want, out)
			}
		})
	}
}

func TestDeviceForwardParsesPortsAndBlocksUntilCancel(t *testing.T) {
	stub := &stubDevHandler{devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}}}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	cmd := newDeviceCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"forward", "8080", "3999"})
	if err := cmd.Execute(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if stub.forwardPorts != [2]uint16{8080, 3999} {
		t.Fatalf("ports must reach the handler in order, got %v", stub.forwardPorts)
	}
	if !strings.Contains(out.String(), "forwarding localhost:8080 -> AAAA:3999") {
		t.Fatalf("forward banner missing: %q", out.String())
	}
}

func TestDeviceForwardRejectsBadPorts(t *testing.T) {
	for _, args := range [][]string{{"forward", "0", "80"}, {"forward", "99999", "80"}, {"forward", "abc", "80"}} {
		out, err := runDeviceCmd(t, args[0], args[1:]...)
		if err == nil || !strings.Contains(err.Error(), "not a TCP port") {
			t.Fatalf("args %v must be rejected, got out=%q err=%v", args, out, err)
		}
	}
}

func TestDevicePasteboardGetSet(t *testing.T) {
	stub := &stubDevHandler{devs: []device.Dev{{UDID: "AAAA", Transport: device.TransportUSB}}}
	orig := deviceHandler
	deviceHandler = func(o device.Options) device.Handler { return stub }
	t.Cleanup(func() { deviceHandler = orig })

	out, err := runDeviceCmdStub(t, stub, "pasteboard", "get")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "clipboard text" {
		t.Fatalf("get output mismatch: %q", out)
	}

	if _, err := runDeviceCmdStub(t, stub, "pasteboard", "set", "hello world"); err != nil {
		t.Fatal(err)
	}
	if stub.clipboardSet != "hello world" {
		t.Fatalf("set text must reach the handler, got %q", stub.clipboardSet)
	}

	if _, err := runDeviceCmdStub(t, stub, "pasteboard", "bogus"); err == nil {
		t.Fatal("unknown pasteboard verb must fail")
	}
}
