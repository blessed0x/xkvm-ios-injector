package device

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeHandler records everything and returns scripted results.
type fake struct {
	closeCalls   int
	listCalls    int
	lists        [][]Dev
	listErr      error
	pairCalls    []pairCall
	pairErr      error
	infoCalls    int
	infoVal      Info
	infoErr      error
	appsCalls    []appsCall
	appsVal      []App
	appsErr      error
	installs     []installCall
	installErr   error
	uninstalls   []string
	launchCalls  []launchCall
	launchPID    uint64
	launchErr    error
	killCalls    []uint64
	killErr      error
	syslogOut    string
	syslogErr    error
	watchEvents  []WatchEvent
	forwardCalls [][2]uint16
	clipboard    string
	clipboardSet string
	restarts     int
	shutdowns    int
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// pngMagic is the smallest answer that satisfies the CLI's magic sniffing.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

type pairCall struct {
	udid string
	sup  *Supervised
}
type appsCall struct {
	udid   string
	system bool
}
type installCall struct {
	udid string
	path string
}

type launchCall struct {
	udid     string
	bundleID string
	env      map[string]string
	args     []string
}

func (f *fake) Close() error { f.closeCalls++; return nil }

func (f *fake) List(ctx context.Context) ([]Dev, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.lists) > 0 {
		out := f.lists[0]
		f.lists = f.lists[1:]
		return out, nil
	}
	return nil, &Error{Kind: KindNotFound, Op: "list devices", Err: errors.New("no devices")}
}

func (f *fake) Pair(ctx context.Context, udid string, sup *Supervised) error {
	f.pairCalls = append(f.pairCalls, pairCall{udid, sup})
	return f.pairErr
}

