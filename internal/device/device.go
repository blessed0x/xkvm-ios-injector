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
	UDID       string      // Properties.SerialNumber in usbmuxd terms
	Transport  Transport   // preferred transport (USB wins when both exist)
	Transports []Transport // every transport the device is reachable over
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

// LogFilter narrows what `device syslog` streams. Zero values mean
// everything. Process matches case-insensitively as a substring of the
// emitting process name; Contains matches the message body (and, for raw
// unparseable lines, the whole line).
type LogFilter struct {
	Process  string
	Contains string
}

// WatchEvent is one usbmuxd attach/detach notification.
type WatchEvent struct {
	UDID     string
	Attached bool
}

// DevMode is the iOS 16+ Developer Mode state as lockdown reports it.
type DevMode struct {
	// Enabled is the amfi DeveloperModeStatus value.
	Enabled bool
	// Reported is false when the device did not answer the amfi domain at
	// all — iOS 15 and older have no Developer Mode gate (always allowed).
	Reported bool
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
	KindUsage       ErrorKind = iota // bad invocation (ambiguous device, no device)
	KindNotFound                     // device, app, or service missing
	KindPermission                   // trust dialog, locked device, pairing refused
	KindTimeout                      // operation exceeded its deadline
	KindConnection                   // usbmuxd / transport failure
	KindUnsupported                  // the operation must not run on this version
	KindInternal                     // everything else
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
	case KindUnsupported:
		return 66
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
	Syslog(ctx context.Context, udid string, w io.Writer, filter LogFilter) error
	// Screenshot captures the device screen and returns the image bytes
	// (PNG on stock iOS; some instruments builds answer JPEG).
	Screenshot(ctx context.Context, udid string) ([]byte, error)
	// Watch streams usbmuxd attach/detach events until ctx is cancelled or
	// the callback returns an error.
	Watch(ctx context.Context, fn func(WatchEvent) error) error
	// DevModeStatus reads the iOS 16+ Developer Mode switch over lockdown.
	DevModeStatus(ctx context.Context, udid string) (DevMode, error)
	// Forward listens on hostPort and relays every accepted connection to
	// phonePort on the device over usbmuxd (iproxy semantics, every iOS
	// version). Stop it via the returned Closer or by cancelling ctx.
	Forward(ctx context.Context, udid string, hostPort, phonePort uint16) (io.Closer, error)
	// PasteboardGet reads the device clipboard; the bool reports whether
	// any text was present.
	PasteboardGet(ctx context.Context, udid string) (string, bool, error)
	// PasteboardSet writes text to the device clipboard.
	PasteboardSet(ctx context.Context, udid, text string) error
	OmegaRestore(ctx context.Context, udid string, progress func(float64)) error
	Restart(ctx context.Context, udid string) error
	Shutdown(ctx context.Context, udid string) error
}

// Distinct collapses the raw muxd list: usbmuxd reports the SAME physical
// device once per transport (USB and Network share a UDID), so the unit of
// "how many devices" is the UDID, not the transport row. Each result keeps
// every transport it was seen on; the preferred one (USB wins) is in
// Transport.
func Distinct(devs []Dev) []Dev {
	type acc struct {
		all    []Dev
		hasUSB bool
	}
	byUDID := make(map[string]*acc, len(devs))
	var order []string
	for _, d := range devs {
		if a, ok := byUDID[d.UDID]; ok {
			a.all = append(a.all, d)
			a.hasUSB = a.hasUSB || d.Transport == TransportUSB
			continue
		}
		byUDID[d.UDID] = &acc{all: []Dev{d}, hasUSB: d.Transport == TransportUSB}
		order = append(order, d.UDID)
	}
	out := make([]Dev, 0, len(order))
	for _, udid := range order {
		a := byUDID[udid]
		trans := make([]Transport, 0, len(a.all))
		for _, d := range a.all {
			trans = append(trans, d.Transport)
		}
		out = append(out, Dev{UDID: udid, Transport: preferredTransport(a.all, a.hasUSB), Transports: trans})
	}
	return out
}

// preferredTransport picks USB unless the device is only reachable over the
// network (pairing and the zip-conduit are happiest over the cable).
func preferredTransport(all []Dev, hasUSB bool) Transport {
	if hasUSB {
		return TransportUSB
	}
	if len(all) > 0 {
		return all[0].Transport
	}
	return TransportUSB
}

// Resolve picks the device an operation should target: an explicit UDID
// wins, otherwise there must be exactly one attached device (same UDID on
// USB + Network counts once). Ambiguity is an error listing the candidates
// — resolution must never guess.
func Resolve(ctx context.Context, h Handler, udid string) (Dev, error) {
	devs, err := h.List(ctx)
	if err != nil {
		return Dev{}, fmt.Errorf("listing devices: %w", err)
	}
	devs = Distinct(devs)
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
