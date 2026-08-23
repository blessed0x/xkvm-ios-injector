package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/diagnostics"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/ostrace"
	"github.com/danielpaulus/go-ios/ios/syslog"
	"github.com/danielpaulus/go-ios/ios/zipconduit"

	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// GoIOS adapts go-ios v1.3.x to the Handler contract.
type GoIOS struct {
	timeout time.Duration
	tunnels *tunnelManager
}

// New builds the production handler. Options.Timeout <= 0 means 15s. The
// handler owns at most one auto-started developer tunnel per device for the
// lifetime of the process; Close tears them down.
func New(opts Options) *GoIOS {
	t := opts.Timeout
	if t <= 0 {
		t = 15 * time.Second
	}
	return &GoIOS{timeout: t, tunnels: newTunnelManager()}
}

// Close releases session-scoped state — the auto-started developer tunnels.
// go-ios service connections are per-operation and need no release.
func (g *GoIOS) Close() error {
	if g.tunnels != nil {
		g.tunnels.Close()
	}
	return nil
}

// tunneledEntry resolves a UDID like entry does, then stamps published
// developer-tunnel coordinates onto the result when one is running. Gated
// services route through the tunnel automatically from that point
// (go-ios checks SupportsRsd in every service constructor).
func (g *GoIOS) tunneledEntry(ctx context.Context, udid string) (ios.DeviceEntry, error) {
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return dev, err
	}
	if g.tunnels == nil {
		return dev, nil
	}
	if t, ok := g.tunnels.info(udid); ok {
		stamped, serr := withTunnel(dev, udid, t)
		if serr != nil {
			// A half-dead tunnel must not break ungated work: fall back to
			// the plain entry and let the gate classification handle it.
			log.Warnf("ignoring unusable tunnel coordinates: %v", serr)
			return dev, nil
		}
		return stamped, nil
	}
	return dev, nil
}

// entry resolves a UDID against the live device list and returns the
// go-ios entry. Listing on every operation keeps us honest about devices
// that disappear between commands.
func (g *GoIOS) entry(ctx context.Context, udid string) (ios.DeviceEntry, error) {
	if err := ctx.Err(); err != nil {
		return ios.DeviceEntry{}, err
	}
	list, err := ios.ListDevices()
	if err != nil {
		return ios.DeviceEntry{}, g.wrap(KindConnection, "list devices", transportRemediation(runtime.GOOS), err)
	}
	var fallback *ios.DeviceEntry
	for i := range list.DeviceList {
		d := &list.DeviceList[i]
		if d.Properties.SerialNumber != udid {
			continue
		}
		if fallback == nil {
			fallback = d
		}
		if d.Properties.ConnectionType == "USB" {
			return *d, nil
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return ios.DeviceEntry{}, &Error{Kind: KindNotFound, Op: "list devices", Err: fmt.Errorf("no device with UDID %q is connected", udid),
		Remediation: "run \"xkvm device list\" to see what is attached"}
}

func (g *GoIOS) List(ctx context.Context) ([]Dev, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	list, err := ios.ListDevices()
	if err != nil {
		return nil, g.wrap(KindConnection, "list devices", transportRemediation(runtime.GOOS), err)
	}
	out := make([]Dev, 0, len(list.DeviceList))
	for _, d := range list.DeviceList {
		out = append(out, Dev{UDID: d.Properties.SerialNumber, Transport: Transport(d.Properties.ConnectionType), Transports: []Transport{Transport(d.Properties.ConnectionType)}})
	}
	return out, nil
}

func (g *GoIOS) Pair(ctx context.Context, udid string, sup *Supervised) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		if sup != nil && len(sup.P12) > 0 {
			done <- ios.PairSupervised(dev, sup.P12, sup.Password)
			return
		}
		done <- ios.Pair(dev)
	}()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		return g.wrapPairErr(err)
	case <-ctx.Done():
		return &Error{Kind: KindTimeout, Op: "pair", Err: ctx.Err(), Remediation: "the device did not answer in time; unlock it and retry"}
	}
}

