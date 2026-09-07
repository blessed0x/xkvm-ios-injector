package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/blessed0x/xkvm-ios-injector/internal/log"
)

// The tunnel-info endpoint contract is three lines of HTTP: go-ios's
// `tunnel start` serves GET /tunnel/<udid> answering the tunnel's JSON
// coordinates. Reading it directly keeps the gvisor/quic-go dependency out
// of xkvm entirely (upstream's tunnel package imports them; xkvm only ever
// consumes already-running tunnels, it does not terminate QUIC itself).

// publishedTunnel mirrors the JSON the go-ios tunnel-info endpoint answers
// with (ios/tunnel.Tunnel's exported fields).
type publishedTunnel struct {
	Address          string `json:"address"`
	RsdPort          int    `json:"rsdPort"`
	UDID             string `json:"udid"`
	UserspaceTUN     bool   `json:"userspaceTun"`
	UserspaceTUNPort int    `json:"userspaceTunPort"`
}

// The iOS 17+ developer tunnel, end to end.
//
// Stock iOS 17 moved process control, install, screenshot and os_trace
// services behind a CoreDevice tunnel — the same prerequisite pymobiledevice3
// documents for its remote services. go-ios ships a userspace tunnel that
// publishes its coordinates on a local HTTP endpoint (default 127.0.0.1:60105,
// GO_IOS_AGENT_HOST / GO_IOS_AGENT_PORT); any other go-ios-based process can
// then stamp those coordinates onto a DeviceEntry and reach the gated
// services over the tunnel interface.
//
// xkvm uses that contract in two layers:
//
//  1. discover — every gated op resolves its device entry through
//     tunneledEntry, which picks up an already-running tunnel (started by a
//     previous xkvm run, by `go-ios tunnel start`, or by pymobiledevice3's
//     remote tunnel when it serves the same endpoint shape).
//  2. auto-start — when an op still hits the gate and no tunnel is
//     published, tunnelManager spawns the canonical userspace-tunnel command
//     as a session-scoped subprocess, waits for it to publish coordinates,
//     and the op retries once. Tunnels die with the xkvm process; set
//     XKVM_NO_AUTO_TUNNEL=1 to always print the manual instructions instead.

const (
	goIOSModuleRef = "github.com/danielpaulus/go-ios@v1.3.2"
	// tunnelBudget bounds how long ensure waits for a spawned tunnel to
	// publish coordinates. The first ever run pays a `go run` module
	// download; later runs only pay tunnel setup (seconds).
	tunnelBudget = 90 * time.Second
	tunnelPoll   = 1500 * time.Millisecond
)

// errAutoTunnelDisabled reports XKVM_NO_AUTO_TUNNEL without pretending the
// spawn itself failed.
var errAutoTunnelDisabled = errors.New("auto-tunnel disabled via XKVM_NO_AUTO_TUNNEL")

// tunnelProc is one spawned tunnel process. The seam exists so tests can
// stand in for `go run` without touching a real process tree.
type tunnelProc interface {
	// exitErr reports how the process ended if it already exited;
	// ok=false while it is still running.
	exitErr() (error, bool)
	kill()
	// output returns captured stderr (the diagnostic tail).
	output() string
}

// cmdProc is the production tunnelProc wrapping exec.Cmd.
type cmdProc struct {
	cmd    *exec.Cmd
	done   chan error // receives Wait() result once the process exits
	stderr *limitedBuffer
}

func (p *cmdProc) exitErr() (error, bool) {
	select {
	case err := <-p.done:
		return err, true
	default:
		return nil, false
	}
}

func (p *cmdProc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *cmdProc) output() string { return p.stderr.String() }

// limitedBuffer keeps the tail of tunnel stderr for error messages without
// letting a chatty tunnel grow memory unboundedly.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	max   int
	trunc bool
}

func newLimitedBuffer(max int) *limitedBuffer { return &limitedBuffer{max: max} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := len(b.buf) + len(p)
	switch {
	case total <= b.max:
		b.buf = append(b.buf, p...)
	case len(p) >= b.max:
		// The new write alone overflows: keep only its newest tail.
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		b.trunc = true
	default:
		// Slide the window: drop the oldest bytes, keep as much of p as
		// fits. Tails matter more than heads for a crashed tunnel.
		drop := total - b.max
		if drop >= len(b.buf) {
			b.buf = append(b.buf[:0], p[drop-len(b.buf):]...)
		} else {
			rest := b.buf[drop:]
			copy(b.buf, rest)
			copy(b.buf[len(rest):], p)
			b.buf = b.buf[:b.max]
		}
		b.trunc = true
	}
	return len(p), nil // io.Writer contract: everything consumed
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.trunc {
		s = "…" + s
	}
	return s
}

