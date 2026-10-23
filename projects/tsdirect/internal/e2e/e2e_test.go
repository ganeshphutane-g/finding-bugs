// End-to-end tests with real Tailscale: headscale, a subnet router and a
// client with --accept-routes, in network namespaces. They run as root when
// TSDIRECT_INTEGRATION=1, with tailscale, tailscaled and headscale on PATH
// (or TAILSCALE, TAILSCALED, HEADSCALE).
//
//	inet ── hs (headscale 10.50.0.2)
//	     ├─ home gateway (NAT) ── 192.168.77.0/24: subnet router .10, nas .20, client .30
//	     └─ café gateway (NAT) ── 192.168.77.0/24 too, another gateway MAC
//
// The client is moved between the two LANs by moving its cable (the veth's
// other end) from one gateway's bridge to the other's.
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The nas runs this test binary as a TCP server (the probe's target and the
// "is the LAN reachable" check).
func TestMain(m *testing.M) {
	if addr := os.Getenv("TSDIRECT_E2E_LISTEN"); addr != "" {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				os.Exit(1)
			}
			c.Write([]byte("nas\n"))
			c.Close()
		}
	}
	os.Exit(m.Run())
}

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

// tsdirect builds the binary once.
func tsdirect(t *testing.T) string {
	if b := os.Getenv("TSDIRECT_BIN"); b != "" {
		return b
	}
	binOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "tsdirect-e2e")
		binPath = filepath.Join(dir, "tsdirect")
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

// proc is a process running in a namespace.
type proc struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
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