// wrapPairErr maps the pairing-specific upstream errors to KindPermission.
func (g *GoIOS) wrapPairErr(err error) error {
	if errors.Is(err, ios.ErrDeviceLockedPairingDeferred) {
		return &Error{Kind: KindPermission, Op: "pair", Err: err,
			Remediation: "the device accepted supervised pairing but is locked: unlock it once, then pair again"}
	}
	if msg := err.Error(); strings.Contains(msg, "PairingDialog") || strings.Contains(msg, "pairing dialog") {
		return &Error{Kind: KindPermission, Op: "pair", Err: err,
			Remediation: "tap \"Trust\" on the device screen, then run \"xkvm device pair\" again"}
	}
	return g.wrap(KindPermission, "pair", "pairing refused by the device", err)
}

func (g *GoIOS) Info(ctx context.Context, udid string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return Info{}, err
	}
	var out Info
	err = g.run(ctx, func() error {
		vals, err := ios.GetValues(dev)
		if err != nil {
			return err
		}
		out = Info{
			Name:            vals.Value.DeviceName,
			ProductVersion:  vals.Value.ProductVersion,
			ProductType:     vals.Value.ProductType,
			BuildVersion:    vals.Value.BuildVersion,
			FirmwareVersion: vals.Value.FirmwareVersion,
			DeviceColor:     vals.Value.DeviceColor,
			HardwareModel:   vals.Value.HardwareModel,
			CPUArchitecture: vals.Value.CPUArchitecture,
			SerialNumber:    vals.Value.SerialNumber,
		}
		return nil
	})
	if err != nil {
		return Info{}, g.wrap(KindConnection, "read device info", "unlock the device and make sure it trusts this computer", err)
	}
	return out, nil
}

func (g *GoIOS) Battery(ctx context.Context, udid string) (Battery, error) {
	if err := ctx.Err(); err != nil {
		return Battery{}, err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return Battery{}, err
	}
	var out Battery
	err = g.run(ctx, func() error {
		b, err := ios.GetBatteryDiagnostics(dev)
		if err != nil {
			return err
		}
		out = Battery{
			CurrentCapacity: b.BatteryCurrentCapacity,
			IsCharging:      b.BatteryIsCharging,
			ExternalCharged: b.ExternalConnected,
			FullyCharged:    b.FullyCharged,
			HasBattery:      b.HasBattery,
		}
		return nil
	})
	if err != nil {
		return Battery{}, g.wrap(KindConnection, "read battery", "", err)
	}
	return out, nil
}

func (g *GoIOS) Apps(ctx context.Context, udid string, system bool) ([]App, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return nil, err
	}
	var out []App
	err = g.run(ctx, func() error {
		svc, err := installationproxy.New(dev)
		if err != nil {
			return fmt.Errorf("connecting to installation proxy: %w", err)
		}
		defer svc.Close()
		var infos []installationproxy.AppInfo
		if system {
			infos, err = svc.BrowseSystemApps()
		} else {
			infos, err = svc.BrowseUserApps()
		}
		if err != nil {
			return fmt.Errorf("browsing apps: %w", err)
		}
		for _, a := range infos {
			out = append(out, App{BundleID: a.CFBundleIdentifier(), Name: a.CFBundleName(), Path: a.Path()})
		}
		return nil
	})
	if err != nil {
		return nil, g.wrap(KindConnection, "list apps", "", err)
	}
	return out, nil
}

func (g *GoIOS) Install(ctx context.Context, udid, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.tunneledEntry(ctx, udid)
	if err != nil {
		return err
	}
	body := func(dev ios.DeviceEntry) error {
		conn, err := zipconduit.New(dev)
		if err != nil {
			return fmt.Errorf("connecting to the install conduit: %w", err)
		}
		defer conn.Close()
		if err := conn.SendFile(path); err != nil {
			return fmt.Errorf("streaming %s: %w", path, err)
		}
		return nil
	}
	err = g.run(ctx, func() error { return body(dev) })
	if err != nil && tunnelGate(err) && g.tunnelReady(ctx, udid) {
		if dev2, derr := g.tunneledEntry(ctx, udid); derr == nil {
			if rerr := g.run(ctx, func() error { return body(dev2) }); rerr == nil {
				return nil
			}
		}
		return g.wrap(KindNotFound, "install", manualTunnelRemediation(udid), err)
	}
	if err != nil {
		return g.wrap(KindInternal, "install", "check disk space on the device and try again", err)
	}
	return nil
}

