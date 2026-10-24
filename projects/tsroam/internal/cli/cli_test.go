package cli

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/agent"
	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/killswitch"
	"github.com/ganeshphutane-g/tsroam/internal/policy"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
	"github.com/ganeshphutane-g/tsroam/internal/tailscale"
)

type machine struct {
	mu  sync.Mutex
	mac net.HardwareAddr
}

var homeMAC = net.HardwareAddr{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}

func (m *machine) DefaultRoute() (string, int, netip.Addr, bool, error) {
	return "wlan0", 2, netip.MustParseAddr("192.168.1.1"), true, nil
}
func (m *machine) Connected(p netip.Prefix) (string, int, netip.Prefix, error) {
	n := netip.MustParsePrefix("192.168.1.0/24")
	if !n.Contains(p.Addr()) || n.Bits() > p.Bits() {
		return "", 0, netip.Prefix{}, nil
	}
	return "wlan0", 2, n, nil
}
func (m *machine) DefaultGateway(int) (netip.Addr, bool) {
	return netip.MustParseAddr("192.168.1.1"), true
}
func (m *machine) GatewayMAC(context.Context, string, netip.Addr) (net.HardwareAddr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mac == nil {
		return nil, sys.ErrNoAnswer
	}
	return m.mac, nil
}
func (m *machine) Probe(context.Context, string, netip.AddrPort) error { return nil }

type tailnet struct {
	mu    sync.Mutex
	exit  string
	calls []string
}

func (t *tailnet) Status(context.Context) (tailscale.Status, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return tailscale.Status{State: "Running", ExitNodeID: t.exit, Peers: []tailscale.Peer{{ID: "1", HostName: "sr", DNSName: "sr.lab.example.", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}, Online: true}}}, nil
}
func (t *tailnet) SetExitNode(_ context.Context, node string, _ bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, "set "+node)
	t.exit = map[string]string{"": "", "sr": "1", "100.64.0.1": "1"}[node]
	return nil
}

type wall struct {
	mu sync.Mutex
	on bool
}

func (w *wall) On(killswitch.Rules) error { w.mu.Lock(); w.on = true; w.mu.Unlock(); return nil }
func (w *wall) Off() error                { w.mu.Lock(); w.on = false; w.mu.Unlock(); return nil }
func (w *wall) Active() (bool, error)     { w.mu.Lock(); defer w.mu.Unlock(); return w.on, nil }
func (w *wall) Intact(killswitch.Rules) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.on, nil
}

type world struct {
	m    *machine
	ts   *tailnet
	w    *wall
	dir  string
	conf string
}

func newWorld(t *testing.T, conf string) *world {
	w := &world{m: &machine{}, ts: &tailnet{}, w: &wall{}, dir: t.TempDir()}
	w.conf = filepath.Join(w.dir, "tsroam.conf")
	os.WriteFile(w.conf, []byte(conf), 0o644)
	return w
}

func (w *world) deps() Deps {
	return Deps{
		System:  func() policy.System { return w.m },
		Tailnet: func(string, string) agent.Tailnet { return w.ts },
		Wall:    func() agent.Wall { return w.w },
		Watch:   func(ctx context.Context) (<-chan struct{}, error) { return make(chan struct{}), nil },
		Signals: func(chan<- os.Signal) {},
	}
}

func (w *world) cmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	if args[0] != "learn" && args[0] != "version" {
		args = append(args, "--config", w.conf, "--state-dir", w.dir)
	}
	code := Main(args, &out, &errb, w.deps())
	return code, out.String(), errb.String()
}

const conf = "exit-node sr\nkill-switch on\ntrusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56\n"

func TestUsage(t *testing.T) {
	w := newWorld(t, conf)
	var out, errb bytes.Buffer
	if code := Main(nil, &out, &errb, w.deps()); code != exitUsage || !strings.Contains(errb.String(), "tsroam off") {
		t.Errorf("%d %q", code, errb.String())
	}
	if code, out, _ := w.cmd("version"); code != 0 || !strings.HasPrefix(out, "tsroam ") {
		t.Errorf("%d %q", code, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "--interval", "1ms"}, "at least 1s"},
		{[]string{"off", "--for", "1s"}, "from 1m to 24h"},
		{[]string{"check", "--tun", ""}, "not an interface name"},
		{[]string{"check", "--ts-mark", "0x80000/0x100"}, "inside the mask"},
		{[]string{"check", "extra"}, `unexpected "extra"`},
	} {
		if code, _, e := w.cmd(c.args...); code != exitUsage || !strings.Contains(e, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, e, c.want)
		}
	}
}

func TestNoExitNode(t *testing.T) {
	w := newWorld(t, "kill-switch on\n")
	if code, _, e := w.cmd("check"); code != exitError || !strings.Contains(e, "no exit node") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, e := w.cmd("check", "--exit-node", "-x"); code != exitError || !strings.Contains(e, "is not a node") {
		t.Errorf("%d %q", code, e)
	}
}