func (f *fake) Info(ctx context.Context, udid string) (Info, error) {
	f.infoCalls++
	return f.infoVal, f.infoErr
}
func (f *fake) Battery(ctx context.Context, udid string) (Battery, error) {
	return Battery{CurrentCapacity: 80, IsCharging: true}, nil
}
func (f *fake) Apps(ctx context.Context, udid string, system bool) ([]App, error) {
	f.appsCalls = append(f.appsCalls, appsCall{udid, system})
	return f.appsVal, f.appsErr
}
func (f *fake) Install(ctx context.Context, udid, path string) error {
	f.installs = append(f.installs, installCall{udid, path})
	return f.installErr
}
func (f *fake) Uninstall(ctx context.Context, udid, bundleID string) error {
	f.uninstalls = append(f.uninstalls, bundleID)
	return nil
}
func (f *fake) Launch(ctx context.Context, udid, bundleID string, env map[string]string, args []string) (uint64, error) {
	f.launchCalls = append(f.launchCalls, launchCall{udid, bundleID, env, args})
	return f.launchPID, f.launchErr
}
func (f *fake) Kill(ctx context.Context, udid string, pid uint64) error {
	f.killCalls = append(f.killCalls, pid)
	return f.killErr
}
func (f *fake) Syslog(ctx context.Context, udid string, w io.Writer, filter LogFilter) error {
	if f.syslogOut != "" {
		io.WriteString(w, f.syslogOut)
	}
	return f.syslogErr
}
func (f *fake) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	return pngMagic, nil
}
func (f *fake) Watch(ctx context.Context, fn func(WatchEvent) error) error {
	for _, ev := range f.watchEvents {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (f *fake) DevModeStatus(ctx context.Context, udid string) (DevMode, error) {
	return DevMode{Enabled: true, Reported: true}, nil
}
func (f *fake) Forward(ctx context.Context, udid string, hostPort, phonePort uint16) (io.Closer, error) {
	f.forwardCalls = append(f.forwardCalls, [2]uint16{hostPort, phonePort})
	return nopCloser{}, nil
}
func (f *fake) PasteboardGet(ctx context.Context, udid string) (string, bool, error) {
	return f.clipboard, f.clipboard != "", nil
}
func (f *fake) PasteboardSet(ctx context.Context, udid, text string) error {
	f.clipboardSet = text
	return nil
}
func (f *fake) Restart(ctx context.Context, udid string) error { f.restarts++; return nil }
func (f *fake) OmegaRestore(ctx context.Context, udid string, progress func(float64)) error {
	f.pairCalls = append(f.pairCalls, pairCall{udid, nil})
	return nil
}
func (f *fake) Shutdown(ctx context.Context, udid string) error { f.shutdowns++; return nil }

func TestResolveNoDevicesGivesRemediation(t *testing.T) {
	f := &fake{lists: [][]Dev{{}}}
	_, err := Resolve(context.Background(), f, "")
	if err == nil || !strings.Contains(err.Error(), "no iOS device is connected") {
		t.Fatalf("want no-device remediation, got %v", err)
	}
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindNotFound {
		t.Errorf("want KindNotFound typed error, got %#v", err)
	}
}

func TestResolvePicksTheOnlyDevice(t *testing.T) {
	f := &fake{lists: [][]Dev{{{UDID: "A1B2", Transport: TransportUSB}}}}
	d, err := Resolve(context.Background(), f, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.UDID != "A1B2" || d.Transport != TransportUSB {
		t.Errorf("resolved wrong device: %+v", d)
	}
}

func TestResolveUDIDWinsAmongMany(t *testing.T) {
	f := &fake{lists: [][]Dev{
		{{UDID: "AAAA"}, {UDID: "BBBB"}, {UDID: "CCCC", Transport: TransportNetwork}},
	}}
	d, err := Resolve(context.Background(), f, "BBBB")
	if err != nil {
		t.Fatal(err)
	}
	if d.UDID != "BBBB" {
		t.Errorf("got %q", d.UDID)
	}
}

func TestResolveAmbiguityListsCandidates(t *testing.T) {
	f := &fake{lists: [][]Dev{{{UDID: "AAAA", Transport: TransportUSB}, {UDID: "BBBB", Transport: TransportNetwork}}}}
	_, err := Resolve(context.Background(), f, "")
	if err == nil {
		t.Fatal("want ambiguity error")
	}
	for _, want := range []string{"2 devices are connected", "AAAA", "BBBB", "Network"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error missing %q: %v", want, err)
		}
	}
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindUsage || ExitCode(err) != 64 {
		t.Errorf("want KindUsage/64, got %#v (code %d)", de, ExitCode(err))
	}
}

func TestResolveUnknownUDID(t *testing.T) {
	f := &fake{lists: [][]Dev{{{UDID: "AAAA"}}}}
	_, err := Resolve(context.Background(), f, "ZZZZ")
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindNotFound {
		t.Fatalf("want unknown-UDID typed as NotFound, got %v", err)
	}
}

func TestExitCodeTaxonomy(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errors.New("plain"), 1},
		{&Error{Kind: KindUsage}, 64},
		{&Error{Kind: KindNotFound}, 65},
		{&Error{Kind: KindPermission}, 67},
		{&Error{Kind: KindTimeout}, 68},
		{&Error{Kind: KindConnection}, 69},
		{&Error{Kind: KindUnsupported}, 66},
		{&Error{Kind: KindInternal}, 70},
		{fmtWrap(&Error{Kind: KindNotFound}), 65},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func fmtWrap(err error) error { return err }

func TestWrapperCarriesOperationAndCause(t *testing.T) {
	g := New(Options{})
	got := g.wrap(KindConnection, "install", "replug the cable", io.ErrUnexpectedEOF)
	var de *Error
	if !errors.As(got, &de) || de.Kind != KindConnection || de.Op != "install" || de.Err != io.ErrUnexpectedEOF {
		t.Fatalf("bad wrap: %#v", got)
	}
	want := "replug the cable"
	if !strings.Contains(got.Error(), want) {
		t.Errorf("remediation %q missing from %q", want, got.Error())
	}
}

func TestWrapPairErrMapsTrustDialog(t *testing.T) {
	g := New(Options{})
	err := g.wrapPairErr(errors.New("Please accept the PairingDialog on the device and run pairing again!"))
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindPermission {
		t.Fatalf("want Permission from PairingDialog, got %v", err)
	}
	if !strings.Contains(err.Error(), "Trust") {
		t.Errorf("remediation should tell the user to tap Trust: %v", err)
	}
	if ExitCode(err) != 67 {
		t.Errorf("pairing dialog should exit 67, got %d", ExitCode(err))
	}
}

func TestWrapPairErrMapsLockedDevice(t *testing.T) {
	g := New(Options{})
	err := g.wrapPairErr(errors.New("some wrapping of device is locked"))
	var de *Error
	if !errors.As(err, &de) || strings.TrimSpace(err.Error()) == "" {
		t.Fatalf("expected a permission error, got %v", err)
	}
}

func TestPairSupervisedWithRule_SendsP12(t *testing.T) {
	f := &fake{}
	var h Handler = f
	sup := &Supervised{P12: []byte("p12-bytes"), Password: "pw"}
	if err := h.Pair(context.Background(), "AAAA", sup); err != nil {
		t.Fatalf("fake pair returned %v", err)
	}
	if len(f.pairCalls) != 1 || f.pairCalls[0].udid != "AAAA" || string(f.pairCalls[0].sup.P12) != "p12-bytes" {
		t.Errorf("pair call not forwarded: %+v", f.pairCalls)
	}
}

func TestBatchOfOps(t *testing.T) {
	f := &fake{infoVal: Info{Name: "Phone", ProductVersion: "18.1"}, launchPID: 42}
	var h Handler = f
	info, err := h.Info(context.Background(), "A")
	if err != nil || info.Name != "Phone" {
		t.Fatalf("info: %v %+v", err, info)
	}
	pid, err := h.Launch(context.Background(), "A", "com.x.app", map[string]string{"DYLD_FOO": "1"}, []string{"-debug"})
	if err != nil || pid != 42 {
		t.Fatalf("launch: %v pid=%d", err, pid)
	}
	if f.launchCalls[0].env["DYLD_FOO"] != "1" || len(f.launchCalls[0].args) != 1 {
		t.Errorf("launch args not forwarded: %+v", f.launchCalls[0])
	}
	var buf bytes.Buffer
	if err := h.Syslog(context.Background(), "A", &buf, LogFilter{}); err != nil {
		t.Fatal(err)
	}
}

// TestResolveSameUDIDTwoTransportsIsOneDevice — the bug from the field:
// usbmuxd lists one phone twice (USB + Network); that must count as ONE
// device, resolved without --udid, USB preferred.
func TestResolveSameUDIDTwoTransportsIsOneDevice(t *testing.T) {
	f := &fake{lists: [][]Dev{{
		{UDID: "AAAA", Transport: TransportNetwork},
		{UDID: "AAAA", Transport: TransportUSB},
	}}}
	d, err := Resolve(context.Background(), f, "")
	if err != nil {
		t.Fatalf("one phone on two transports must resolve without --udid: %v", err)
	}
	if d.UDID != "AAAA" || d.Transport != TransportUSB {
		t.Errorf("want the USB face, got %+v", d)
	}
}

func TestResolveDistinctUDIDsStillAmbiguous(t *testing.T) {
	f := &fake{lists: [][]Dev{{
		{UDID: "AAAA", Transport: TransportUSB},
		{UDID: "AAAA", Transport: TransportNetwork},
		{UDID: "BBBB", Transport: TransportUSB},
	}}}
	_, err := Resolve(context.Background(), f, "")
	if err == nil || !strings.Contains(err.Error(), "2 devices are connected") {
		t.Fatalf("two real phones must stay ambiguous: %v", err)
	}
	if !strings.Contains(err.Error(), "BBBB") || strings.Count(err.Error(), "AAAA") > 1 {
		t.Errorf("candidates should be one row per phone: %v", err)
	}
}

func TestDistinctOrderStableAndDedupe(t *testing.T) {
	got := Distinct([]Dev{
		{UDID: "B", Transport: TransportUSB},
		{UDID: "A", Transport: TransportNetwork},
		{UDID: "B", Transport: TransportNetwork},
	})
	if len(got) != 2 || got[0].UDID != "B" || got[0].Transport != TransportUSB || got[1].UDID != "A" {
		t.Errorf("distinct wrong: %+v", got)
	}
}
