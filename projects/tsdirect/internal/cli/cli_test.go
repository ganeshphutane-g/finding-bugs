package cli

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/ganeshphutane-g/tsdirect/internal/agent"
	"github.com/ganeshphutane-g/tsdirect/internal/config"
	"github.com/ganeshphutane-g/tsdirect/internal/sys"
)

// machine is a fake system on the home LAN.
type machine struct {
	proto    uint8
	iface    string
	gateway  netip.Addr
	mac      net.HardwareAddr
	probeErr error
	rules    map[sys.Rule]bool
}

func home() *machine {
	return &machine{iface: "wlan0", gateway: netip.MustParseAddr("192.168.1.1"),
		mac: net.HardwareAddr{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}, rules: map[sys.Rule]bool{}}
}

func (m *machine) Connected(p netip.Prefix) (string, int, netip.Prefix, error) {
	c := netip.MustParsePrefix("192.168.1.0/24")
	if m.iface == "" || !c.Overlaps(p) {
		return "", 0, netip.Prefix{}, nil
	}
	return m.iface, 3, c, nil
}
func (m *machine) DefaultGateway(int) (netip.Addr, bool) { return m.gateway, m.gateway.IsValid() }
func (m *machine) GatewayMAC(context.Context, string, netip.Addr) (net.HardwareAddr, error) {
	if m.mac == nil {
		return nil, sys.ErrNoAnswer
	}
	return m.mac, nil
}
func (m *machine) Probe(context.Context, string, netip.AddrPort) error { return m.probeErr }
func (m *machine) Rules() ([]sys.Rule, error) {
	var out []sys.Rule
	for r := range m.rules {
		out = append(out, r)
	}
	return out, nil
}
func (m *machine) AddRule(r sys.Rule) error { m.rules[r] = true; return nil }
func (m *machine) DelRule(r sys.Rule) error { delete(m.rules, r); return nil }

func deps(m *machine) Deps {
	return Deps{
		System: func(proto uint8) (agent.System, error) { m.proto = proto; return m, nil },
		Watch:  func(ctx context.Context) (<-chan struct{}, error) { return make(chan struct{}), nil },
	}
}

func cmd(t *testing.T, m *machine, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb, deps(m))
	return code, out.String(), errb.String()
}

const homeLAN = "192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56"

func TestUsage(t *testing.T) {
	if code, _, e := cmd(t, home()); code != exitUsage || !strings.Contains(e, "tsdirect learn   PREFIX") {
		t.Errorf("no args: %d %q", code, e)
	}
	if code, _, e := cmd(t, home(), "frobnicate"); code != exitUsage || !strings.Contains(e, `unknown command "frobnicate"`) {
		t.Errorf("unknown: %d %q", code, e)
	}
	if code, out, _ := cmd(t, home(), "version"); code != exitOK || !strings.HasPrefix(out, "tsdirect ") {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestBadFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "--rule-proto", "3"}, "0 to 4 are used by the kernel"},
		{[]string{"check", "--rule-proto", "300"}, "a number from 5 to 255"},
		{[]string{"check", "--priority", "32766"}, "from 1 to 32765"},
		{[]string{"run", "--interval", "10ms"}, "at least 1s"},
		{[]string{"check", "extra"}, `unexpected "extra"`},
		{[]string{"learn"}, "Usage: tsdirect learn PREFIX"},
		{[]string{"learn", "192.168.1.5/24"}, "not an IPv4 network prefix"},
		{[]string{"learn", "fd00::/64"}, "not an IPv4 network prefix"},
		{[]string{"learn", "192.168.1.0/24", "--probe", "10.0.0.1:22"}, "address in 192.168.1.0/24"},
		{[]string{"check", "--rule-proto", "16"}, "16 is dhcp's"},
		{[]string{"cleanup", "--rule-proto", "186"}, "186 is bgp's"},
	} {
		code, _, e := cmd(t, home(), c.args...)
		if code != exitUsage || !strings.Contains(e, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, e, c.want)
		}
	}
}

func TestConfigErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "tsdirect.conf")
	os.WriteFile(bad, []byte("lan 192.168.1.0/24\n"), 0o644)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "--config", filepath.Join(dir, "missing")}, "no LANs"},
		{[]string{"check", "--config", bad}, "tsdirect.conf:1: 192.168.1.0/24 has no fingerprint"},
		{[]string{"check", "--config", "", "--lan", homeLAN, "--lan", "192.168.0.0/16 probe 192.168.0.1:22"}, "overlap"},
	} {
		code, _, e := cmd(t, home(), c.args...)
		if code != exitError || !strings.Contains(e, c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, code, e, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	m := home()
	code, out, _ := cmd(t, m, "check", "--config", "", "--lan", homeLAN)
	if code != exitOK || !strings.Contains(out, "192.168.1.0/24  wlan0      direct  no") || !strings.Contains(out, "[run would add the rule]") {
		t.Errorf("home: %d\n%s", code, out)
	}
	if len(m.rules) != 0 {
		t.Errorf("check changed the rules: %v", m.rules)
	}
	if m.proto != defaultProto {
		t.Errorf("proto %#x", m.proto)
	}

	m.mac = net.HardwareAddr{2, 0, 0, 0, 0, 9}
	code, out, _ = cmd(t, m, "check", "--config", "", "--lan", homeLAN)
	if code != exitMismatch || !strings.Contains(out, "not confirmed") {
		t.Errorf("café: %d\n%s", code, out)
	}

	m.iface = ""
	m.rules[sys.Rule{Prefix: netip.MustParsePrefix("192.168.1.0/24"), Priority: 5200}] = true
	code, out, _ = cmd(t, m, "check", "--config", "", "--lan", homeLAN)
	if code != exitOK || !strings.Contains(out, "not here") || !strings.Contains(out, "[run would remove the rule]") {
		t.Errorf("away: %d\n%s", code, out)
	}
}

func TestPriorityWarning(t *testing.T) {
	_, _, e := cmd(t, home(), "check", "--config", "", "--lan", homeLAN, "--priority", "5300")
	if !strings.Contains(e, "comes after Tailscale's rules") {
		t.Errorf("stderr %q", e)
	}
}

func TestLearn(t *testing.T) {
	code, out, e := cmd(t, home(), "learn", "192.168.1.0/24")
	if code != exitOK || out != "lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56\n" {
		t.Errorf("%d %q %q", code, out, e)
	}
	code, out, _ = cmd(t, home(), "learn", "192.168.1.0/24", "--gateway", "192.168.1.2", "--probe", "192.168.1.5:22")
	if code != exitOK || out != "lan 192.168.1.0/24 gateway 192.168.1.2 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22\n" {
		t.Errorf("%d %q", code, out)
	}
	m := home()
	m.probeErr = errors.New("connection refused")
	if code, out, e := cmd(t, m, "learn", "192.168.1.0/24", "--probe", "192.168.1.5:22"); code != exitError || out != "" || !strings.Contains(e, "connection refused") {
		t.Errorf("failing probe: %d %q %q", code, out, e)
	}
	m = home()
	m.gateway = netip.Addr{}
	if code, _, e := cmd(t, m, "learn", "192.168.1.0/24"); code != exitError || !strings.Contains(e, "give --gateway") {
		t.Errorf("no gateway: %d %q", code, e)
	}
	if code, out, _ := cmd(t, m, "learn", "192.168.1.0/24", "--probe", "192.168.1.5:22"); code != exitOK || out != "lan 192.168.1.0/24 probe 192.168.1.5:22\n" {
		t.Errorf("probe only: %d %q", code, out)
	}
	if code, _, e := cmd(t, home(), "learn", "10.0.0.0/8"); code != exitError || !strings.Contains(e, "not on 10.0.0.0/8") {
		t.Errorf("elsewhere: %d %q", code, e)
	}
	if code, _, e := cmd(t, home(), "learn", "192.168.1.0/25"); code != exitOK {
		t.Errorf("narrower: %d %q", code, e)
	}
	if code, _, e := cmd(t, home(), "learn", "192.168.0.0/16"); code != exitError || !strings.Contains(e, "learn 192.168.1.0/24 instead") {
		t.Errorf("wider: %d %q", code, e)
	}
}

func TestCleanup(t *testing.T) {
	m := home()
	m.rules[sys.Rule{Prefix: netip.MustParsePrefix("192.168.1.0/24"), Priority: 5200}] = true
	code, out, _ := cmd(t, m, "cleanup", "--rule-proto", "0xd2")
	if code != exitOK || out != "removed 1 rule\n" || len(m.rules) != 0 || m.proto != 0xd2 {
		t.Errorf("%d %q %v %#x", code, out, m.rules, m.proto)
	}
}

func TestRunAddsAndRemovesOnStop(t *testing.T) {
	m := home()
	d := deps(m)
	sig := make(chan chan<- os.Signal, 1)
	d.Signals = func(c chan<- os.Signal) { sig <- c }
	var out, errb syncBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"run", "--config", "", "--lan", homeLAN}, &out, &errb, d) }()
	c := <-sig
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "added rule"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no rule added:\n%s%s", out.String(), errb.String())
		}
	}
	c <- syscall.SIGTERM
	if code := <-done; code != exitOK {
		t.Errorf("exit %d: %s", code, errb.String())
	}
	if len(m.rules) != 0 || !strings.Contains(out.String(), "stopping: removed 1 rule") {
		t.Errorf("rules %v, log:\n%s", m.rules, out.String())
	}
}