func (p *proc) stop(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	p.cmd.Process.Signal(sig)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

type lab struct {
	t                         *testing.T
	x, dir                    string
	inet, hs, gw, cafe        string // namespaces
	sr, nas, cl               string
	clPort                    string // the client's cable, at the gateway end
	hsCfg                     string
	srDir, clDir              string
	srTS, clTS                *proc
	procs                     []*proc
	tailscale, tailscaled, hd string
	resolv                    []byte
}

func id() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (l *lab) start(ns string, env []string, args ...string) *proc {
	l.t.Helper()
	p := &proc{out: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command("ip", append([]string{"netns", "exec", ns}, args...)...)
	p.cmd.Env = append(labEnv(), env...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		l.t.Fatal(err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	l.procs = append(l.procs, p)
	return p
}

// labEnv is the environment without proxy settings: the lab has no proxy,
// and Tailscale would use one for its relay connections.
func labEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (l *lab) in(ns string, args ...string) string {
	l.t.Helper()
	out, err := l.try(ns, args...)
	if err != nil {
		l.t.Fatalf("in %s: %s: %v\n%s", ns, strings.Join(args, " "), err, out)
	}
	return out
}

func (l *lab) try(ns string, args ...string) (string, error) {
	c := exec.Command("ip", append([]string{"netns", "exec", ns}, args...)...)
	c.Env = labEnv()
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); ; time.Sleep(250 * time.Millisecond) {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
	}
}

func newLab(t *testing.T) *lab {
	if os.Getenv("TSDIRECT_INTEGRATION") != "1" {
		t.Skip("set TSDIRECT_INTEGRATION=1 (as root) to run with real Tailscale")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	x := id()
	l := &lab{t: t, x: x, dir: t.TempDir(),
		tailscale: env("TAILSCALE", "tailscale"), tailscaled: env("TAILSCALED", "tailscaled"), hd: env("HEADSCALE", "headscale")}
	for _, b := range []*string{&l.tailscale, &l.tailscaled, &l.hd} {
		p, err := exec.LookPath(*b)
		if err != nil {
			t.Fatalf("%s: %v (set TAILSCALE, TAILSCALED, HEADSCALE)", *b, err)
		}
		*b = p
	}
	// tailscaled writes /etc/resolv.conf unless told not to, and network
	// namespaces share the file: put it back whatever happens.
	l.resolv, _ = os.ReadFile("/etc/resolv.conf")
	t.Cleanup(l.close)
	names := []string{"inet", "hs", "gw", "cafe", "sr", "nas", "cl"}
	ns := []*string{&l.inet, &l.hs, &l.gw, &l.cafe, &l.sr, &l.nas, &l.cl}
	for i, n := range ns {
		*n = "td" + x + names[i]
		sh(t, "ip", "netns", "add", *n)
		sh(t, "ip", "-n", *n, "link", "set", "lo", "up")
	}
	veth := func(a, aNS, b, bNS string) {
		sh(t, "ip", "link", "add", a, "type", "veth", "peer", "name", b)
		sh(t, "ip", "link", "set", a, "netns", aNS)
		sh(t, "ip", "link", "set", b, "netns", bNS)
		sh(t, "ip", "-n", aNS, "link", "set", a, "up")
		sh(t, "ip", "-n", bNS, "link", "set", b, "up")
	}
	// The internet.
	sh(t, "ip", "netns", "exec", l.inet, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	for i, n := range []string{l.hs, l.gw, l.cafe} {
		a, b := fmt.Sprintf("ti%s%d", x, i), fmt.Sprintf("tw%s%d", x, i)
		veth(a, l.inet, b, n)
		sh(t, "ip", "-n", l.inet, "addr", "add", fmt.Sprintf("10.5%d.0.1/24", i), "dev", a)
		sh(t, "ip", "-n", n, "addr", "add", fmt.Sprintf("10.5%d.0.2/24", i), "dev", b)
		sh(t, "ip", "-n", n, "route", "add", "default", "via", fmt.Sprintf("10.5%d.0.1", i))
	}
	// Two LANs with the same addresses, each behind its own NAT gateway.
	for i, g := range []string{l.gw, l.cafe} {
		// A bridge takes its lowest port's MAC unless given one; routers
		// have their own.
		sh(t, "ip", "-n", g, "link", "add", "br0", "address", fmt.Sprintf("02:00:5e:77:0%d:01", i), "type", "bridge")
		sh(t, "ip", "-n", g, "addr", "add", "192.168.77.1/24", "dev", "br0")
		sh(t, "ip", "-n", g, "link", "set", "br0", "up")
		sh(t, "ip", "netns", "exec", g, "sysctl", "-qw", "net.ipv4.ip_forward=1")
		for _, c := range []string{"add table ip nat",
			"add chain ip nat post { type nat hook postrouting priority 100 ; }",
			fmt.Sprintf("add rule ip nat post oifname tw%s%d masquerade", x, i+1)} {
			sh(t, append([]string{"ip", "netns", "exec", g, "nft"}, strings.Fields(c)...)...)
		}
	}
	for i, h := range []struct{ ns, addr string }{{l.sr, "192.168.77.10"}, {l.nas, "192.168.77.20"}, {l.cl, "192.168.77.30"}} {
		lan, port := fmt.Sprintf("tl%s%d", x, i), fmt.Sprintf("tp%s%d", x, i)
		sh(t, "ip", "link", "add", lan, "type", "veth", "peer", "name", port)
		sh(t, "ip", "link", "set", lan, "netns", h.ns)
		sh(t, "ip", "-n", h.ns, "link", "set", lan, "name", "lan0")
		sh(t, "ip", "link", "set", port, "netns", l.gw)
		sh(t, "ip", "-n", l.gw, "link", "set", port, "master", "br0")
		sh(t, "ip", "-n", l.gw, "link", "set", port, "up")
		lan = "lan0"
		sh(t, "ip", "-n", h.ns, "link", "set", lan, "up")
		sh(t, "ip", "-n", h.ns, "addr", "add", h.addr+"/24", "dev", lan)
		sh(t, "ip", "-n", h.ns, "route", "add", "default", "via", "192.168.77.1")
		if h.ns == l.cl {
			l.clPort = port
		}
	}
	sh(t, "ip", "netns", "exec", l.sr, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	l.start(l.nas, []string{"TSDIRECT_E2E_LISTEN=:8022"}, exe)

	l.headscale()
	key := l.hsCmd("preauthkeys", "create", "--user", "1", "--reusable", "--expiration", "2h")
	key = key[strings.LastIndex(key, "\n")+1:]
	l.srDir, l.clDir = filepath.Join(l.dir, "sr"), filepath.Join(l.dir, "cl")
	l.srTS = l.tailscaledIn(l.sr, l.srDir)
	l.up(l.sr, l.srDir, "--authkey", key, "--hostname", "sr", "--advertise-routes", "192.168.77.0/24")
	l.approve("sr", "192.168.77.0/24")
	l.clTS = l.tailscaledIn(l.cl, l.clDir)
	l.up(l.cl, l.clDir, "--authkey", key, "--hostname", "cl", "--accept-routes")
	waitFor(t, "the subnet route on the client", 60*time.Second, func() bool {
		out, _ := l.try(l.cl, "ip", "route", "show", "table", "52")
		return strings.Contains(out, "192.168.77.0/24 dev tailscale0")
	})
	return l
}

func (l *lab) headscale() {
	t := l.t
	d := filepath.Join(l.dir, "hs")
	os.MkdirAll(d, 0o700)
	l.hsCfg = filepath.Join(d, "config.yaml")
	cfg := fmt.Sprintf(`server_url: https://10.50.0.2:8443
listen_addr: 0.0.0.0:8443
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise:
  private_key_path: %[1]s/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: true
    region_id: 999
    region_code: lab
    region_name: lab
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: %[1]s/derp_server_private.key
    automatically_add_embedded_derp_region: true
    ipv4: 10.50.0.2
  urls: []
  paths: []
  auto_update_enabled: false
disable_check_updates: true
database:
  type: sqlite
  sqlite:
    path: %[1]s/db.sqlite
log:
  level: info
policy:
  mode: file
  path: ""
dns:
  magic_dns: false
  base_domain: lab.example
  override_local_dns: false
  nameservers:
    global: []
tls_cert_path: %[1]s/cert.pem
tls_key_path: %[1]s/key.pem
unix_socket: %[1]s/hs.sock
unix_socket_permission: "0770"
logtail:
  enabled: false
`, d)
	if err := os.WriteFile(l.hsCfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// Tailscale's relay (DERP) needs TLS: a certificate for 10.50.0.2,
	// trusted by the nodes through SSL_CERT_FILE.
	writeCert(t, d)
	p := l.start(l.hs, nil, l.hd, "-c", l.hsCfg, "serve")
	waitFor(t, "headscale", 30*time.Second, func() bool {
		select {
		case <-p.done:
			t.Fatalf("headscale exited:\n%s", p.out)
		default:
		}
		return exec.Command(l.hd, "-c", l.hsCfg, "users", "list").Run() == nil
	})
	l.hsCmd("users", "create", "lab")
}

func (l *lab) hsCmd(args ...string) string {
	return sh(l.t, append([]string{l.hd, "-c", l.hsCfg}, args...)...)
}

func (l *lab) approve(name, routes string) {
	var nodes []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(l.hsCmd("nodes", "list", "-o", "json")), &nodes); err != nil {
		l.t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == name {
			l.hsCmd("nodes", "approve-routes", "--identifier", fmt.Sprint(n.ID), "--routes", routes)
			return
		}
	}
	l.t.Fatalf("no node %q in %+v", name, nodes)
}

func (l *lab) tailscaledIn(ns, dir string) *proc {
	os.MkdirAll(dir, 0o700)
	p := l.start(ns, []string{"TS_NO_LOGS_NO_SUPPORT=true", "SSL_CERT_FILE=" + filepath.Join(l.dir, "hs", "cert.pem")}, l.tailscaled,
		"--state="+filepath.Join(dir, "state"), "--socket="+filepath.Join(dir, "sock"),
		"--tun=tailscale0", "--port=41641", "--statedir="+dir)
	waitFor(l.t, "tailscaled", 30*time.Second, func() bool {
		out, err := l.try(ns, l.tailscale, "--socket="+filepath.Join(dir, "sock"), "status")
		return err == nil || strings.Contains(out, "Logged out") || strings.Contains(out, "stopped") || strings.Contains(out, "NeedsLogin")
	})
	return p
}

// up logs a node in. --accept-dns=false always: see tailscaledIn.
func (l *lab) up(ns, dir string, args ...string) {
	l.in(ns, append([]string{l.tailscale, "--socket=" + filepath.Join(dir, "sock"), "up",
		"--login-server", "https://10.50.0.2:8443", "--accept-dns=false", "--timeout", "60s"}, args...)...)
}

func (l *lab) close() {
	for i := len(l.procs) - 1; i >= 0; i-- {
		l.procs[i].stop(syscall.SIGTERM)
	}
	for _, n := range []string{l.cl, l.nas, l.sr, l.cafe, l.gw, l.hs, l.inet} {
		if n != "" {
			exec.Command("ip", "netns", "del", n).Run()
		}
	}
	if now, _ := os.ReadFile("/etc/resolv.conf"); l.resolv != nil && !bytes.Equal(now, l.resolv) {
		os.WriteFile("/etc/resolv.conf", l.resolv, 0o644)
		l.t.Log("put /etc/resolv.conf back")
	}
	// TSDIRECT_E2E_LOGS=1 shows them on success too.
	if l.t.Failed() || os.Getenv("TSDIRECT_E2E_LOGS") == "1" {
		for _, p := range l.procs {
			l.t.Logf("%s:\n%s", strings.Join(p.cmd.Args[3:], " "), p.out)
		}
	}
}

// moveClient plugs the client's cable into the home or the café LAN.
func (l *lab) moveClient(toCafe bool) {
	from, to := l.gw, l.cafe
	if !toCafe {
		from, to = l.cafe, l.gw
	}
	sh(l.t, "ip", "-n", from, "link", "set", l.clPort, "netns", to)
	sh(l.t, "ip", "-n", to, "link", "set", l.clPort, "master", "br0")
	sh(l.t, "ip", "-n", to, "link", "set", l.clPort, "up")
}

// rules counts the client's rules sending the LAN to the main table.
func (l *lab) rules() int {
	return strings.Count(l.in(l.cl, "ip", "rule", "show"), "to 192.168.77.0/24 lookup main")
}

// via is the device the client's kernel sends LAN traffic to.
func (l *lab) via() string {
	out := l.in(l.cl, "ip", "route", "get", "192.168.77.20")
	f := strings.Fields(out)
	for i := range f {
		if f[i] == "dev" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return out
}

// dial connects from the client to the nas (unbound: as any program would).
func (l *lab) dial() error {
	_, err := l.try(l.cl, "timeout", "3", "bash", "-c", "exec 3<>/dev/tcp/192.168.77.20/8022")
	return err
}

func (l *lab) tsdirect(args ...string) *proc {
	return l.start(l.cl, nil, append([]string{tsdirect(l.t)}, args...)...)
}

func (l *lab) srUp(up bool) {
	sock := "--socket=" + filepath.Join(l.srDir, "sock")
	if up {
		l.in(l.sr, l.tailscale, sock, "up", "--login-server", "https://10.50.0.2:8443", "--accept-dns=false",
			"--hostname", "sr", "--advertise-routes", "192.168.77.0/24")
	} else {
		l.in(l.sr, l.tailscale, sock, "down")
	}
}

func TestTailscaleLab(t *testing.T) {
	l := newLab(t)

	t.Run("the problem: LAN traffic goes into the tailnet", func(t *testing.T) {
		if v := l.via(); v != "tailscale0" {
			t.Fatalf("LAN traffic goes to %s before tsdirect; the lab does not show the problem", v)
		}
	})

	learned, err := exec.Command("ip", "netns", "exec", l.cl, tsdirect(t), "learn", "192.168.77.0/24").Output()
	lanLine := strings.TrimSpace(string(learned))
	if err != nil || !strings.HasPrefix(lanLine, "lan 192.168.77.0/24 gateway-mac ") || strings.Contains(lanLine, "\n") {
		t.Fatalf("learn: %v, printed %q", err, lanLine)
	}
	lan := strings.TrimPrefix(lanLine, "lan ")
	t.Logf("learned: %s", lan)
	if out, err := l.try(l.cl, tsdirect(t), "check", "--config", "", "--lan", lan); err != nil || !strings.Contains(out, "direct") {
		t.Fatalf("check at home: %v\n%s", err, out)
	}

	// A long interval: every reaction below comes from network events.
	td := l.tsdirect("run", "--config", "", "--lan", lan, "--interval", "10m")
	waitRule := func(t *testing.T, want int) {
		t.Helper()
		waitFor(t, fmt.Sprintf("%d rule(s)", want), 20*time.Second, func() bool { return l.rules() == want })
	}

	t.Run("home: the rule is added and LAN traffic goes direct", func(t *testing.T) {
		waitRule(t, 1)
		if v := l.via(); v != "lan0" {
			t.Errorf("LAN traffic goes to %s", v)
		}
		if err := l.dial(); err != nil {
			t.Errorf("the nas: %v", err)
		}
		// With the subnet router gone, the LAN still works: it is direct.
		l.srUp(false)
		defer l.srUp(true)
		time.Sleep(2 * time.Second)
		if err := l.dial(); err != nil {
			t.Errorf("the nas with the subnet router down: %v", err)
		}
	})

	t.Run("restarting tailscaled keeps the rule", func(t *testing.T) {
		l.clTS.stop(syscall.SIGTERM)
		l.clTS = l.tailscaledIn(l.cl, l.clDir)
		waitFor(t, "Tailscale's rules back", 60*time.Second, func() bool {
			out, _ := l.try(l.cl, "ip", "route", "show", "table", "52")
			return strings.Contains(out, "192.168.77.0/24 dev tailscale0")
		})
		time.Sleep(3 * time.Second)
		if n := l.rules(); n != 1 {
			t.Errorf("%d rules", n)
		}
		if v := l.via(); v != "lan0" {
			t.Errorf("LAN traffic goes to %s", v)
		}
	})

	t.Run("a café with the same addresses: no rule", func(t *testing.T) {
		l.moveClient(true)
		defer func() { l.moveClient(false); waitRule(t, 1) }()
		waitRule(t, 0)
		if v := l.via(); v != "tailscale0" {
			t.Errorf("at the café, 192.168.77.20 goes to %s, not to the home LAN through the tailnet", v)
		}
		if !strings.Contains(td.out.String(), "another network") {
			t.Errorf("the log does not say why:\n%s", td.out)
		}
		if out, err := l.try(l.cl, tsdirect(t), "check", "--config", "", "--lan", lan); err == nil || !strings.Contains(out, "not confirmed") {
			t.Errorf("check at the café: %v\n%s", err, out)
		}
	})

	t.Run("leaving the LAN removes the rule", func(t *testing.T) {
		l.in(l.cl, "ip", "addr", "flush", "dev", "lan0")
		waitRule(t, 0)
		l.in(l.cl, "ip", "addr", "add", "192.168.77.30/24", "dev", "lan0")
		l.in(l.cl, "ip", "route", "replace", "default", "via", "192.168.77.1")
		waitRule(t, 1)
		l.in(l.cl, "ip", "link", "set", "lan0", "down")
		waitRule(t, 0)
		l.in(l.cl, "ip", "link", "set", "lan0", "up")
		l.in(l.cl, "ip", "route", "replace", "default", "via", "192.168.77.1")
		waitRule(t, 1)
	})

	t.Run("another interface with part of the prefix: no rule", func(t *testing.T) {
		// With the rule, 192.168.77.128/25 would go to dum0 instead of
		// through the tailnet: tsdirect must not add it.
		l.in(l.cl, "ip", "link", "add", "dum0", "type", "veth", "peer", "name", "dum1")
		l.in(l.cl, "ip", "addr", "add", "192.168.77.200/25", "dev", "dum0")
		l.in(l.cl, "ip", "link", "set", "dum1", "up")
		l.in(l.cl, "ip", "link", "set", "dum0", "up")
		waitRule(t, 0)
		if !strings.Contains(td.out.String(), "192.168.77.128/25 to dum0") {
			t.Errorf("the log does not say why:\n%s", td.out)
		}
		l.in(l.cl, "ip", "link", "del", "dum0")
		waitRule(t, 1)
	})

	t.Run("after a crash: one rule, and cleanup removes it", func(t *testing.T) {
		td.stop(syscall.SIGKILL)
		if n := l.rules(); n != 1 {
			t.Fatalf("%d rules after the crash", n)
		}
		td = l.tsdirect("run", "--config", "", "--lan", lan, "--interval", "10m")
		waitFor(t, "a pass", 20*time.Second, func() bool { return strings.Contains(td.out.String(), "192.168.77.0/24: on") })
		time.Sleep(3 * time.Second)
		if n := l.rules(); n != 1 || strings.Contains(td.out.String(), "added") {
			t.Errorf("%d rules; log:\n%s", n, td.out)
		}
		td.stop(syscall.SIGKILL)
		out := l.in(l.cl, tsdirect(t), "cleanup")
		if out != "removed 1 rule" || l.rules() != 0 {
			t.Errorf("cleanup: %q, %d rules", out, l.rules())
		}
	})

	t.Run("a probe fingerprint, made from the LAN interface", func(t *testing.T) {
		td = l.tsdirect("run", "--config", "", "--lan", "192.168.77.0/24 probe 192.168.77.20:8022", "--interval", "10m")
		waitRule(t, 1)
		l.moveClient(true)
		waitRule(t, 0)
		// From the café the nas is reachable through the tailnet; the probe
		// must not count that.
		for deadline := time.Now().Add(60 * time.Second); l.dial() != nil; time.Sleep(time.Second) {
			if time.Now().After(deadline) {
				sock := "--socket=" + filepath.Join(l.clDir, "sock")
				for _, c := range [][]string{{l.tailscale, sock, "status"}, {l.tailscale, sock, "netcheck"}, {l.tailscale, sock, "ping", "-c", "3", "sr"}, {"ip", "route", "get", "192.168.77.20"}} {
					out, err := l.try(l.cl, c...)
					t.Logf("%s: %v\n%s", strings.Join(c, " "), err, out)
				}
				t.Fatal("the nas is not reachable through the tailnet from the café")
			}
		}
		time.Sleep(5 * time.Second)
		if n := l.rules(); n != 0 {
			t.Errorf("%d rules at the café: the probe went through the tailnet", n)
		}
		l.moveClient(false)
		waitRule(t, 1)
		td.stop(syscall.SIGTERM)
		if n := l.rules(); n != 0 || !strings.Contains(td.out.String(), "stopping: removed 1 rule") {
			t.Errorf("%d rules after stopping; log:\n%s", n, td.out)
		}
	})
}

// writeCert writes a self-signed certificate for 10.50.0.2 (cert.pem, also
// its own CA) and its key (key.pem).
func writeCert(t *testing.T, dir string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tsdirect lab"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("10.50.0.2")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
}
