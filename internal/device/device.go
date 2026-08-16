// Package device wraps go-ios (github.com/danielpaulus/go-ios) with the
// xkvm contract: typed errors with remediation text, a deterministic
// device-resolution policy, and a Handler seam so every operation is
// testable without hardware. The heavy lifting is upstream; everything here
// is policy, packaging, and honest error reporting.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Transport is how a device is attached to the host.
type Transport string

const (
	TransportUSB     Transport = "USB"
	TransportNetwork Transport = "Network"
)

// Dev is the xkvm-facing view of a connected device.
type Dev struct {
	UDID      string    // Properties.SerialNumber in usbmuxd terms
	Transport Transport // USB or Network
}

// Info is the lockdown value subset worth printing.
type Info struct {
	Name            string
	ProductVersion  string
	ProductType     string
	BuildVersion    string
	FirmwareVersion string
	DeviceColor     string
	HardwareModel   string
	CPUArchitecture string
	SerialNumber    string
}

// Battery is the battery diagnostics subset.
type Battery struct {
	CurrentCapacity uint64
	IsCharging      bool
	ExternalCharged bool
	FullyCharged    bool
	HasBattery      bool
}

// App is one installed application.
type App struct {
	BundleID string
	Name     string
	Path     string
}

// Supervised packs an Apple Configurator identity for supervised pairing.
type Supervised struct {
	P12      []byte
	Password string
}

// Options configure a Handler. Zero values are usable; Timeout defaults to
// 15s per operation where the underlying transport supports deadlines.
type Options struct {
	Timeout time.Duration
}

// ErrorKind classifies failures so the CLI can map them to exit codes and
// remediation text.
type ErrorKind int

const (
	KindUsage      ErrorKind = iota // bad invocation (ambiguous device, no device)
	KindNotFound                    // device, app, or service missing
	KindPermission                  // trust dialog, locked device, pairing refused
	KindTimeout                     // operation exceeded its deadline
	KindConnection                  // usbmuxd / transport failure
	KindInternal                    // everything else
)

// Error is a device operation failure with a human-remediation hint.
type Error struct {
	Kind        ErrorKind
	Op          string // the operation that failed, e.g. "pair"
	Remediation string // what the user should do next
	Err         error  // underlying cause (may be nil)
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s failed", e.Op)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Remediation != "" {
		msg += "\n  " + e.Remediation
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode maps an error to a process exit code. device failures use
// 64-70; everything else stays 1.
func ExitCode(err error) int {
	var de *Error
	if !errors.As(err, &de) {
		return 1
	}
	switch de.Kind {
	case KindUsage:
		return 64
	case KindNotFound:
		return 65
	case KindPermission:
		return 67
	case KindTimeout:
		return 68
	case KindConnection:
		return 69
	default:
		return 70
	}
}

// Handler is the device-control seam. The production implementation adapts
// go-ios; tests use a fake. All methods take a context; implementations
// honor ctx where the upstream transport allows it and always stop
// streaming loops on ctx cancellation.
type Handler interface {
	io.Closer
	List(ctx context.Context) ([]Dev, error)
	Pair(ctx context.Context, udid string, sup *Supervised) error
	Info(ctx context.Context, udid string) (Info, error)
	Battery(ctx context.Context, udid string) (Battery, error)
	Apps(ctx context.Context, udid string, system bool) ([]App, error)
	Install(ctx context.Context, udid, path string) error
	Uninstall(ctx context.Context, udid, bundleID string) error
	Launch(ctx context.Context, udid, bundleID string, env map[string]string, args []string) (uint64, error)
	Kill(ctx context.Context, udid string, pid uint64) error
	Syslog(ctx context.Context, udid string, w io.Writer) error
	Restart(ctx context.Context, udid string) error
	Shutdown(ctx context.Context, udid string) error
}

// Resolve picks the device an operation should target: an explicit UDID
// wins, otherwise there must be exactly one attached device. Ambiguity is
// an error listing the candidates — resolution must never guess.
func Resolve(ctx context.Context, h Handler, udid string) (Dev, error) {
	devs, err := h.List(ctx)
	if err != nil {
		return Dev{}, fmt.Errorf("listing devices: %w", err)
	}
	if len(devs) == 0 {
		return Dev{}, &Error{Kind: KindNotFound, Op: "list devices",
			Remediation: "no iOS device is connected. Check that:\n" +
				"  - the device is plugged in (or on the same network for WiFi mode)\n" +
				"  - usbmuxd is running (default on macOS; \"usbmuxd\" on Linux)\n" +
				"  - the device is unlocked and has trusted this computer before"}
	}
	if udid != "" {
		for _, d := range devs {
			if d.UDID == udid {
				return d, nil
			}
		}
		return Dev{}, &Error{Kind: KindNotFound, Op: "select device", Err: fmt.Errorf("no device with UDID %q", udid)}
	}
	if len(devs) > 1 {
		var ids []string
		for _, d := range devs {
			ids = append(ids, fmt.Sprintf("%s (%s)", d.UDID, d.Transport))
		}
		return Dev{}, &Error{Kind: KindUsage, Op: "select device", Err: fmt.Errorf("%d devices are connected", len(devs)),
			Remediation: "pick one with --udid:\n  " + strings.Join(ids, "\n  ")}
	}
	return devs[0], nil
}
