package agent

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/killswitch"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
	"github.com/ganeshphutane-g/tsroam/internal/tailscale"
)

// machine is the network side: one interface on one network.
type machine struct {
	mu      sync.Mutex
	iface   string
	net     netip.Prefix
	gateway netip.Addr
	mac     net.HardwareAddr
}

func (m *machine) DefaultRoute() (string, int, netip.Addr, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.iface, 2, m.gateway, m.iface != "", nil
}

func (m *machine) Connected(p netip.Prefix) (string, int, netip.Prefix, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.iface == "" || !m.net.Contains(p.Addr()) || m.net.Bits() > p.Bits() {
		return "", 0, netip.Prefix{}, nil
	}
	return m.iface, 2, m.net, nil
}

func (m *machine) DefaultGateway(int) (netip.Addr, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gateway, m.gateway.IsValid()
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

func (m *machine) set(f func(*machine)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
}

// tailnet is tailscaled.
type tailnet struct {
	mu      sync.Mutex
	state   string
	exit    string // the exit node ID in use
	calls   []string
	lanFlag []bool
}

func (t *tailnet) Status(context.Context) (tailscale.Status, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return tailscale.Status{State: t.state, ExitNodeID: t.exit, Peers: []tailscale.Peer{
		{ID: "1", HostName: "sr", IPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}, ExitNodeOption: true, Online: true},
	}}, nil
}

func (t *tailnet) SetExitNode(_ context.Context, node string, allowLAN bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, "set "+node)
	t.lanFlag = append(t.lanFlag, allowLAN)
	switch node {
	case "":
		t.exit = ""
	case "100.64.0.1", "sr":
		t.exit = "1"
	}
	return nil
}

func (t *tailnet) set(f func(*tailnet)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(t)
}

func (t *tailnet) n() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// wall is the kill switch.
type wall struct {
	mu     sync.Mutex
	on     bool
	rules  killswitch.Rules
	events []string
}

func (w *wall) On(r killswitch.Rules) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.on, w.rules = true, r
	w.events = append(w.events, "on")
	return nil
}

func (w *wall) Off() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.on = false
	w.events = append(w.events, "off")
	return nil
}

func (w *wall) Active() (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.on, nil
}

var (
	homeMAC = net.HardwareAddr{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}
	cafeMAC = net.HardwareAddr{2, 0, 0, 0, 0, 9}
)

func home() *machine {
	return &machine{iface: "wlan0", net: netip.MustParsePrefix("192.168.1.0/24"), gateway: netip.MustParseAddr("192.168.1.1"), mac: homeMAC}
}

func cafe() *machine {
	m := home()
	m.mac = cafeMAC // the same addresses, another router
	return m
}

type rig struct {
	a   *Agent
	m   *machine
	ts  *tailnet
	w   *wall
	log *bytes.Buffer
	now time.Time
}

func newRig(t *testing.T, m *machine, conf string) *rig {
	c, err := config.Read(strings.NewReader(conf), "test")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{m: m, ts: &tailnet{state: "Running"}, w: &wall{}, log: &bytes.Buffer{}, now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	r.a = &Agent{Sys: m, TS: r.ts, Wall: r.w, Config: c, Log: r.log,
		Rules:     killswitch.Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000},
		PauseFile: filepath.Join(t.TempDir(), "pause"),
		Now:       func() time.Time { return r.now }}
	return r
}

const conf = `exit-node 100.64.0.1
kill-switch on
trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
`

func (r *rig) pass() State { return r.a.Pass(context.Background()) }

func TestCafe(t *testing.T) {
	r := newRig(t, cafe(), conf)
	st := r.pass()
	if st.Place.Kind != "untrusted" || !st.ExitNode || !st.Wall || st.Note != "" {
		t.Fatalf("%+v", st)
	}
	if strings.Join(r.ts.calls, ",") != "set 100.64.0.1" || r.ts.lanFlag[0] {
		t.Errorf("calls %v %v", r.ts.calls, r.ts.lanFlag)
	}
	if r.w.rules.LAN.IsValid() {
		t.Errorf("the café LAN is allowed: %+v", r.w.rules)
	}
	r.pass()
	if r.ts.n() != 1 || len(r.w.events) != 1 {
		t.Errorf("a second pass changed things: %v %v", r.ts.calls, r.w.events)
	}
	for _, want := range []string{"kill switch on", "exit node on: 100.64.0.1", "untrusted: the network on wlan0 is not a trusted one"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, r.log)
		}
	}
}

func TestCafeKeepsTheExitNodeOn(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	r.ts.set(func(t *tailnet) { t.exit = "" }) // someone turned it off
	if st := r.pass(); !st.ExitNode || r.ts.n() != 2 {
		t.Errorf("%+v %v", st, r.ts.calls)
	}
	r.w.Off() // someone removed the table
	if st := r.pass(); !st.Wall {
		t.Errorf("kill switch not restored: %+v", st)
	}
}

func TestHome(t *testing.T) {
	r := newRig(t, home(), conf)
	r.ts.exit = "1" // left on from the café
	r.w.on = true
	st := r.pass()
	if st.Place.Kind != "trusted" || st.ExitNode || st.Wall {
		t.Fatalf("%+v", st)
	}
	if strings.Join(r.ts.calls, ",") != "set " {
		t.Errorf("calls %v", r.ts.calls)
	}
	// At home the user may choose an exit node: not undone.
	r.ts.set(func(t *tailnet) { t.exit = "1" })
	r.pass()
	if r.ts.n() != 1 {
		t.Errorf("tsroam fought the user at home: %v", r.ts.calls)
	}
}

