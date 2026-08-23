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
	"github.com/danielpaulus/go-ios/ios/syslog"
	"github.com/danielpaulus/go-ios/ios/zipconduit"
)

// GoIOS adapts go-ios v1.3.x to the Handler contract.
type GoIOS struct {
	timeout time.Duration
}

// New builds the production handler. Options.Timeout <= 0 means 15s.
func New(opts Options) *GoIOS {
	t := opts.Timeout
	if t <= 0 {
		t = 15 * time.Second
	}
	return &GoIOS{timeout: t}
}

// Close releases nothing (go-ios connections are per-operation); it exists
// to satisfy io.Closer so callers can defer it uniformly.
func (g *GoIOS) Close() error { return nil }

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
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.run(ctx, func() error {
		conn, err := zipconduit.New(dev)
		if err != nil {
			return fmt.Errorf("connecting to the install conduit: %w", err)
		}
		defer conn.Close()
		if err := conn.SendFile(path); err != nil {
			return fmt.Errorf("streaming %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		if tunnelGate(err) {
			return g.wrap(KindNotFound, "install", tunnelRemediation, err)
		}
		return g.wrap(KindInternal, "install", "check disk space on the device and try again", err)
	}
	return nil
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
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return 0, err
	}
	var pid uint64
	err = g.run(ctx, func() error {
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
	})
	if err != nil {
		if tunnelGate(err) {
			return 0, g.wrap(KindNotFound, "launch", tunnelRemediation, err)
		}
		return 0, g.wrap(KindNotFound, "launch", "is the bundle id installed? see \"xkvm device apps\"", err)
	}
	return pid, nil
}

func (g *GoIOS) Kill(ctx context.Context, udid string, pid uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	err = g.run(ctx, func() error {
		pc, err := instruments.NewProcessControl(dev)
		if err != nil {
			return fmt.Errorf("connecting to process control: %w", err)
		}
		defer pc.Close()
		return pc.KillProcess(pid)
	})
	if err != nil {
		if tunnelGate(err) {
			return g.wrap(KindNotFound, "kill", tunnelRemediation, err)
		}
		return g.wrap(KindNotFound, "kill", "the process may have exited already", err)
	}
	return nil
}

func (g *GoIOS) Syslog(ctx context.Context, udid string, w io.Writer) error {
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	conn, err := syslog.New(dev)
	if err != nil {
		if tunnelGate(err) {
			return g.wrap(KindNotFound, "syslog",
				"stock iOS 17+ removed the classic syslog relay service — syslog works on\njailbroken devices and stock iOS 16 and older. Not a fault of this machine.", err)
		}
		return g.wrap(KindConnection, "syslog", "", err)
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
			return g.wrap(KindConnection, "syslog", "", err)
		}
		entry, perr := parse(msg)
		if perr != nil {
			// Unparseable lines still surface — dropping logs silently is
			// worse than showing them raw.
			fmt.Fprintln(w, msg)
			continue
		}
		fmt.Fprintf(w, "%s %s[%s] <%s>: %s\n", entry.Timestamp, entry.Process, entry.PID, entry.Level, entry.Message)
	}
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

const tunnelRemediation = "iOS 17+ gated the process-control and install services behind a developer tunnel:\n" +
	"  go run github.com/danielpaulus/go-ios@v1.3.2 tunnel start --userspace --udid <UDID>\n" +
	"in a second terminal, then retry. --userspace needs no admin on macOS, Linux,\n" +
	"or Windows; the same prerequisite as pymobiledevice3's Developer Disk Image\n" +
	"mount — nothing is wrong with the device."
