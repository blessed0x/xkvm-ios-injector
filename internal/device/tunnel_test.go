package device

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc is a tunnelProc stand-in that never touches the process tree.
type fakeProc struct {
	mu     sync.Mutex
	exited error // nil while "running"
	killed bool
	out    string
}

func (p *fakeProc) exitErr() (error, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited != nil || p.exited == errFakeExit {
		return p.exited, true
	}
	return nil, false
}

// errFakeExit marks a proc that should report as exited with no error.
var errFakeExit = errors.New("fake exit")

func (p *fakeProc) kill()          { p.killed = true }
func (p *fakeProc) output() string { return p.out }

// finish marks the proc exited with err (nil = clean exit).
func (p *fakeProc) finish(err error) {
	p.mu.Lock()
	p.exited = err
	p.mu.Unlock()
}

// tunnelServer serves /tunnel/<udid> like go-ios's tunnel-info endpoint,
// publishing only after publish is closed (simulating tunnel setup time).
type tunnelServer struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	entries map[string]publishedTunnel
}

func mustAtoi(t testing.TB, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("bad port %q: %v", s, err)
	}
	return n
}

func newTunnelServer(t *testing.T) *tunnelServer {
	ts := &tunnelServer{t: t, entries: map[string]publishedTunnel{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel/", func(w http.ResponseWriter, r *http.Request) {
		udid := strings.TrimPrefix(r.URL.Path, "/tunnel/")
		ts.mu.Lock()
		entry, ok := ts.entries[udid]
		ts.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entry)
	})
	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tunnelServer) publish(udid string) {
	ts.mu.Lock()
	ts.entries[udid] = publishedTunnel{
		Address: "fe80::1", RsdPort: 54321,
		UserspaceTUN: true, UserspaceTUNPort: 60106,
	}
	ts.mu.Unlock()
}

func (ts *tunnelServer) hostPort() (func() string, func() int) {
	addr := strings.TrimPrefix(ts.srv.URL, "http://")
	i := strings.LastIndex(addr, ":")
	host, port := addr[:i], mustAtoi(ts.t, addr[i+1:])
	return func() string { return host }, func() int { return port }
}

const testUDID = "0000000000000000000deadbeef000000000dead"

func newTestManager(ts *tunnelServer) *tunnelManager {
	m := newTunnelManager()
	h, p := ts.hostPort()
	m.apiHost, m.apiPort = h, p
	m.budget, m.poll = 2*time.Second, 10*time.Millisecond
	return m
}

func TestInfoDownWhenNoEndpoint(t *testing.T) {
	m := newTunnelManager()
	// Point at a port with nothing listening.
	m.apiHost, m.apiPort = func() string { return "127.0.0.1" }, func() int { return 1 }
	if _, ok := m.info(testUDID); ok {
		t.Fatal("info must be false when no endpoint answers")
	}
}

func TestEnsureDiscoversExistingTunnel(t *testing.T) {
	ts := newTunnelServer(t)
	ts.publish(testUDID)
	spawned := false
	m := newTestManager(ts)
	m.start = func(ctx context.Context, udid string) (tunnelProc, error) {
		spawned = true
		return &fakeProc{}, nil
	}
	if err := m.ensure(context.Background(), testUDID); err != nil {
		t.Fatalf("ensure over an existing tunnel: %v", err)
	}
	if spawned {
		t.Fatal("an already-published tunnel must not spawn a process")
	}
}

func TestEnsureSpawnsAndWaitsForPublish(t *testing.T) {
	ts := newTunnelServer(t)
	m := newTestManager(ts)
	var spawnedFor string
	m.start = func(ctx context.Context, udid string) (tunnelProc, error) {
		spawnedFor = udid
		go func() {
			time.Sleep(100 * time.Millisecond)
			ts.publish(udid)
		}()
		return &fakeProc{}, nil
	}
	if err := m.ensure(context.Background(), testUDID); err != nil {
		t.Fatalf("ensure after spawn: %v", err)
	}
	if spawnedFor != testUDID {
		t.Fatalf("spawn targeted %q, want %q", spawnedFor, testUDID)
	}
}