func TestMetrics(t *testing.T) {
	m := home()
	a := &agent.Agent{Sys: m, Priority: 5200}
	lan, _ := parseLAN(homeLAN)
	a.SetLANs(lan)
	h := handler(a, 15*time.Second)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Errorf("healthz before a pass: %d", rec.Code)
	}
	a.Reconcile(context.Background())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{`tsdirect_lan_direct{lan="192.168.1.0/24"} 1`, `tsdirect_lan_rule{lan="192.168.1.0/24"} 1`, `tsdirect_rule_changes_total{lan="192.168.1.0/24"} 1`, "tsdirect_errors_total 0"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, rec.Body)
		}
	}
}

func parseLAN(s string) ([]config.LAN, error) {
	l, err := config.Parse(s, "test")
	return []config.LAN{l}, err
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

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunThatCannotStartRemovesOldRules(t *testing.T) {
	m := home()
	m.rules[sys.Rule{Prefix: netip.MustParsePrefix("192.168.1.0/24"), Priority: 5200}] = true // a crash's
	d := deps(m)
	d.Signals = func(chan<- os.Signal) {}
	var out, errb bytes.Buffer
	code := Main([]string{"run", "--config", filepath.Join(t.TempDir(), "missing")}, &out, &errb, d)
	if code != exitError || len(m.rules) != 0 || !strings.Contains(errb.String(), "no LANs") {
		t.Errorf("exit %d, rules %v, stderr %q", code, m.rules, errb.String())
	}
}

func TestRunStopsWhenEventsStop(t *testing.T) {
	m := home()
	d := deps(m)
	d.Signals = func(chan<- os.Signal) {}
	events := make(chan struct{})
	d.Watch = func(ctx context.Context) (<-chan struct{}, error) { return events, nil }
	var out, errb syncBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"run", "--config", "", "--lan", homeLAN}, &out, &errb, d) }()
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), "added rule"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no rule added:\n%s", out.String())
		}
	}
	close(events)
	if code := <-done; code != exitError || len(m.rules) != 0 || !strings.Contains(errb.String(), "events stopped") {
		t.Errorf("exit %d, rules %v, stderr %q", code, m.rules, errb.String())
	}
}