func TestMovingHomeAndAway(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	r.m.set(func(m *machine) { m.mac = homeMAC })
	if st := r.pass(); st.Wall || st.ExitNode {
		t.Fatalf("home: %+v", st)
	}
	r.m.set(func(m *machine) { m.mac = cafeMAC })
	if st := r.pass(); !st.Wall || !st.ExitNode {
		t.Fatalf("café again: %+v", st)
	}
	if strings.Join(r.ts.calls, ",") != "set 100.64.0.1,set ,set 100.64.0.1" {
		t.Errorf("calls %v", r.ts.calls)
	}
}

func TestOfflineChangesNothing(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	r.m.set(func(m *machine) { m.iface = "" })
	if st := r.pass(); st.Place.Kind != "offline" || !st.Wall {
		t.Fatalf("%+v", st)
	}
	if r.ts.n() != 1 || len(r.w.events) != 1 {
		t.Errorf("%v %v", r.ts.calls, r.w.events)
	}
}

func TestTailscaledDown(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.ts.state = "Stopped"
	st := r.pass()
	if !st.Wall || st.ExitNode || !strings.Contains(st.Note, "tailscaled is in state Stopped") {
		t.Fatalf("%+v", st)
	}
	r.ts.set(func(t *tailnet) { t.state = "Running" })
	if st := r.pass(); !st.ExitNode {
		t.Fatalf("%+v", st)
	}
}

func TestArrivingHomeWhileTailscaledIsDown(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	r.ts.set(func(t *tailnet) { t.state = "Stopped" })
	r.m.set(func(m *machine) { m.mac = homeMAC })
	r.pass()
	r.ts.set(func(t *tailnet) { t.state = "Running" }) // it comes back with the exit node set
	if st := r.pass(); st.ExitNode {
		t.Fatalf("exit node left on at home: %+v %v", st, r.ts.calls)
	}
}

func TestPause(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	// "tsroam off" at the café: both off, a pause for this network.
	r.w.Off()
	r.ts.SetExitNode(context.Background(), "", false)
	if err := WritePause(r.a.PauseFile, Pause{Key: r.a.Last().Place.Key(), Until: r.now.Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	calls := r.ts.n()
	if st := r.pass(); !st.Paused || st.Wall || st.ExitNode || r.ts.n() != calls {
		t.Fatalf("paused: %+v %v", st, r.ts.calls)
	}
	// It runs out.
	r.now = r.now.Add(11 * time.Minute)
	if st := r.pass(); st.Paused || !st.Wall || !st.ExitNode {
		t.Fatalf("after the pause: %+v", st)
	}
	if _, ok := ReadPause(r.a.PauseFile); ok {
		t.Error("the pause file is still there")
	}
	// A pause ends when the network changes.
	WritePause(r.a.PauseFile, Pause{Key: r.a.Last().Place.Key(), Until: r.now.Add(time.Hour)})
	r.m.set(func(m *machine) { m.gateway = netip.MustParseAddr("192.168.1.254") })
	if st := r.pass(); st.Paused || !strings.Contains(r.log.String(), "pause over: the network changed") {
		t.Fatalf("%+v\n%s", st, r.log)
	}
}

func TestAllowLAN(t *testing.T) {
	r := newRig(t, cafe(), conf+"allow-lan on\n")
	r.pass()
	if r.w.rules.LAN.String() != "192.168.1.0/24" || !r.ts.lanFlag[0] {
		t.Fatalf("rules %+v, lan flag %v", r.w.rules, r.ts.lanFlag)
	}
	// Another café, another LAN: the table is replaced.
	r.m.set(func(m *machine) {
		m.net, m.gateway = netip.MustParsePrefix("10.20.0.0/16"), netip.MustParseAddr("10.20.0.1")
	})
	r.pass()
	if r.w.rules.LAN.String() != "10.20.0.0/16" || !strings.Contains(r.log.String(), "kill switch updated") {
		t.Errorf("rules %+v\n%s", r.w.rules, r.log)
	}
}

func TestNoKillSwitch(t *testing.T) {
	r := newRig(t, cafe(), "exit-node sr\n")
	if st := r.pass(); st.Wall || !st.ExitNode {
		t.Fatalf("%+v", st)
	}
}

func TestUnknownExitNode(t *testing.T) {
	r := newRig(t, cafe(), "exit-node nosuch\nkill-switch on\n")
	st := r.pass()
	if !st.Wall || st.ExitNode || !strings.Contains(st.Note, `exit node "nosuch" is not in the tailnet`) {
		t.Fatalf("%+v", st)
	}
}

func TestDryRun(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.a.DryRun = true
	st := r.pass()
	if st.Wall || st.ExitNode || r.ts.n() != 0 || len(r.w.events) != 0 {
		t.Fatalf("%+v %v %v", st, r.ts.calls, r.w.events)
	}
}

func TestLogsOncePerChange(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.ts.state = "Stopped"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r.a.Run(ctx, 10*time.Millisecond, nil) // about 10 passes
	if n := strings.Count(r.log.String(), "note: tailscaled is in state Stopped"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, r.log)
	}
	if n := strings.Count(r.log.String(), "untrusted:"); n != 1 {
		t.Errorf("place logged %d times:\n%s", n, r.log)
	}
}

func TestStop(t *testing.T) {
	r := newRig(t, cafe(), conf)
	r.pass()
	if err := r.a.Stop(); err != nil || r.w.on {
		t.Fatalf("%v %v", err, r.w.on)
	}
	if r.ts.exit != "1" {
		t.Error("the exit node was changed on stop")
	}
}