func TestEnsureEarlyExitSurfacesOutput(t *testing.T) {
	ts := newTunnelServer(t)
	m := newTestManager(ts)
	m.start = func(ctx context.Context, udid string) (tunnelProc, error) {
		p := &fakeProc{out: "go: module download failed"}
		p.finish(errors.New("exit status 1"))
		return p, nil
	}
	err := m.ensure(context.Background(), testUDID)
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindConnection {
		t.Fatalf("want KindConnection typed error, got %#v", err)
	}
	if !strings.Contains(err.Error(), "module download failed") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("error must carry the process output and cause, got: %v", err)
	}
	// The failed proc must be forgotten so a later attempt can respawn.
	m.mu.Lock()
	_, stillTracked := m.running[testUDID]
	m.mu.Unlock()
	if stillTracked {
		t.Fatal("exited tunnel proc must not stay in the running set")
	}
}

func TestEnsureDisabledReportsKillSwitch(t *testing.T) {
	ts := newTunnelServer(t)
	m := newTestManager(ts)
	m.disabled = func() bool { return true }
	err := m.ensure(context.Background(), testUDID)
	if !errors.Is(err, errAutoTunnelDisabled) {
		t.Fatalf("want errAutoTunnelDisabled, got %v", err)
	}
}

func TestEnsureTimeoutKillsProcess(t *testing.T) {
	ts := newTunnelServer(t) // never publishes for testUDID
	m := newTestManager(ts)
	p := &fakeProc{}
	m.start = func(ctx context.Context, udid string) (tunnelProc, error) { return p, nil }
	m.budget, m.poll = 60*time.Millisecond, 5*time.Millisecond
	err := m.ensure(context.Background(), testUDID)
	var de *Error
	if !errors.As(err, &de) || de.Kind != KindTimeout {
		t.Fatalf("want KindTimeout, got %#v", err)
	}
	if !p.killed {
		t.Fatal("timed-out tunnel process must be killed")
	}
}

func TestLimitedBufferKeepsTail(t *testing.T) {
	b := newLimitedBuffer(16)
	if n, _ := b.Write([]byte(strings.Repeat("a", 40))); n != 40 {
		t.Fatalf("Write must consume everything, got %d", n)
	}
	got := b.String()
	// The rendered string may carry one multi-byte ellipsis marker on top
	// of the capped byte count.
	if len(got) > 16+3 {
		t.Fatalf("buffer grew past its cap: %q", got)
	}
	if !strings.HasSuffix(got, "aaaaaaaa") {
		t.Fatalf("buffer must keep the newest bytes, got %q", got)
	}
}

func TestLogFilterMatching(t *testing.T) {
	f := LogFilter{Process: "spring", Contains: "crash"}
	if !f.matches("SpringBoard", "app crashed now") {
		t.Fatal("process substring + message substring must both match case-insensitively on name")
	}
	if f.matches("backboardd", "app crashed now") {
		t.Fatal("wrong process must be filtered out")
	}
	if f.matches("SpringBoard", "all quiet") {
		t.Fatal("missing Contains must be filtered out")
	}
	zero := LogFilter{}
	if !zero.matches("", "") {
		t.Fatal("the zero filter passes everything")
	}
}

func TestManualRemediationNamesTheCommand(t *testing.T) {
	r := manualTunnelRemediation("ABC123")
	for _, want := range []string{"go run", goIOSModuleRef, "tunnel start --userspace", "--udid ABC123"} {
		if !strings.Contains(r, want) {
			t.Fatalf("remediation must mention %q, got:\n%s", want, r)
		}
	}
}

func TestShouldAutoTunnelVersionGate(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"17.0", true},
		{"17.5.1", true},
		{"26.1", true},    // live-verified line: tunnel is the only path
		{"16.7.1", false}, // DDI territory: a spawned tunnel never publishes
		{"15.0", false},
		{"", true}, // unknown device answer: attempt beats a wrong "no"
		{"not a version", true},
	}
	for _, tc := range cases {
		if got := shouldAutoTunnel(tc.version); got != tc.want {
			t.Errorf("shouldAutoTunnel(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}