// tunnelManager owns discovery plus at most one auto-started tunnel per
// UDID for this GoIOS session.
type tunnelManager struct {
	budget time.Duration
	poll   time.Duration

	apiHost func() string
	apiPort func() int
	// start spawns the tunnel process for udid; injectable so tests run a
	// stand-in instead of `go run`.
	start func(ctx context.Context, udid string) (tunnelProc, error)
	// disabled reports the kill-switch state; injectable in tests.
	disabled func() bool

	mu      sync.Mutex
	running map[string]tunnelProc
	base    context.Context    // session scope: tunnels die at Close
	cancel  context.CancelFunc // cancels base
}

func newTunnelManager() *tunnelManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &tunnelManager{
		budget:   tunnelBudget,
		poll:     tunnelPoll,
		apiHost:  ios.HttpApiHost,
		apiPort:  ios.HttpApiPort,
		start:    spawnGoIOSTunnel,
		disabled: func() bool { return os.Getenv("XKVM_NO_AUTO_TUNNEL") != "" },
		running:  map[string]tunnelProc{},
		base:     ctx,
		cancel:   cancel,
	}
}

// infoClient bounds discovery probes; a refused localhost connection must
// fail in milliseconds, not hang the operation behind it.
var infoClient = &http.Client{Timeout: 2 * time.Second}

// info returns the published tunnel coordinates for udid, ok=false when no
// tunnel serves this device right now (endpoint down, device absent, or a
// malformed answer — all render as "no tunnel").
func (m *tunnelManager) info(udid string) (publishedTunnel, bool) {
	url := fmt.Sprintf("http://%s/tunnel/%s", net.JoinHostPort(m.apiHost(), strconv.Itoa(m.apiPort())), udid)
	res, err := infoClient.Get(url)
	if err != nil {
		return publishedTunnel{}, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return publishedTunnel{}, false // 404 = no tunnel for this udid
	}
	var t publishedTunnel
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&t); err != nil {
		return publishedTunnel{}, false
	}
	if t.Address == "" || t.RsdPort == 0 {
		return publishedTunnel{}, false
	}
	return t, true
}

// ensure makes sure a tunnel for udid is up and publishing coordinates,
// spawning one when necessary. It never runs the caller's operation — the
// caller re-resolves its entry and retries after nil.
func (m *tunnelManager) ensure(ctx context.Context, udid string) error {
	if _, ok := m.info(udid); ok {
		return nil // someone already runs a tunnel for this device
	}
	if m.disabled() {
		return errAutoTunnelDisabled
	}

	m.mu.Lock()
	proc := m.running[udid]
	if proc == nil {
		var err error
		proc, err = m.start(m.base, udid)
		if err != nil {
			m.mu.Unlock()
			return &Error{Kind: KindConnection, Op: "tunnel", Remediation: manualTunnelRemediation(udid), Err: err}
		}
		m.running[udid] = proc
		log.Infof("starting developer tunnel for %s in the background (first run may download the go-ios module)..", shortUDID(udid))
	}
	m.mu.Unlock()

	deadline := time.Now().Add(m.budget)
	for {
		if _, ok := m.info(udid); ok {
			log.Infof("developer tunnel ready")
			return nil
		}
		if xerr, exited := proc.exitErr(); exited {
			m.forget(udid)
			detail := proc.output()
			msg := "the tunnel process exited early"
			if xerr != nil {
				msg += fmt.Sprintf(": %v", xerr)
			}
			if detail != "" {
				msg += "\n  tunnel output: " + detail
			}
			return &Error{Kind: KindConnection, Op: "tunnel", Remediation: manualTunnelRemediation(udid),
				Err: errors.New(msg)}
		}
		if err := ctx.Err(); err != nil {
			return &Error{Kind: KindTimeout, Op: "tunnel", Err: err}
		}
		if !time.Now().Before(deadline) {
			m.stopLocked(udid)
			return &Error{Kind: KindTimeout, Op: "tunnel",
				Remediation: "the tunnel did not come up in time.\n  " + manualTunnelRemediation(udid),
				Err:         fmt.Errorf("no tunnel coordinates after %s", m.budget)}
		}
		time.Sleep(m.poll)
	}
}

