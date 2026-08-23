package device

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/diagnostics"
	"howett.net/plist"
)

// The mobilebackup2 device-link protocol, as spoken by pymobiledevice3's
// DeviceLink (the engine Omega uses). Framing: 4-byte big-endian payload
// length + plist. Outgoing messages are XML (matching the proven Omega
// path); incoming messages are decoded from either XML or binary.
const (
	mb2Service = "com.apple.mobilebackup2"

	// One-byte transfer codes inside file payloads.
	codeFileData   = 0xC
	codeError      = 0xB
	codeSuccess    = 0
	fileTerminator = "\x00\x00\x00\x00"
)

// omegaRestore runs the partial-backup restore on the device: connect the
// mobilebackup2 service, handshake, send Restore, serve the backup files,
// wait for the device's verdict, and reboot it.
func (g *GoIOS) OmegaRestore(ctx context.Context, udid string, progress func(float64)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dev, err := g.entry(ctx, udid)
	if err != nil {
		return err
	}
	backup, err := buildOmegaBackup()
	if err != nil {
		return &Error{Kind: KindInternal, Op: "omega", Err: err}
	}
	conn, err := ios.ConnectToService(dev, mb2Service)
	if err != nil {
		if tunnelGate(err) {
			return g.wrap(KindNotFound, "omega", manualTunnelRemediation(udid), err)
		}
		return g.wrap(KindConnection, "omega", "service refused — device locked? unlock and retry", err)
	}
	defer conn.Close()

	frames := &linkFrames{r: bufio.NewReader(conn.Reader()), w: conn.Writer()}
	if err := deviceLinkHandshake(frames); err != nil {
		return g.wrap(KindConnection, "omega", "device link handshake failed (device must be unlocked and trusted)", err)
	}
	return restoreViaLink(ctx, frames, udid, backup, dev, progress)
}

// restoreViaLink is the protocol core after connect+handshake: send
// Restore, serve the file batches, dispatch the device-link messages until
// the device reports done, then reboot. Standalone so the whole exchange
// is testable against a scripted peer — everything the wire sees starts
// here, exactly as the real device sees it.
func restoreViaLink(ctx context.Context, frames *linkFrames, udid string, backup omegaBackup, dev ios.DeviceEntry, progress func(float64)) error {
	if err := frames.writeMessage(message{"DLMessageProcessMessage", map[string]any{
		"MessageName":      "Restore",
		"TargetIdentifier": udid,
		"SourceIdentifier": ".",
		"Options": map[string]any{
			"RestoreShouldReboot":     true,
			"RestoreDontCopyBackup":   true,
			"RestorePreserveSettings": true,
			"RestoreSystemFiles":      true,
			"RemoveItemsNotRestored":  false,
		},
	}}); err != nil {
		return &Error{Kind: KindConnection, Op: "omega", Err: err}
	}
	for {
		select {
		case <-ctx.Done():
			return &Error{Kind: KindTimeout, Op: "omega", Err: ctx.Err(), Remediation: "the restore takes a few minutes; the phone reboots by itself at the end"}
		default:
		}
		msg, err := frames.readMessage()
		if err != nil {
			// The device reboots itself when it is done (crash_on_purpose);
			// an EOF right after the success content is the normal ending.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return &Error{Kind: KindConnection, Op: "omega", Err: err}
		}
		switch msg[0] {
		case "DLMessageProcessMessage":
			body, _ := msg[1].(map[string]any)
			if body == nil {
				continue
			}
			if code := num(body["ErrorCode"]); code != 0 {
				derr := fmt.Sprintf("%v", body)
				if strings.Contains(derr, "Find My") {
					return &Error{Kind: KindPermission, Op: "omega", Err: fmt.Errorf("restore refused: %s", derr),
						Remediation: "Find My must be off: Settings → [your name] → Find My → Find My iPhone → off, then retry"}
				}
				if strings.Contains(derr, "crash_on_purpose") {
					return nil // the device rebooted mid-restore: the restore won
				}
				return &Error{Kind: KindInternal, Op: "omega", Err: fmt.Errorf("the device refused the restore: %s", derr)}
			}
			if pct, ok := body["Percent"].(float64); ok && progress != nil {
				progress(pct)
				continue
			}
			if pct := num(body["Percent"]); pct > 0 && pct <= 100 {
				progressSafe(progress, float64(pct))
				continue
			}
			if _, done := body["Content"]; done {
				progressSafe(progress, 100)
				_ = diagnostics.Reboot(dev)
				return nil
			}
		case "DLMessageDownloadFiles":
			names, _ := msg[1].([]any)
			if err := serveOmegaFiles(frames, backup, names); err != nil {
				return &Error{Kind: KindConnection, Op: "omega", Err: err}
			}
		case "DLMessageGetFreeDiskSpace", "DLContentsOfDirectory":
			if err := frames.status(0, map[string]any{}); err != nil {
				return &Error{Kind: KindConnection, Op: "omega", Err: err}
			}
		case "DLMessageCreateDirectory", "DLMessageRemoveItems", "DLMessageMoveItems", "DLMessageCopyItem":
			if err := frames.status(0, map[string]any{}); err != nil {
				return &Error{Kind: KindConnection, Op: "omega", Err: err}
			}
		case "DLMessagePurgeDiskSpace":
			if err := frames.status(1, map[string]any{}); err != nil {
				return &Error{Kind: KindConnection, Op: "omega", Err: err}
			}
		}
	}
}

func progressSafe(progress func(float64), pct float64) {
	if progress != nil {
		progress(pct)
	}
}