func TestCheck(t *testing.T) {
	w := newWorld(t, conf)
	code, out, _ := w.cmd("check")
	if code != exitUntrusted || !strings.Contains(out, "network:     untrusted") || !strings.Contains(out, "run would:   exit node sr on, kill switch on") {
		t.Errorf("café: %d\n%s", code, out)
	}
	if len(w.ts.calls) != 0 || w.w.on {
		t.Errorf("check changed things: %v %v", w.ts.calls, w.w.on)
	}
	w.m.mac = homeMAC
	if code, out, _ := w.cmd("check"); code != exitOK || !strings.Contains(out, "network:     trusted") {
		t.Errorf("home: %d\n%s", code, out)
	}
}

func TestOffAndCleanup(t *testing.T) {
	w := newWorld(t, conf)
	w.w.on, w.ts.exit = true, "1"
	code, out, _ := w.cmd("off", "--for", "30m")
	if code != exitOK || w.w.on || w.ts.exit != "" || !strings.Contains(out, "until it changes or") {
		t.Fatalf("%d %q %v %q", code, out, w.w.on, w.ts.exit)
	}
	p, ok := agent.ReadPause(filepath.Join(w.dir, "pause"))
	if !ok || !strings.HasPrefix(p.Key, "untrusted wlan0 192.168.1.1") || time.Until(p.Until) < 29*time.Minute {
		t.Fatalf("pause %+v %v", p, ok)
	}
	// check sees the pause.
	if _, out, _ := w.cmd("check"); !strings.Contains(out, "nothing: paused by tsroam off") {
		t.Errorf("check:\n%s", out)
	}
	w.w.on = true
	if code, _, _ := w.cmd("cleanup"); code != exitOK || w.w.on {
		t.Errorf("cleanup: %d %v", code, w.w.on)
	}
	if _, ok := agent.ReadPause(filepath.Join(w.dir, "pause")); ok {
		t.Error("the pause is still there")
	}
}

func TestLearn(t *testing.T) {
	w := newWorld(t, conf)
	w.m.mac = homeMAC
	var out, errb bytes.Buffer
	if code := Main([]string{"learn"}, &out, &errb, w.deps()); code != exitOK || out.String() != "trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56\n" {
		t.Errorf("%d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Main([]string{"learn", "192.168.1.0/24", "--probe", "192.168.1.5:22"}, &out, &errb, w.deps()); code != exitOK || out.String() != "trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22\n" {
		t.Errorf("%d %q", code, out.String())
	}
	errb.Reset()
	if code := Main([]string{"learn", "10.0.0.0/8"}, &out, &errb, w.deps()); code != exitError || !strings.Contains(errb.String(), "not on 10.0.0.0/8") {
		t.Errorf("%d %q", code, errb.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRun(t *testing.T) {
	w := newWorld(t, conf)
	d := w.deps()
	sig := make(chan chan<- os.Signal, 1)
	d.Signals = func(c chan<- os.Signal) { sig <- c }
	var out, errb syncBuffer
	done := make(chan int)
	go func() {
		done <- Main([]string{"run", "--config", w.conf, "--state-dir", w.dir}, &out, &errb, d)
	}()
	c := <-sig
	wait := func(s string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), s); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q:\n%s%s", s, out.String(), errb.String())
			}
		}
	}
	wait("exit node on: sr")
	// A reload that fails keeps the config; a good one is used.
	os.WriteFile(w.conf, []byte("kill-switch maybe\n"), 0o644)
	c <- syscall.SIGHUP
	wait("keeping the config as it was")
	os.WriteFile(w.conf, []byte("exit-node 100.64.0.1\n"), 0o644)
	c <- syscall.SIGHUP
	wait("kill switch off")
	c <- syscall.SIGTERM
	if code := <-done; code != exitOK || !strings.Contains(out.String(), "stopped") {
		t.Errorf("exit %d\n%s", code, out.String())
	}
}

func TestRunStopsWhenEventsStop(t *testing.T) {
	w := newWorld(t, conf)
	d := w.deps()
	events := make(chan struct{})
	d.Watch = func(ctx context.Context) (<-chan struct{}, error) { return events, nil }
	var out, errb syncBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"run", "--config", w.conf, "--state-dir", w.dir}, &out, &errb, d) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "kill switch on"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(out.String())
		}
	}
	close(events)
	if code := <-done; code != exitError || !w.w.on {
		t.Errorf("exit %d, kill switch %v (it must stay on an error exit)", code, w.w.on)
	}
}

func TestMetrics(t *testing.T) {
	w := newWorld(t, conf)
	a := &agent.Agent{Sys: w.m, TS: w.ts, Wall: w.w, Rules: killswitch.Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000}}
	a.SetConfig(mustConfig(t, conf))
	h := handler(a, 15*time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Errorf("healthz before a pass: %d", rec.Code)
	}
	a.Pass(context.Background())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{`tsroam_network{kind="untrusted"} 1`, "tsroam_exit_node 1", "tsroam_kill_switch 1", "tsroam_changes_total 2"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, rec.Body)
		}
	}
}

func mustConfig(t *testing.T, s string) config.Config {
	c, err := config.Read(strings.NewReader(s), "t")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
