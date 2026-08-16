package device

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"howett.net/plist"
)

func TestMBDBGoldenRecord(t *testing.T) {
	rec := mbdbRecord{
		Domain: "Dm", Filename: "f", Link: "ln",
		Hash: []byte{0xAA}, Key: []byte{0xBB, 0xCC},
		Mode: 0o100755, Inode: 1, UserID: 2, GroupID: 3,
		MTime: 4, ATime: 5, CTime: 6, Size: 7, Flags: 4,
		Properties: [][2]string{{"n", "v"}},
	}
	got := rec.bytes()
	var want []byte
	put16 := func(v uint16) { want = binary.BigEndian.AppendUint16(want, v) }
	put32 := func(v uint32) { want = binary.BigEndian.AppendUint32(want, v) }
	put64 := func(v uint64) { want = binary.BigEndian.AppendUint64(want, v) }
	putStr := func(s string) {
		put16(uint16(len(s)))
		want = append(want, s...)
	}
	putStr("Dm")
	putStr("f")
	putStr("ln")
	put16(1)
	want = append(want, 0xAA)
	put16(2)
	want = append(want, 0xBB, 0xCC)
	put16(0o100755)
	put64(1)
	put32(2)
	put32(3)
	put32(4)
	put32(5)
	put32(6)
	put64(7)
	want = append(want, 4)
	want = append(want, 1)
	putStr("n")
	putStr("v")
	if string(got) != string(want) {
		t.Fatalf("record bytes wrong:\n got %x\nwant %x", got, want)
	}
}

func TestOmegaVerdictTable(t *testing.T) {
	cases := []struct {
		ver     string
		want    Support
		whyPart string
	}{
		{"15.8", Unsupported, "16 or higher"},
		{"16.0", Supported, "16-18"},
		{"18.7", Supported, "16-18"},
		// Apple skipped 19-25: year-based versioning went 18 -> 26.
		{"19.0", Unsupported, "never released"},
		{"24.3", Unsupported, "never released"},
		{"26.1", Supported, "live-verified"},
		{"27.0", Unsupported, "reset your data"},
		// 28+ is unreleased: caution, not a hard block — nobody has run it.
		{"30.1", Untested, "future/unreleased"},
	}
	for _, c := range cases {
		got, why, err := OmegaVerdict(c.ver)
		if err != nil {
			t.Errorf("OmegaVerdict(%q) err: %v", c.ver, err)
			continue
		}
		if got != c.want {
			t.Errorf("OmegaVerdict(%q) = %v, want %v", c.ver, got, c.want)
		}
		if c.whyPart != "" && !strings.Contains(why, c.whyPart) {
			t.Errorf("why for %q = %q, want part %q", c.ver, why, c.whyPart)
		}
	}
	if _, _, err := OmegaVerdict("garbage"); err == nil {
		t.Error("garbage version accepted")
	}
}

func TestBuildOmegaBackup(t *testing.T) {
	b, err := buildOmegaBackup()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b.mbdb), "mbdb\x05\x00") {
		t.Fatalf("bad mbdb header: %x", b.mbdb[:6])
	}
	n := 0
	off := 6
	for off < len(b.mbdb) {
		n++
		off = nextMBDBRecord(t, b.mbdb, off)
	}
	if n != len(omegaFiles) {
		t.Errorf("records = %d, want %d", n, len(omegaFiles))
	}
	if len(b.payloads) != 2 {
		t.Errorf("payloads = %d, want 2", len(b.payloads))
	}
	if _, ok := b.payloads[payloadName("ManagedPreferencesDomain", "mobile/com.apple.purplebuddy.plist")]; !ok {
		t.Error("purplebuddy payload missing under its sha1 name")
	}
	var status map[string]interface{}
	if _, err := plist.Unmarshal(b.status, &status); err != nil {
		t.Fatal(err)
	}
	if status["Version"] != "2.4" || status["BackupState"] != "new" {
		t.Errorf("status wrong: %v", status)
	}
	var manifest map[string]interface{}
	if _, err := plist.Unmarshal(b.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["Version"] != "9.1" || manifest["SystemDomainsVersion"] != "20.0" {
		t.Errorf("manifest wrong: %v", manifest)
	}
	if kb, ok := manifest["BackupKeyBag"].([]byte); !ok || len(kb) != 1336 {
		t.Errorf("keybag wrong: %T", manifest["BackupKeyBag"])
	}
}

