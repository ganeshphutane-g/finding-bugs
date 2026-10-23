package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/tsdirect/internal/config"
	"github.com/ganeshphutane-g/tsdirect/internal/sys"
)

// fake is a machine: one interface, maybe on a network, with a gateway.
type fake struct {
	mu        sync.Mutex
	iface     string
	connected netip.Prefix
	gateway   netip.Addr
	mac       net.HardwareAddr
	macErr    error
	probeErr  error
	rules     map[sys.Rule]bool
	asked     []string // what was looked at, in order
	addErr    error
}

func (f *fake) Connected(p netip.Prefix) (string, int, netip.Prefix, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.iface == "" || !f.connected.Contains(p.Addr()) {
		return "", 0, netip.Prefix{}, nil
	}
	return f.iface, 2, f.connected, nil
}

func (f *fake) DefaultGateway(index int) (netip.Addr, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gateway, f.gateway.IsValid()
}

func (f *fake) GatewayMAC(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "mac "+addr.String())
	if f.macErr != nil {
		return nil, f.macErr
	}
	if f.mac == nil {
		return nil, sys.ErrNoAnswer
	}
	return f.mac, nil
}

func (f *fake) Probe(ctx context.Context, iface string, ap netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "probe "+ap.String())
	return f.probeErr
}

func (f *fake) Rules() ([]sys.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sys.Rule
	for r := range f.rules {
		out = append(out, r)
	}
	return out, nil
}

func (f *fake) AddRule(r sys.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	if f.rules == nil {
		f.rules = map[sys.Rule]bool{}
	}
	f.rules[r] = true
	return nil
}

func (f *fake) DelRule(r sys.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, r)
	return nil
}

func (f *fake) set(change func(*fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func lan(t *testing.T, s string) config.LAN {
	t.Helper()
	l, err := config.Parse(s, "test")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var (
	homeMAC = net.HardwareAddr{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}
	cafeMAC = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x99}
	home    = netip.MustParsePrefix("192.168.1.0/24")
	rule    = sys.Rule{Prefix: home, Priority: 5200}
)

func atHome() *fake {
	return &fake{iface: "wlan0", connected: home, gateway: netip.MustParseAddr("192.168.1.1"), mac: homeMAC}
}

func newAgent(t *testing.T, f *fake, lans ...string) (*Agent, *bytes.Buffer) {
	var log bytes.Buffer
	a := &Agent{Sys: f, Priority: 5200, Log: &log}
	for _, s := range lans {
		a.LANs = append(a.LANs, lan(t, s))
	}
	return a, &log
}

func TestHomeAddsTheRuleOnce(t *testing.T) {
	f := atHome()
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	for range 3 {
		if err := a.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.rules) != 1 || !f.rules[rule] {
		t.Fatalf("rules = %v, want just %v", f.rules, rule)
	}
	if n := strings.Count(log.String(), "added rule to 192.168.1.0/24 lookup main priority 5200"); n != 1 {
		t.Errorf("logged %d adds, want 1:\n%s", n, log)
	}
	st := a.Statuses()
	if !st[0].Rule || !st[0].Verdict.On || st[0].Changes != 1 {
		t.Errorf("status = %+v", st[0])
	}
}

func TestCafeWithTheSameAddresses(t *testing.T) {
	f := atHome()
	f.mac = cafeMAC
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22")
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) != 0 {
		t.Fatalf("rules = %v on another network", f.rules)
	}
	// The probe is not sent onto a stranger's network.
	if strings.Join(f.asked, ",") != "mac 192.168.1.1" {
		t.Errorf("looked at %v", f.asked)
	}
	if !strings.Contains(log.String(), "02:00:00:00:00:99, not 3c:7c:3f:12:34:56") {
		t.Errorf("log does not say why:\n%s", log)
	}
}

func TestLeavingRemovesTheRule(t *testing.T) {
	f := atHome()
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	f.iface = "" // Wi-Fi gone
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.rules) != 0 {
		t.Fatalf("rules = %v after leaving", f.rules)
	}
	if !strings.Contains(log.String(), "removed rule to 192.168.1.0/24 lookup main priority 5200: not on 192.168.1.0/24") {
		t.Errorf("log:\n%s", log)
	}
	if st := a.Statuses(); st[0].Rule || st[0].Changes != 2 {
		t.Errorf("status = %+v", st[0])
	}
}