// serveOmegaFiles answers a DLMessageDownloadFiles batch: name + payload
// chunks for each requested file, then the zero terminator.
func serveOmegaFiles(frames *linkFrames, b omegaBackup, names []any) error {
	var missing []string
	for _, n := range names {
		name, _ := n.(string)
		if err := frames.writeRaw(binary.BigEndian.AppendUint32(nil, uint32(len(name))), []byte(name)); err != nil {
			return err
		}
		payload := b.lookup(name)
		if payload == nil {
			// A missing file is a local error, like pym3's OSError path:
			// error code + message byte, and a Multi status at the end.
			missing = append(missing, name)
			// error frame: [code][message] — writeRaw adds the one
			// length prefix the protocol asks for.
			eframe := append([]byte{codeError}, "local file error"...)
			if err := frames.writeRaw(nil, eframe); err != nil {
				return err
			}
			continue
		}
		const chunk = 128 * 1024
		for off := 0; off < len(payload); off += chunk {
			end := min(off+chunk, len(payload))
			// [code FILE_DATA][bytes…] — writeRaw adds the single
			// length prefix (which includes the code, per the spec).
			if err := frames.writeRaw(nil, append([]byte{codeFileData}, payload[off:end]...)); err != nil {
				return err
			}
		}
		// file complete: [code SUCCESS] under its own length prefix.
		if err := frames.writeRaw(nil, []byte{codeSuccess}); err != nil {
			return err
		}
	}
	// zero terminator (raw — no length prefix, like pym3) + status
	if _, err := frames.w.Write([]byte(fileTerminator)); err != nil {
		return err
	}
	if len(missing) > 0 {
		status := map[string]any{}
		for _, m := range missing {
			status[m] = map[string]any{
				"DLFileErrorString": "local file error",
				"DLFileErrorCode":   uint64(^uint64(0) - 1), // -13 pattern
			}
		}
		return frames.status(18446744073709551603, status)
	}
	return frames.status(0, map[string]any{})
}

// lookup serves the four manifest files and the payload blobs by name.
// Names are cleaned first: devices have been observed (iOS 26, live) to
// request "./Manifest.plist" — pym3's pathlib join absorbs that prefix, so
// we do the same.
func (b omegaBackup) lookup(name string) []byte {
	name = path.Clean(name)
	switch name {
	case "Manifest.mbdb":
		return b.mbdb
	case "Status.plist":
		return b.status
	case "Manifest.plist":
		return b.manifest
	case "Info.plist":
		return b.info
	}
	return b.payloads[name]
}

// deviceLinkHandshake performs the version exchange exactly like
// pymobiledevice3: receive the device's opening version message, answer
// DLVersionsOk, wait for DLMessageDeviceReady.
func deviceLinkHandshake(frames *linkFrames) error {
	msg, err := frames.readMessage()
	if err != nil {
		return fmt.Errorf("reading device hello: %w", err)
	}
	if len(msg) < 2 || (msg[0] != "DLMessageVersionExchange" && msg[0] != "DLMessageHello") {
		return fmt.Errorf("unexpected opening message %v", msg[0])
	}
	version := num(msg[1])
	if err := frames.writeMessage(message{"DLMessageVersionExchange", "DLVersionsOk", version}); err != nil {
		return err
	}
	ready, err := frames.readMessage()
	if err != nil {
		return err
	}
	if len(ready) == 0 || ready[0] != "DLMessageDeviceReady" {
		return fmt.Errorf("device not ready, said %v", ready[0])
	}
	return nil
}

// linkFrames is the 4-byte-length + plist message layer. Outgoing messages
// are XML (the Omega-proven encoding); incoming messages parse both XML
// and binary plists.
type linkFrames struct {
	r *bufio.Reader
	w io.Writer
}

type message []any

func (f *linkFrames) writeMessage(m message) error {
	body, err := plist.Marshal(m, plist.XMLFormat)
	if err != nil {
		return err
	}
	return f.writeRaw(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body)
}

var errTooBig = errors.New("frame exceeds sane size")

func (f *linkFrames) readMessage() (message, error) {
	var lenb [4]byte
	if _, err := io.ReadFull(f.r, lenb[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(lenb[:])
	if size > 64*1024*1024 {
		return nil, errTooBig
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(f.r, body); err != nil {
		return nil, err
	}
	var v any
	if _, err := plist.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("decoding frame: %w", err)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("frame is %T, want array", v)
	}
	out := make(message, 0, len(arr))
	for _, e := range arr {
		out = append(out, e)
	}
	return out, nil
}

// writeRaw writes len-prefixed blobs to the framing writer.
func (f *linkFrames) writeRaw(prefix []byte, payload []byte) error {
	if len(prefix) == 0 {
		prefix = binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	}
	var buf bytes.Buffer
	buf.Write(prefix)
	buf.Write(payload)
	_, err := f.w.Write(buf.Bytes())
	return err
}

// status sends a DLMessageStatusResponse.
func (f *linkFrames) status(code uint64, ctx map[string]any) error {
	if ctx == nil {
		ctx = map[string]any{}
	}
	return f.writeMessage(message{"DLMessageStatusResponse", code, "___EmptyParameterString___", ctx})
}

// num extracts an unsigned integer from whatever shape the plist decoder
// produced (howett may hand back int64, uint64, or float64).
func num(v any) uint64 {
	switch t := v.(type) {
	case uint64:
		return t
	case int64:
		if t < 0 {
			return 0
		}
		return uint64(t)
	case int:
		if t < 0 {
			return 0
		}
		return uint64(t)
	case float64:
		if t < 0 {
			return 0
		}
		return uint64(t)
	}
	if f, ok := v.(float64); ok {
		return uint64(f)
	}
	return 0
}