// nextMBDBRecord walks one record from the mbdb stream (test-only decoder).
func nextMBDBRecord(t *testing.T, data []byte, off int) int {
	t.Helper()
	rd := func() {
		if off+2 > len(data) {
			t.Fatalf("mbdb truncated at %d/%d", off, len(data))
		}
		off += 2 + int(binary.BigEndian.Uint16(data[off:off+2]))
	}
	rd()                                 // domain
	rd()                                 // filename
	rd()                                 // link
	rd()                                 // hash
	rd()                                 // key
	off += 2 + 8 + 4 + 4 + 4 + 4 + 4 + 8 // mode,inode,uids,times,size
	off += 1                             // flags
	props := int(data[off])
	off += 1
	for i := 0; i < props; i++ {
		rd()
		rd()
	}
	return off
}

// scriptedOmegaPeer drives restoreViaLink over a net.Pipe with a scripted
// device on the other end, and collects what reached it.
type scriptedOmegaPeer struct {
	server net.Conn
	svF    *linkFrames
	client net.Conn
	b      omegaBackup

	gotOpts map[string]interface{}
}

func newScriptedOmega(t *testing.T) *scriptedOmegaPeer {
	t.Helper()
	cl, sv := net.Pipe()
	b, err := buildOmegaBackup()
	if err != nil {
		t.Fatal(err)
	}
	p := &scriptedOmegaPeer{
		server: sv, client: cl, b: b,
		svF: &linkFrames{r: bufio.NewReader(sv), w: sv},
	}
	t.Cleanup(func() {
		sv.Close()
		cl.Close()
	})
	return p
}

// run starts the client against the scripted server and returns its error.
// The ctx deadline turns any protocol mismatch into a fast, loud failure
// instead of a hung suite (net.Pipe has no timeouts of its own).
func (p *scriptedOmegaPeer) run(progress func(float64)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	frames := &linkFrames{r: bufio.NewReader(p.client), w: p.client}
	if err := deviceLinkHandshake(frames); err != nil {
		return err
	}
	return restoreViaLink(ctx, frames, "UDID", p.b, ios.DeviceEntry{}, progress)
}