func TestGatewayGoesSilent(t *testing.T) {
	f := atHome()
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	f.mac = nil
	a.Reconcile(context.Background())
	if len(f.rules) != 0 {
		t.Fatalf("rules = %v with the gateway silent", f.rules)
	}
}

func TestAskingFailsIsOff(t *testing.T) {
	f := atHome()
	f.macErr = errors.New("ppp0 has no Ethernet address (ARP needs one)")
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	st := a.Statuses()[0]
	if st.Verdict.On || !strings.Contains(st.Verdict.Reason, "could not be asked: ppp0 has no Ethernet address") {
		t.Errorf("verdict = %+v", st.Verdict)
	}
}

func TestGivenGatewayIsAsked(t *testing.T) {
	f := atHome()
	f.gateway = netip.Addr{} // no default route on this interface
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway 192.168.1.254 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	if strings.Join(f.asked, ",") != "mac 192.168.1.254" || len(f.rules) != 1 {
		t.Errorf("asked %v, rules %v", f.asked, f.rules)
	}
}

func TestNoGatewayIsOff(t *testing.T) {
	f := atHome()
	f.gateway = netip.Addr{}
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	if len(f.rules) != 0 || len(f.asked) != 0 {
		t.Errorf("asked %v, rules %v", f.asked, f.rules)
	}
}

func TestProbe(t *testing.T) {
	f := atHome()
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 probe 192.168.1.5:22")
	a.Reconcile(context.Background())
	if len(f.rules) != 1 {
		t.Fatalf("rules = %v with the probe answering", f.rules)
	}
	f.probeErr = errors.New("connection refused")
	a.Reconcile(context.Background())
	if len(f.rules) != 0 {
		t.Fatalf("rules = %v with the probe failing", f.rules)
	}
	if r := a.Statuses()[0].Verdict.Reason; !strings.Contains(r, "probe 192.168.1.5:22 failed (connection refused)") {
		t.Errorf("reason = %q", r)
	}
}

func TestPartlyConnectedIsNotProbed(t *testing.T) {
	f := atHome()
	f.connected = netip.MustParsePrefix("192.168.1.0/25")
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 probe 192.168.1.5:22")
	a.Reconcile(context.Background())
	if len(f.rules) != 0 || len(f.asked) != 0 {
		t.Errorf("asked %v, rules %v", f.asked, f.rules)
	}
}