// tunnelReady makes sure a developer tunnel for udid exists and publishes
// coordinates, auto-starting one when allowed. Failures are logged (never
// silent); the bool tells the caller whether an over-the-tunnel retry is
// worth attempting.
func (g *GoIOS) tunnelReady(ctx context.Context, udid string) bool {
	if g.tunnels == nil {
		return false
	}
	err := g.tunnels.ensure(ctx, udid)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errAutoTunnelDisabled):
		log.Infof("auto-tunnel skipped (%v)", err)
	default:
		log.Warnf("auto-tunnel unavailable: %v", err)
	}
	return false
}

func (g *GoIOS) Uninstall(ctx context.Context, udid, bundleID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.run(ctx, func() error {
		svc, err := installationproxy.New(dev)
		if err != nil {
			return fmt.Errorf("connecting to installation proxy: %w", err)
		}
		defer svc.Close()
		return svc.Uninstall(bundleID)
	})
	if err != nil {
		return g.wrap(KindInternal, "uninstall", "is the bundle id spelled exactly as shown by \"xkvm device apps\"?", err)
	}
	return nil
}

func (g *GoIOS) Launch(ctx context.Context, udid, bundleID string, env map[string]string, args []string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dev, err := g.tunneledEntry(ctx, udid)
	if err != nil {
		return 0, err
	}
	var pid uint64
	body := func(dev ios.DeviceEntry) error {
		pc, err := instruments.NewProcessControl(dev)
		if err != nil {
			return fmt.Errorf("connecting to process control: %w", err)
		}
		defer pc.Close()
		var (
			// Upstream's own defaults: KillExisting 0 replaces a running
			// instance (the app relaunches cleanly instead of erroring).
			opts = map[string]any{"KillExisting": uint64(0)}
			la   = make([]any, 0, len(args))
		)
		for _, a := range args {
			la = append(la, a)
		}
		var e map[string]any
		if len(env) > 0 {
			e = make(map[string]any, len(env))
			for k, v := range env {
				e[k] = v
			}
		}
		if len(la) > 0 || len(e) > 0 {
			pid, err = pc.LaunchAppWithArgs(bundleID, la, e, opts)
		} else {
			pid, err = pc.LaunchApp(bundleID, opts)
		}
		return err
	}
	err = g.run(ctx, func() error { return body(dev) })
	if err != nil && tunnelGate(err) && g.tunnelReady(ctx, udid) {
		if dev2, derr := g.tunneledEntry(ctx, udid); derr == nil {
			if rerr := g.run(ctx, func() error { return body(dev2) }); rerr == nil {
				return pid, nil
			}
		}
		return 0, g.wrap(KindNotFound, "launch", manualTunnelRemediation(udid), err)
	}
	if err != nil {
		return 0, g.wrap(KindNotFound, "launch", "is the bundle id installed? see \"xkvm device apps\"", err)
	}
	return pid, nil
}

func (g *GoIOS) Kill(ctx context.Context, udid string, pid uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.tunneledEntry(ctx, udid)
	if err != nil {
		return err
	}
	body := func(dev ios.DeviceEntry) error {
		pc, err := instruments.NewProcessControl(dev)
		if err != nil {
			return fmt.Errorf("connecting to process control: %w", err)
		}
		defer pc.Close()
		return pc.KillProcess(pid)
	}
	err = g.run(ctx, func() error { return body(dev) })
	if err != nil && tunnelGate(err) && g.tunnelReady(ctx, udid) {
		if dev2, derr := g.tunneledEntry(ctx, udid); derr == nil {
			if rerr := g.run(ctx, func() error { return body(dev2) }); rerr == nil {
				return nil
			}
		}
		return g.wrap(KindNotFound, "kill", manualTunnelRemediation(udid), err)
	}
	if err != nil {
		return g.wrap(KindNotFound, "kill", "the process may have exited already", err)
	}
	return nil
}