func (m *tunnelManager) forget(udid string) {
	m.mu.Lock()
	delete(m.running, udid)
	m.mu.Unlock()
}

func (m *tunnelManager) stopLocked(udid string) {
	m.mu.Lock()
	if p := m.running[udid]; p != nil {
		p.kill()
		delete(m.running, udid)
	}
	m.mu.Unlock()
}

// Close tears down every tunnel this session spawned.
func (m *tunnelManager) Close() {
	m.cancel()
	m.mu.Lock()
	for udid, p := range m.running {
		p.kill()
		delete(m.running, udid)
	}
	m.mu.Unlock()
}

// spawnGoIOSTunnel runs the canonical go-ios userspace tunnel as our child:
//
//	go run github.com/danielpaulus/go-ios@v1.3.2 tunnel start --userspace --udid <u>
//
// The process lifetime is bound to m.base (session scope), not to the
// triggering operation, so one tunnel serves every later op in the run.
func spawnGoIOSTunnel(ctx context.Context, udid string) (tunnelProc, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("the go toolchain is not on PATH, so xkvm cannot start the tunnel itself; install Go, or install go-ios and run the tunnel command yourself")
	}
	cmd := exec.CommandContext(ctx, "go", "run", goIOSModuleRef, "tunnel", "start", "--userspace", "--udid", udid)
	stderr := newLimitedBuffer(8 << 10)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawning %s: %w", cmd.String(), err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return &cmdProc{cmd: cmd, done: done, stderr: stderr}, nil
}

// withTunnel stamps published tunnel coordinates onto a usbmuxd device
// entry — exactly what go-ios's own CLI does before every gated op
// (main.go deviceWithRsdProvider): RSD handshake against the tunnel
// address, then UserspaceTUN fields pointing at the local port.
func withTunnel(dev ios.DeviceEntry, udid string, t publishedTunnel) (ios.DeviceEntry, error) {
	rsdService, err := ios.NewWithAddrPortDevice(t.Address, t.RsdPort, dev)
	if err != nil {
		return dev, fmt.Errorf("connecting to the tunnel's remote service discovery: %w", err)
	}
	defer rsdService.Close()
	provider, err := rsdService.Handshake()
	if err != nil {
		return dev, fmt.Errorf("remote service discovery handshake failed: %w", err)
	}
	stamped, err := ios.GetDeviceWithAddress(udid, t.Address, provider)
	if err != nil {
		return dev, fmt.Errorf("resolving the device over the tunnel: %w", err)
	}
	stamped.UserspaceTUN = t.UserspaceTUN
	stamped.UserspaceTUNHost = ios.HttpApiHost()
	stamped.UserspaceTUNPort = t.UserspaceTUNPort
	return stamped, nil
}

// shouldAutoTunnel reports whether a device's reported iOS version makes
// the CoreDevice-tunnel path worthwhile. Stock iOS 17+ gates process
// control, install and screenshot behind that tunnel; iOS 16 and older gate
// them behind the Developer Disk Image instead, where a spawned tunnel
// would only burn the readiness budget on coordinates that never publish.
// Unknown or unparseable versions still attempt — a wrong "no" is worse
// than one wasted attempt.
func shouldAutoTunnel(productVersion string) bool {
	v, err := ParseVersion(productVersion)
	if err != nil {
		return true
	}
	return v[0] >= 17
}

// manualTunnelRemediation is the instruction set shown whenever automatic
// tunnel handling is unavailable or declined.
func manualTunnelRemediation(udid string) string {
	return "iOS 17+ gates process-control, install and screenshot services behind a developer tunnel:\n" +
		fmt.Sprintf("  go run %s tunnel start --userspace --udid %s\n", goIOSModuleRef, udid) +
		"in a second terminal, then retry. --userspace needs no admin on macOS, Linux,\n" +
		"or Windows; on iOS 16 and older the same services need the Developer Disk\n" +
		"Image instead (mount once with Xcode). Nothing is wrong with the device."
}

// shortUDID trims the 40-hex wireless UDIDs to something printable.
func shortUDID(udid string) string {
	if len(udid) > 12 {
		return udid[:12] + "…"
	}
	return udid
}

var _ io.Writer = (*limitedBuffer)(nil)