func TestStaleRulesAreRemoved(t *testing.T) {
	f := atHome()
	old := sys.Rule{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Priority: 5200}
	moved := sys.Rule{Prefix: home, Priority: 5100}
	f.rules = map[sys.Rule]bool{old: true, moved: true}
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	if len(f.rules) != 1 || !f.rules[rule] {
		t.Fatalf("rules = %v", f.rules)
	}
	for _, want := range []string{"removed rule to 10.9.0.0/16 lookup main priority 5200: not configured", "removed rule to 192.168.1.0/24 lookup main priority 5100: priority is now 5200"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestAfterACrashTheRuleIsKept(t *testing.T) {
	f := atHome()
	f.rules = map[sys.Rule]bool{rule: true} // left by the last run
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.Reconcile(context.Background())
	if len(f.rules) != 1 || strings.Contains(log.String(), "added") || strings.Contains(log.String(), "removed") {
		t.Errorf("rules %v, log:\n%s", f.rules, log)
	}
	if !a.Statuses()[0].Rule {
		t.Error("status does not show the rule")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	f := atHome()
	stale := sys.Rule{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Priority: 5200}
	f.rules = map[sys.Rule]bool{stale: true}
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.DryRun = true
	a.Reconcile(context.Background())
	if len(f.rules) != 1 || !f.rules[stale] {
		t.Errorf("rules = %v", f.rules)
	}
	if st := a.Statuses()[0]; !st.Verdict.On || st.Rule {
		t.Errorf("status = %+v", st)
	}
}

func TestAddErrorIsReported(t *testing.T) {
	f := atHome()
	f.addErr = errors.New("operation not permitted")
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	err := a.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "adding the rule for 192.168.1.0/24: operation not permitted") {
		t.Errorf("err = %v", err)
	}
	if a.Errors() != 1 || a.Statuses()[0].Rule {
		t.Errorf("errors %d, status %+v", a.Errors(), a.Statuses()[0])
	}
}

func TestVerdictChangesAreLoggedOnce(t *testing.T) {
	f := atHome()
	f.iface = ""
	a, log := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	for range 3 {
		a.Reconcile(context.Background())
	}
	if n := strings.Count(log.String(), "192.168.1.0/24: not on"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, log)
	}
}

func TestCleanup(t *testing.T) {
	f := atHome()
	f.rules = map[sys.Rule]bool{rule: true, {Prefix: netip.MustParsePrefix("10.0.0.0/8"), Priority: 5200}: true}
	a, _ := newAgent(t, f)
	n, err := a.Cleanup()
	if err != nil || n != 2 || len(f.rules) != 0 {
		t.Errorf("n %d, err %v, rules %v", n, err, f.rules)
	}
}

func TestRunReactsToEvents(t *testing.T) {
	f := atHome()
	f.iface = ""
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	old := Settle
	Settle = 10 * time.Millisecond
	defer func() { Settle = old }()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { a.Run(ctx, time.Hour, events); close(done) }()
	waitFor(t, func() bool { return !a.LastPass().IsZero() })
	// Joining the network: the event, not the hourly pass, finds it.
	f.set(func(f *fake) { f.iface = "wlan0" })
	events <- struct{}{}
	waitFor(t, func() bool { st := a.Statuses(); return len(st) == 1 && st[0].Rule })
	cancel()
	<-done
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

func TestChangingErrorTextIsNotANewVerdict(t *testing.T) {
	f := atHome()
	a, log := newAgent(t, f, "lan 192.168.1.0/24 probe 192.168.1.5:22")
	for _, e := range []string{"i/o timeout", "connect: no route to host", "i/o timeout"} {
		f.set(func(f *fake) { f.probeErr = errors.New(e) })
		a.Reconcile(context.Background())
	}
	if n := strings.Count(log.String(), "probe 192.168.1.5:22 failed"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, log)
	}
}

func TestStaleRulesInADryRun(t *testing.T) {
	f := atHome()
	old := sys.Rule{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Priority: 5200}
	f.rules = map[sys.Rule]bool{old: true, rule: true}
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	a.DryRun = true
	a.Reconcile(context.Background())
	if st := a.Stale(); len(st) != 1 || st[0] != old {
		t.Errorf("stale = %v", st)
	}
}

func TestRunLooksAgainAfterAChange(t *testing.T) {
	f := atHome()
	f.mac = nil // the gateway does not answer yet
	a, _ := newAgent(t, f, "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56")
	oldS, oldA := Settle, Again
	Settle, Again = 10*time.Millisecond, 50*time.Millisecond
	defer func() { Settle, Again = oldS, oldA }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan struct{}, 1)
	go a.Run(ctx, time.Hour, events)
	waitFor(t, func() bool { return !a.LastPass().IsZero() })
	events <- struct{}{}
	time.Sleep(30 * time.Millisecond) // the event's own pass: still silent
	f.set(func(f *fake) { f.mac = homeMAC })
	// No more events: only the follow-up pass can find it.
	waitFor(t, func() bool { st := a.Statuses(); return len(st) == 1 && st[0].Rule })
}