// Syslog streams device logs to w until ctx is cancelled or the relay
// ends. filter narrows the stream (zero value = everything).
//
// Stock iOS 16 and older expose the classic syslog relay. iOS 17+ removed
// it; there xkvm streams the os_trace relay over a developer tunnel
// instead — the same path pymobiledevice3 uses — auto-starting the tunnel
// when allowed. The two sources have different field shapes; both are
// rendered as `timestamp process[pid] <level>: message`.
func (g *GoIOS) Syslog(ctx context.Context, udid string, w io.Writer, filter LogFilter) error {
	dev, err := g.tunneledEntry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.streamSyslogRelay(ctx, dev, w, filter)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !tunnelGate(err) {
		return g.wrap(KindConnection, "syslog", "", err)
	}
	// iOS 17+: fall back to os_trace over the developer tunnel.
	if g.tunnelReady(ctx, udid) {
		dev2, derr := g.tunneledEntry(ctx, udid)
		if derr == nil {
			if ostraceErr := g.streamOstrace(ctx, dev2, w, filter); ostraceErr == nil {
				return nil
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return g.wrap(KindNotFound, "syslog",
			"stock iOS 17+ removed the classic syslog relay; streaming os_trace over a\n"+
				"developer tunnel is the replacement, but it did not come up here:\n"+
				manualTunnelRemediation(udid), err)
	}
	return g.wrap(KindNotFound, "syslog",
		"stock iOS 17+ removed the classic syslog relay — logs need a developer tunnel\n"+
			"there (the same prerequisite pymobiledevice3 has). Not a fault of this machine.", err)
}

// streamSyslogRelay reads the classic relay until EOF/error, applying
// filter. Unparseable lines still surface raw (dropping logs silently is
// worse than showing them), unless they lose the Contains match too.
func (g *GoIOS) streamSyslogRelay(ctx context.Context, dev ios.DeviceEntry, w io.Writer, filter LogFilter) error {
	conn, err := syslog.New(dev)
	if err != nil {
		return err
	}
	defer conn.Close()
	parse := syslog.Parser()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := conn.ReadLogMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		entry, perr := parse(msg)
		if perr != nil {
			if filter.Contains == "" || strings.Contains(msg, filter.Contains) {
				fmt.Fprintln(w, msg)
			}
			continue
		}
		if !filter.matches(entry.Process, entry.Message) {
			continue
		}
		fmt.Fprintf(w, "%s %s[%s] <%s>: %s\n", entry.Timestamp, entry.Process, entry.PID, entry.Level, entry.Message)
	}
}

// matches reports whether a log line passes the filter. Process is a
// case-insensitive substring match on the emitter name.
func (f LogFilter) matches(process, message string) bool {
	if f.Process != "" && !strings.Contains(strings.ToLower(process), strings.ToLower(f.Process)) {
		return false
	}
	if f.Contains != "" && !strings.Contains(message, f.Contains) {
		return false
	}
	return true
}

// streamOstrace renders the iOS 17+ os_trace relay through w. It runs its
// own loop (streaming operations bypass run's timeout budget) and honors
// ctx between entries.
func (g *GoIOS) streamOstrace(ctx context.Context, dev ios.DeviceEntry, w io.Writer, filter LogFilter) error {
	conn, err := ostrace.New(dev, -1, ostrace.MessageFilterLogMessage, ostrace.StreamFlagsAll)
	if err != nil {
		return err
	}
	defer conn.Close()
	cf := ostrace.ClientFilter{Match: filter.Contains}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := conn.ReadFilteredEntry(cf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		process := entry.ImageName
		if i := strings.LastIndexByte(process, '/'); i >= 0 {
			process = process[i+1:]
		}
		if !filter.matches(process, entry.Message) {
			continue
		}
		fmt.Fprintf(w, "%s %s[%d] <%s>: %s\n",
			entry.Timestamp.Format(time.RFC3339), process, entry.PID, entry.LevelName, entry.Message)
	}
}

// Screenshot captures the device screen via the instruments screenshot
// service — USB-direct on stock iOS 16 and older, over the developer
// tunnel on iOS 17+.
func (g *GoIOS) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	dev, err := g.tunneledEntry(ctx, udid)
	if err != nil {
		return nil, err
	}
	var shot []byte
	body := func(dev ios.DeviceEntry) error {
		svc, err := instruments.NewScreenshotService(dev)
		if err != nil {
			return fmt.Errorf("connecting to the screenshot service: %w", err)
		}
		defer svc.Close()
		b, err := svc.TakeScreenshot()
		if err != nil {
			return fmt.Errorf("taking the screenshot: %w", err)
		}
		shot = b
		return nil
	}
	err = g.run(ctx, func() error { return body(dev) })
	if err != nil && tunnelGate(err) && g.tunnelReady(ctx, udid) {
		if dev2, derr := g.tunneledEntry(ctx, udid); derr == nil {
			if rerr := g.run(ctx, func() error { return body(dev2) }); rerr == nil {
				return shot, nil
			}
		}
		return nil, g.wrap(KindNotFound, "screenshot", manualTunnelRemediation(udid), err)
	}
	if err != nil {
		return nil, g.wrap(KindConnection, "screenshot", "unlock the device and try again", err)
	}
	return shot, nil
}

