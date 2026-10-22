// End-to-end tests with real WireGuard in network namespaces. They run as
// root when WGRESOLVE_INTEGRATION=1. Kernel WireGuard is used when the
// kernel has it; otherwise wireguard-go (WIREGUARD_GO, default
// "wireguard-go" on PATH). dnsmasq (DNSMASQ, default "dnsmasq") serves the
// server's name.
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// wgresolve builds the binary once.
func wgresolve(t *testing.T) string {
	if b := os.Getenv("WGRESOLVE_BIN"); b != "" {
		return b
	}
	binOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "wgresolve-e2e")
		binPath = filepath.Join(dir, "wgresolve")
		out, err := exec.Command("go", "build", "-o", binPath, "../..").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// lab is two namespaces joined by veth: the client (where wgresolve runs)
// and the server, each with a WireGuard interface, and dnsmasq in the client
// namespace answering for the server's name.
type lab struct {
	t          *testing.T
	cns, sns   string
	rns, rpub  string // the router's namespace and its public interface
	cif, sif   string
	ckey, skey wgtypes.Key
	sport      int
	hosts      string // dnsmasq's hosts file
	dnsPort    int
	dnsmasq    *exec.Cmd
	userspace  bool
	ipv6       bool
}

func id() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newLab(t *testing.T) *lab {
	if os.Getenv("WGRESOLVE_INTEGRATION") != "1" {
		t.Skip("set WGRESOLVE_INTEGRATION=1 (as root) to run with real WireGuard")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	x := id()
	l := &lab{t: t, cns: "wgrc" + x, rns: "wgrr" + x, sns: "wgrs" + x, cif: "wgc" + x, sif: "wgs" + x, sport: 51821, dnsPort: 5353}
	t.Cleanup(l.close)
	// client 10.77.0.1 -- 10.77.0.2 (public) router 10.88.0.1 -- 10.88.0.2 server
	// The router forwards the WireGuard port from its public address to the
	// server, as a home router does; the server never sees its public
	// address change.
	for _, ns := range []string{l.cns, l.rns, l.sns} {
		sh(t, "ip", "netns", "add", ns)
		sh(t, "ip", "-n", ns, "link", "set", "lo", "up")
	}
	sh(t, "ip", "link", "add", "vc"+x, "type", "veth", "peer", "name", "vr"+x)
	sh(t, "ip", "link", "add", "vs"+x, "type", "veth", "peer", "name", "vq"+x)
	sh(t, "ip", "link", "set", "vc"+x, "netns", l.cns)
	sh(t, "ip", "link", "set", "vr"+x, "netns", l.rns)
	sh(t, "ip", "link", "set", "vq"+x, "netns", l.rns)
	sh(t, "ip", "link", "set", "vs"+x, "netns", l.sns)
	l.rpub = "vr" + x
	for _, c := range [][]string{
		{l.cns, "addr", "add", "10.77.0.1/24", "dev", "vc" + x},
		{l.rns, "addr", "add", "10.77.0.2/24", "dev", "vr" + x},
		{l.rns, "addr", "add", "10.88.0.1/24", "dev", "vq" + x},
		{l.sns, "addr", "add", "10.88.0.2/24", "dev", "vs" + x},
		{l.cns, "link", "set", "vc" + x, "up"},
		{l.rns, "link", "set", "vr" + x, "up"},
		{l.rns, "link", "set", "vq" + x, "up"},
		{l.sns, "link", "set", "vs" + x, "up"},
		{l.sns, "route", "add", "default", "via", "10.88.0.1"},
	} {
		sh(t, append([]string{"ip", "-n"}, c...)...)
	}
	sh(t, "ip", "netns", "exec", l.rns, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	l.nft(l.rns, "add table ip nat",
		"add chain ip nat pre { type nat hook prerouting priority -100 ; }",
		"add rule ip nat pre ip daddr 10.77.0.2 udp dport "+strconv.Itoa(l.sport)+" dnat to 10.88.0.2")
	// IPv6, where the kernel has it: routed, no NAT.
	if exec.Command("ip", "-n", l.cns, "-6", "addr", "add", "fd77::1/64", "dev", "vc"+x, "nodad").Run() == nil {
		for _, c := range [][]string{
			{l.rns, "-6", "addr", "add", "fd77::2/64", "dev", "vr" + x, "nodad"},
			{l.rns, "-6", "addr", "add", "fd88::1/64", "dev", "vq" + x, "nodad"},
			{l.sns, "-6", "addr", "add", "fd88::2/64", "dev", "vs" + x, "nodad"},
			{l.cns, "-6", "route", "add", "fd88::/64", "via", "fd77::2"},
			{l.sns, "-6", "route", "add", "default", "via", "fd88::1"},
		} {
			sh(t, append([]string{"ip", "-n"}, c...)...)
		}
		sh(t, "ip", "netns", "exec", l.rns, "sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
		l.ipv6 = true
	}
	l.addWG(l.cns, l.cif)
	l.addWG(l.sns, l.sif)
	l.ckey, _ = wgtypes.GeneratePrivateKey()
	l.skey, _ = wgtypes.GeneratePrivateKey()
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	ck := write("c.key", l.ckey.String())
	sk := write("s.key", l.skey.String())
	l.wg(l.sns, "set", l.sif, "private-key", sk, "listen-port", strconv.Itoa(l.sport), "peer", l.ckey.PublicKey().String(), "allowed-ips", "10.78.0.1/32")
	l.wg(l.cns, "set", l.cif, "private-key", ck, "peer", l.skey.PublicKey().String(), "allowed-ips", "10.78.0.2/32", "persistent-keepalive", "1")
	sh(t, "ip", "-n", l.cns, "addr", "add", "10.78.0.1/24", "dev", l.cif)
	sh(t, "ip", "-n", l.sns, "addr", "add", "10.78.0.2/24", "dev", l.sif)
	sh(t, "ip", "-n", l.cns, "link", "set", l.cif, "up")
	sh(t, "ip", "-n", l.sns, "link", "set", l.sif, "up")

	l.hosts = write("hosts", "")
	l.dnsmasq = exec.Command("ip", "netns", "exec", l.cns, env("DNSMASQ", "dnsmasq"), "--keep-in-foreground", "--no-resolv", "--no-hosts",
		"--listen-address=127.0.0.1", "--bind-interfaces", "--port", strconv.Itoa(l.dnsPort), "--addn-hosts="+l.hosts, "--local-ttl=1", "--local=/test/",
		"--pid-file=", "--user=root", "--log-facility=-")
	var log bytes.Buffer
	l.dnsmasq.Stdout, l.dnsmasq.Stderr = &log, &log
	if err := l.dnsmasq.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	return l
}

func (l *lab) addWG(ns, name string) {
	if exec.Command("ip", "-n", ns, "link", "add", name, "type", "wireguard").Run() == nil {
		return
	}
	l.userspace = true
	// wireguard-go puts its UAPI socket in /var/run/wireguard, shared by
	// all namespaces: names are unique per lab.
	sh(l.t, "ip", "netns", "exec", ns, env("WIREGUARD_GO", "wireguard-go"), name)
}

func (l *lab) wg(ns string, args ...string) string {
	return sh(l.t, append([]string{"ip", "netns", "exec", ns, "wg"}, args...)...)
}

// setDNS writes the server's name records (address per line) and has
// dnsmasq read them again.
func (l *lab) setDNS(lines ...string) {
	os.WriteFile(l.hosts, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	l.dnsmasq.Process.Signal(syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond)
}

// handshake is the client's last handshake with the server (zero: never).
func (l *lab) handshake() time.Time {
	out := l.wg(l.cns, "show", l.cif, "latest-handshakes")
	f := strings.Fields(out)
	if len(f) < 2 {
		return time.Time{}
	}
	n, _ := strconv.ParseInt(f[1], 10, 64)
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (l *lab) endpoint(ns, iface string) string {
	f := strings.Fields(l.wg(ns, "show", iface, "endpoints"))
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// moveServer gives the router a new public address, as when a home
// connection gets a new address from its provider: the old one is gone.
func (l *lab) moveServer(old, new string) {
	sh(l.t, "ip", "-n", l.rns, "addr", "del", old+"/24", "dev", l.rpub)
	sh(l.t, "ip", "-n", l.rns, "addr", "add", new+"/24", "dev", l.rpub)
	l.nft(l.rns, "flush chain ip nat pre",
		"add rule ip nat pre ip daddr "+new+" udp dport "+strconv.Itoa(l.sport)+" dnat to 10.88.0.2")
}

func (l *lab) nft(ns string, rules ...string) {
	for _, r := range rules {
		sh(l.t, append([]string{"ip", "netns", "exec", ns, env("NFT", "nft")}, strings.Fields(r)...)...)
	}
}

// start runs wgresolve in the client namespace, with a short --stale (a
// quiet peer is only changed when DNS disagrees, so tests can be quick).
func (l *lab) start(args ...string) *bytes.Buffer {
	return l.startWith(append([]string{"--stale", "10s"}, args...)...)
}

func (l *lab) startWith(args ...string) *bytes.Buffer {
	full := append([]string{"netns", "exec", l.cns, wgresolve(l.t), "run", "--resolver", "127.0.0.1:" + strconv.Itoa(l.dnsPort),
		"--interval", "1s", "--min-ttl", "1s", "--networkd-dir", l.t.TempDir(), "--nm-dir", l.t.TempDir()}, args...)
	cmd := exec.Command("ip", full...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
		if l.t.Failed() {
			l.t.Logf("wgresolve output:\n%s", out.String())
		}
	})
	return &out
}

func (l *lab) close() {
	if l.dnsmasq != nil && l.dnsmasq.Process != nil {
		l.dnsmasq.Process.Kill()
		l.dnsmasq.Wait()
	}
	exec.Command("ip", "-n", l.cns, "link", "del", l.cif).Run()
	exec.Command("ip", "-n", l.sns, "link", "del", l.sif).Run()
	exec.Command("ip", "netns", "del", l.cns).Run()
	exec.Command("ip", "netns", "del", l.rns).Run()
	exec.Command("ip", "netns", "del", l.sns).Run()
	os.Remove("/var/run/wireguard/" + l.cif + ".sock")
	os.Remove("/var/run/wireguard/" + l.sif + ".sock")
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

func (l *lab) peerFlag(host string) string {
	return l.cif + "=" + l.skey.PublicKey().String() + "=" + host + ":" + strconv.Itoa(l.sport)
}

func TestServerMoves(t *testing.T) {
	l := newLab(t)
	l.setDNS("10.77.0.2 server.test")
	// As wg-quick would have set it up: the name resolved at start.
	l.wg(l.cns, "set", l.cif, "peer", l.skey.PublicKey().String(), "endpoint", "10.77.0.2:"+strconv.Itoa(l.sport))
	waitFor(t, 10*time.Second, "the first handshake", func() bool { return !l.handshake().IsZero() })

	// The source is a wg-quick conf, with a second peer at a fixed address.
	conf := t.TempDir()
	other, _ := wgtypes.GeneratePrivateKey()
	l.wg(l.cns, "set", l.cif, "peer", other.PublicKey().String(), "endpoint", "10.77.0.9:51820", "allowed-ips", "10.78.0.9/32")
	os.WriteFile(filepath.Join(conf, l.cif+".conf"), []byte(fmt.Sprintf(`[Interface]
PrivateKey = %s

[Peer]
PublicKey = %s
Endpoint = server.test:%d
AllowedIPs = 10.78.0.2/32

[Peer]
PublicKey = %s
Endpoint = 10.77.0.9:51820
AllowedIPs = 10.78.0.9/32
`, l.ckey, l.skey.PublicKey(), l.sport, other.PublicKey())), 0o600)
	out := l.start("--wg-quick-dir", conf)

	// A working peer is left alone even when DNS says something else for a
	// moment.
	l.setDNS("10.77.0.200 server.test")
	time.Sleep(3 * time.Second)
	if ep := l.endpoint(l.cns, l.cif); !strings.Contains(ep, "10.77.0.2:") {
		t.Fatalf("a working peer was changed: %s\n%s", ep, out)
	}

	// The server moves, and its name follows.
	l.moveServer("10.77.0.2", "10.77.0.3")
	l.setDNS("10.77.0.3 server.test")
	// The session's keys stay valid for up to 3 minutes, so traffic flows
	// again at once; the next handshake comes at the next rekey.
	waitFor(t, 60*time.Second, "traffic through the tunnel at the new address", func() bool {
		return strings.HasPrefix(l.endpoint(l.cns, l.cif), "10.77.0.3:") && l.ping()
	})
	if !strings.Contains(out.String(), "-> 10.77.0.3:") {
		t.Fatalf("no change logged:\n%s", out)
	}
	// The fixed peer was not touched.
	f := strings.Fields(l.wg(l.cns, "show", l.cif, "endpoints"))
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == other.PublicKey().String() && f[i+1] != "10.77.0.9:51820" {
			t.Fatalf("the fixed peer was changed: %s", f[i+1])
		}
	}
}

// ping sends one ping through the tunnel.
func (l *lab) ping() bool {
	return exec.Command("ip", "netns", "exec", l.cns, "ping", "-c", "1", "-W", "1", "10.78.0.2").Run() == nil
}

func TestNameLateAtBoot(t *testing.T) {
	l := newLab(t)
	// The name did not resolve when the interface came up: no endpoint.
	l.setDNS("")
	out := l.start("--no-files", "--peer", l.peerFlag("late.test"))
	time.Sleep(3 * time.Second)
	if l.endpoint(l.cns, l.cif) != "(none)" {
		t.Fatalf("endpoint %q", l.endpoint(l.cns, l.cif))
	}
	if !strings.Contains(out.String(), "cannot resolve late.test") {
		t.Fatalf("%s", out)
	}
	l.setDNS("10.77.0.2 late.test")
	waitFor(t, 30*time.Second, "the first handshake", func() bool { return !l.handshake().IsZero() })
	if !l.ping() {
		t.Fatal("no traffic through the tunnel")
	}
}

func TestIPv6Preferred(t *testing.T) {
	l := newLab(t)
	if !l.ipv6 {
		t.Skip("this kernel has no IPv6")
	}
	l.setDNS("10.77.0.99 dual.test", "fd88::2 dual.test")
	l.start("--no-files", "--prefer", "ipv6", "--peer", l.peerFlag("dual.test"))
	waitFor(t, 30*time.Second, "a handshake over IPv6", func() bool {
		return !l.handshake().IsZero() && strings.HasPrefix(l.endpoint(l.cns, l.cif), "[fd88::2]:")
	})
}

func TestCheck(t *testing.T) {
	l := newLab(t)
	l.setDNS("10.77.0.2 server.test")
	run := func() (string, int) {
		cmd := exec.Command("ip", "netns", "exec", l.cns, wgresolve(t), "check", "--no-files", "--resolver", "127.0.0.1:"+strconv.Itoa(l.dnsPort), "--peer", l.peerFlag("server.test"))
		b, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(b), code
	}
	// No endpoint yet: check says it would set one, and changes nothing.
	out, code := run()
	if code != 3 || !strings.Contains(out, "would change") || l.endpoint(l.cns, l.cif) != "(none)" {
		t.Fatalf("%d %s", code, out)
	}
	l.wg(l.cns, "set", l.cif, "peer", l.skey.PublicKey().String(), "endpoint", "10.77.0.2:"+strconv.Itoa(l.sport))
	waitFor(t, 10*time.Second, "the first handshake", func() bool { return !l.handshake().IsZero() })
	if out, code := run(); code != 0 || !strings.Contains(out, " ok") {
		t.Fatalf("%d %s", code, out)
	}
}

// TestServerMovesDefaults is the real thing, with the default 135 s: the
// tunnel dies, WireGuard's own rekey fails, wgresolve moves the endpoint,
// and a new handshake follows within seconds.
func TestServerMovesDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 3 minutes")
	}
	l := newLab(t)
	l.setDNS("10.77.0.2 server.test")
	l.wg(l.cns, "set", l.cif, "peer", l.skey.PublicKey().String(), "endpoint", "10.77.0.2:"+strconv.Itoa(l.sport))
	waitFor(t, 10*time.Second, "the first handshake", func() bool { return !l.handshake().IsZero() })
	first := l.handshake()
	out := l.startWith("--no-files", "--peer", l.peerFlag("server.test"))
	l.moveServer("10.77.0.2", "10.77.0.3")
	l.setDNS("10.77.0.3 server.test")
	waitFor(t, 5*time.Minute, "a new handshake at the new address", func() bool {
		return l.handshake().After(first) && strings.HasPrefix(l.endpoint(l.cns, l.cif), "10.77.0.3:")
	})
	if !l.ping() {
		t.Fatalf("no traffic:\n%s", out)
	}
	t.Logf("handshake again %s after the last good one", l.handshake().Sub(first))
}