func readRawPrefixed(r *bufio.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint32(l[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func TestOmegaHappyPathOverTheWire(t *testing.T) {
	p := newScriptedOmega(t)
	serverDone := make(chan error, 1)
	go func() {
		defer p.server.Close()
		if err := p.svF.writeMessage(message{"DLMessageVersionExchange", uint64(400)}); err != nil {
			serverDone <- err
			return
		}
		ack, err := p.svF.readMessage()
		if err != nil {
			serverDone <- err
			return
		}
		if ack[0] != "DLMessageVersionExchange" || ack[1] != "DLVersionsOk" || num(ack[2]) != 400 {
			serverDone <- errors.New("handshake reply wrong")
			return
		}
		if err := p.svF.writeMessage(message{"DLMessageDeviceReady"}); err != nil {
			serverDone <- err
			return
		}
		restore, err := p.svF.readMessage()
		if err != nil {
			serverDone <- err
			return
		}
		t.Logf("srv: got Restore")
		body := restore[1].(map[string]interface{})
		if body["MessageName"] != "Restore" || body["TargetIdentifier"] != "UDID" {
			serverDone <- errors.New("restore message wrong")
			return
		}
		p.gotOpts, _ = body["Options"].(map[string]interface{})
		var names []interface{}
		for _, n := range []string{"Manifest.mbdb", "Status.plist", "Info.plist", "Manifest.plist"} {
			names = append(names, n)
		}
		for name := range p.b.payloads {
			names = append(names, name)
		}
		if err := p.svF.writeMessage(message{"DLMessageDownloadFiles", names}); err != nil {
			serverDone <- err
			return
		}
		t.Logf("srv: download request sent (%d files)", len(names))
		for i, n := range names {
			t.Logf("srv: awaiting file %d/%d", i+1, len(names))
			gotName, err := readRawPrefixed(p.svF.r)
			if err != nil {
				serverDone <- err
				return
			}
			t.Logf("srv: got name %q", string(gotName))
			if string(gotName) != n.(string) {
				serverDone <- errors.New("name mismatch")
				return
			}
			var got []byte
			for {
				frame, err := readRawPrefixed(p.svF.r)
				if err != nil {
					serverDone <- err
					return
				}
				if len(frame) == 1 && frame[0] == codeSuccess {
					break
				}
				if len(frame) == 0 || frame[0] != codeFileData {
					serverDone <- errors.New("bad chunk")
					return
				}
				got = append(got, frame[1:]...)
			}
			if string(got) != string(p.b.lookup(string(gotName))) {
				serverDone <- errors.New("payload mismatch")
				return
			}
		}
		zer := make([]byte, 4)
		if _, err := io.ReadFull(p.svF.r, zer); err != nil || string(zer) != "\x00\x00\x00\x00" {
			serverDone <- errors.New("terminator missing")
			return
		}
		st, err := p.svF.readMessage()
		if err != nil {
			serverDone <- err
			return
		}
		if st[0] != "DLMessageStatusResponse" || num(st[1]) != 0 {
			serverDone <- errors.New("status wrong")
			return
		}
		if err := p.svF.writeMessage(message{"DLMessageProcessMessage", map[string]interface{}{"Content": "RestoreResult"}}); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()
	var pct []float64
	clientErr := p.run(func(v float64) { pct = append(pct, v) })
	// Never block forever: a mismatched script must fail the test, not hang it.
	var serverErr error
	select {
	case serverErr = <-serverDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("scripted server stuck (client said: %v)", clientErr)
	}
	if serverErr != nil {
		t.Fatalf("server saw: %v (client said: %v)", serverErr, clientErr)
	}
	if clientErr != nil {
		t.Fatalf("client: %v", clientErr)
	}
	if len(pct) == 0 || pct[len(pct)-1] != 100 {
		t.Errorf("final progress missing: %v", pct)
	}
	o := p.gotOpts
	if o["RestoreShouldReboot"] != true || o["RestoreDontCopyBackup"] != true ||
		o["RestorePreserveSettings"] != true || o["RestoreSystemFiles"] != true ||
		o["RemoveItemsNotRestored"] != false {
		t.Fatalf("restore options wrong: %v", o)
	}
}

func TestOmegaFindMyRefusal(t *testing.T) {
	p := newScriptedOmega(t)
	go func() {
		defer p.server.Close()
		p.svF.writeMessage(message{"DLMessageVersionExchange", uint64(400)})
		p.svF.readMessage()
		p.svF.writeMessage(message{"DLMessageDeviceReady"})
		p.svF.readMessage() // Restore
		p.svF.writeMessage(message{"DLMessageProcessMessage", map[string]interface{}{
			"ErrorCode": uint64(1), "ErrorDescription": "Find My must be disabled in order to use this tool.",
		}})
	}()
	err := p.run(nil)
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindPermission || !strings.Contains(err.Error(), "Find My") {
		t.Fatalf("want Find My permission error, got %v", err)
	}
}

func TestOmegaCrashOnPurposeIsSuccess(t *testing.T) {
	p := newScriptedOmega(t)
	go func() {
		defer p.server.Close()
		p.svF.writeMessage(message{"DLMessageVersionExchange", uint64(400)})
		p.svF.readMessage()
		p.svF.writeMessage(message{"DLMessageDeviceReady"})
		p.svF.readMessage()
		p.svF.writeMessage(message{"DLMessageProcessMessage", map[string]interface{}{
			"ErrorCode": uint64(1), "ErrorDescription": "crash_on_purpose",
		}})
	}()
	if err := p.run(nil); err != nil {
		t.Fatalf("wanted nil for crash_on_purpose, got %v", err)
	}
}

func TestOmegaUnknownMissingFileSendsErrorCode(t *testing.T) {
	p := newScriptedOmega(t)
	serverDone := make(chan error, 1)
	go func() {
		defer p.server.Close()
		p.svF.writeMessage(message{"DLMessageVersionExchange", uint64(400)})
		p.svF.readMessage()
		p.svF.writeMessage(message{"DLMessageDeviceReady"})
		p.svF.readMessage()
		p.svF.writeMessage(message{"DLMessageDownloadFiles", []interface{}{"nope.hex"}})
		name, err := readRawPrefixed(p.svF.r)
		if err != nil || string(name) != "nope.hex" {
			serverDone <- errors.New("name not served")
			return
		}
		frame, err := readRawPrefixed(p.svF.r)
		if err != nil || len(frame) < 1 || frame[0] != codeError {
			serverDone <- errors.New("missing file not reported as error")
			return
		}
		zer := make([]byte, 4)
		io.ReadFull(p.svF.r, zer)
		st, _ := p.svF.readMessage()
		if st[0] != "DLMessageStatusResponse" {
			serverDone <- errors.New("no status after missing file")
			return
		}
		defer p.svF.writeMessage(message{"DLMessageProcessMessage", map[string]interface{}{"Content": "done"}})
		serverDone <- nil
	}()
	if err := p.run(nil); err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

// TestOmegaLookupCleansDotSlash is the regression guard for the live iOS 26
// finding: the device requested "./Manifest.plist" and the exact-match
// lookup reported it missing. Cleaned names must serve the real files.
func TestOmegaLookupCleansDotSlash(t *testing.T) {
	b, err := buildOmegaBackup()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"./Manifest.plist", "./Status.plist", "./Info.plist", "./Manifest.mbdb", "Manifest.plist"} {
		if b.lookup(n) == nil {
			t.Errorf("lookup(%q) came back empty", n)
		}
	}
	if b.lookup("./definitely-not-there.deb") != nil {
		t.Error("unknown name resolved")
	}
}