func TestReload(t *testing.T) {
	m := home()
	conf := filepath.Join(t.TempDir(), "tsdirect.conf")
	os.WriteFile(conf, []byte("lan 10.9.0.0/16 probe 10.9.0.1:22\n"), 0o644)
	d := deps(m)
	sig := make(chan chan<- os.Signal, 1)
	d.Signals = func(c chan<- os.Signal) { sig <- c }
	var out, errb syncBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"run", "--config", conf}, &out, &errb, d) }()
	c := <-sig
	waitOut := func(s string) {
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), s); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q:\n%s%s", s, out.String(), errb.String())
			}
		}
	}
	waitOut("10.9.0.0/16: not on")
	os.WriteFile(conf, []byte("lan 192.168.1.0/24 gateway-mac nope\n"), 0o644)
	c <- syscall.SIGHUP
	waitOut("keeping the LANs as they were")
	os.WriteFile(conf, []byte("lan "+homeLAN+"\n"), 0o644)
	c <- syscall.SIGHUP
	waitOut("added rule to 192.168.1.0/24")
	c <- syscall.SIGTERM
	if code := <-done; code != exitOK || len(m.rules) != 0 {
		t.Errorf("exit %d, rules %v", code, m.rules)
	}
}

func TestCheckNotesStaleRules(t *testing.T) {
	m := home()
	m.rules[sys.Rule{Prefix: netip.MustParsePrefix("10.9.0.0/16"), Priority: 5200}] = true
	_, out, _ := cmd(t, m, "check", "--config", "", "--lan", homeLAN)
	if !strings.Contains(out, "note: rule to 10.9.0.0/16 priority 5200 is tsdirect's but no configured LAN has it") {
		t.Errorf("out:\n%s", out)
	}
}

func TestRunWithBadFlagsRemovesOldRules(t *testing.T) {
	m := home()
	m.rules[sys.Rule{Prefix: netip.MustParsePrefix("192.168.1.0/24"), Priority: 5200}] = true
	d := deps(m)
	d.Signals = func(chan<- os.Signal) {}
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "--interval", "500ms"}, &out, &errb, d); code != exitUsage || len(m.rules) != 0 {
		t.Errorf("exit %d, rules %v, stderr %q", code, m.rules, errb.String())
	}
}

func TestCheckCannotAskIsAnError(t *testing.T) {
	m := &errMachine{home()}
	var out, errb bytes.Buffer
	d := deps(m.machine)
	d.System = func(uint8) (agent.System, error) { return m, nil }
	code := Main([]string{"check", "--config", "", "--lan", homeLAN}, &out, &errb, d)
	if code != exitError || !strings.Contains(out.String(), "could not be asked") {
		t.Errorf("exit %d:\n%s", code, out.String())
	}
}

// errMachine cannot send ARP requests (no CAP_NET_RAW).
type errMachine struct{ *machine }

func (m *errMachine) GatewayMAC(context.Context, string, netip.Addr) (net.HardwareAddr, error) {
	return nil, errors.New("ARP socket: operation not permitted")
}

func TestReloadToNoLANsRemovesTheRule(t *testing.T) {
	m := home()
	conf := filepath.Join(t.TempDir(), "tsdirect.conf")
	os.WriteFile(conf, []byte("lan "+homeLAN+"\n"), 0o644)
	d := deps(m)
	sig := make(chan chan<- os.Signal, 1)
	d.Signals = func(c chan<- os.Signal) { sig <- c }
	var out, errb syncBuffer
	done := make(chan int)
	go func() { done <- Main([]string{"run", "--config", conf}, &out, &errb, d) }()
	c := <-sig
	waitOut := func(s string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(out.String(), s); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q:\n%s%s", s, out.String(), errb.String())
			}
		}
	}
	waitOut("added rule")
	os.WriteFile(conf, []byte("# lan "+homeLAN+"\n"), 0o644)
	c <- syscall.SIGHUP
	waitOut("removed rule to 192.168.1.0/24 lookup main priority 5200: not configured")
	c <- syscall.SIGTERM
	<-done
}