// Watch streams usbmuxd attach/detach notifications until ctx is done or
// fn returns an error (which terminates the watch and is returned as-is).
func (g *GoIOS) Watch(ctx context.Context, fn func(WatchEvent) error) error {
	next, closeFn, err := ios.Listen()
	if err != nil {
		return g.wrap(KindConnection, "watch", "is usbmuxd running? see \"xkvm device doctor\"", err)
	}
	defer func() { _ = closeFn() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := next()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return ctx.Err()
			}
			return g.wrap(KindConnection, "watch", "", err)
		}
		ev := WatchEvent{
			UDID:     msg.DeviceEntry().Properties.SerialNumber,
			Attached: msg.DeviceAttached(),
		}
		if ev.UDID == "" {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}

// DevModeStatus reads the iOS 16+ Developer Mode switch from lockdown's amfi
// domain. iOS 15 and older never report the domain — that maps to
// DevMode{Reported: false}, which callers render as "always allowed".
func (g *GoIOS) DevModeStatus(ctx context.Context, udid string) (DevMode, error) {
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return DevMode{}, err
	}
	var out DevMode
	err = g.run(ctx, func() error {
		conn, err := ios.ConnectLockdownWithSession(dev)
		if err != nil {
			return err
		}
		defer conn.Close()
		v, err := conn.GetValueForDomain("DeveloperModeStatus", "com.apple.security.mac.amfi")
		if err != nil {
			// A missing domain is the pre-iOS-16 shape, not a failure.
			out = DevMode{}
			return nil
		}
		enabled, _ := v.(bool)
		out = DevMode{Enabled: enabled, Reported: true}
		return nil
	})
	if err != nil {
		return DevMode{}, g.wrap(KindConnection, "developer mode status", "unlock the device and make sure it trusts this computer", err)
	}
	return out, nil
}

func (g *GoIOS) Restart(ctx context.Context, udid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.run(ctx, func() error { return diagnostics.Reboot(dev) })
	if err != nil {
		return g.wrap(KindConnection, "restart", "", err)
	}
	return nil
}

func (g *GoIOS) Shutdown(ctx context.Context, udid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.run(ctx, func() error { return diagnostics.Shutdown(dev) })
	if err != nil {
		return g.wrap(KindConnection, "shutdown", "", err)
	}
	return nil
}

// run executes an operation that has no context plumbing in go-ios,
// enforcing g.timeout and ctx cancellation from our side. On ctx/timeout
// the inner goroutine may outlive this call — bounded to one per CLI
// invocation (the Handler is per-process), which is why the seam documents
// it rather than pretending the wrapper can cancel upstream dials.
func (g *GoIOS) run(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return &Error{Kind: KindTimeout, Op: "operation", Err: ctx.Err()}
	case <-time.After(g.timeout):
		return &Error{Kind: KindTimeout, Op: "operation", Err: errors.New("no answer from the device"),
			Remediation: "retry; if it keeps happening, replug the cable and re-trust the computer"}
	}
}

func (g *GoIOS) wrap(kind ErrorKind, op, remediation string, err error) error {
	return &Error{Kind: kind, Op: op, Remediation: remediation, Err: err}
}

// tunnelGate reports whether err is the iOS 17+ service gate: instruments
// and the install conduit are only offered once a developer tunnel (or
// Developer Disk Image) is active on the host.
func tunnelGate(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sig := range []string{"needs an active tunnel", "InvalidService", "Developer Image", "Failed connecting to service", "Have you mounted"} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}
