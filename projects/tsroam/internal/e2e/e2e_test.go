// End-to-end tests with real Tailscale: headscale, an exit node and a
// client, in network namespaces. They run as root when TSROAM_INTEGRATION=1,
// with tailscale, tailscaled and headscale on PATH (or TAILSCALE,
// TAILSCALED, HEADSCALE).
//
//	inet ── hs (headscale 10.50.0.2)
//	     ├─ web (9.9.9.2: says which address it sees; exit nodes only
//	     │       forward to public addresses)
//	     ├─ home gateway (NAT, 10.51.0.2) ── 192.168.77.0/24: exit node .10, client .30
//	     └─ café gateway (NAT, 10.52.0.2) ── 192.168.77.0/24 too, another
//	                                         gateway MAC; a café host .20
//
// The client is moved between the two LANs by moving its cable (the veth's
// other end) from one gateway's bridge to the other's. Through the exit
// node, the web host sees the home gateway's address, 10.51.0.2; directly
// from the café, 10.52.0.2.
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
	"errors"
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

// The web and café hosts run this test binary as a TCP server that writes
// back the address it sees.
func TestMain(m *testing.M) {
	if addr := os.Getenv("TSROAM_E2E_LISTEN"); addr != "" {
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
			host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			c.Write([]byte(host + "\n"))
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

// tsroam builds the binary once.
func tsroam(t *testing.T) string {
	if b := os.Getenv("TSROAM_BIN"); b != "" {
		return b
	}
	binOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "tsroam-e2e")
		binPath = filepath.Join(dir, "tsroam")
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
	inet, hs, gw, cafe, web   string // namespaces
	sr, caf, cl               string
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
	if os.Getenv("TSROAM_INTEGRATION") != "1" {
		t.Skip("set TSROAM_INTEGRATION=1 (as root) to run with real Tailscale")
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
	names := []string{"inet", "hs", "gw", "cafe", "web", "sr", "caf", "cl"}
	ns := []*string{&l.inet, &l.hs, &l.gw, &l.cafe, &l.web, &l.sr, &l.caf, &l.cl}
	for i, n := range ns {
		*n = "tr" + x + names[i]
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
	veth("ti"+x+"w", l.inet, "tw"+x+"w", l.web)
	sh(t, "ip", "-n", l.inet, "addr", "add", "9.9.9.1/24", "dev", "ti"+x+"w")
	sh(t, "ip", "-n", l.web, "addr", "add", "9.9.9.2/24", "dev", "tw"+x+"w")
	sh(t, "ip", "-n", l.web, "route", "add", "default", "via", "9.9.9.1")
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
	for i, h := range []struct{ ns, addr, gw string }{{l.sr, "192.168.77.10", l.gw}, {l.caf, "192.168.77.20", l.cafe}, {l.cl, "192.168.77.30", l.gw}} {
		lan, port := fmt.Sprintf("tl%s%d", x, i), fmt.Sprintf("tp%s%d", x, i)
		sh(t, "ip", "link", "add", lan, "type", "veth", "peer", "name", port)
		sh(t, "ip", "link", "set", lan, "netns", h.ns)
		sh(t, "ip", "-n", h.ns, "link", "set", lan, "name", "lan0")
		sh(t, "ip", "link", "set", port, "netns", h.gw)
		sh(t, "ip", "-n", h.gw, "link", "set", port, "master", "br0")
		sh(t, "ip", "-n", h.gw, "link", "set", port, "up")
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
	l.start(l.web, []string{"TSROAM_E2E_LISTEN=:80"}, exe)
	l.start(l.caf, []string{"TSROAM_E2E_LISTEN=:80"}, exe)

	l.headscale()
	key := l.hsCmd("preauthkeys", "create", "--user", "1", "--reusable", "--expiration", "2h")
	key = key[strings.LastIndex(key, "\n")+1:]
	l.srDir, l.clDir = filepath.Join(l.dir, "sr"), filepath.Join(l.dir, "cl")
	l.srTS = l.tailscaledIn(l.sr, l.srDir)
	l.up(l.sr, l.srDir, "--authkey", key, "--hostname", "sr", "--advertise-exit-node")
	l.approve("sr", "0.0.0.0/0,::/0")
	l.clTS = l.tailscaledIn(l.cl, l.clDir)
	l.up(l.cl, l.clDir, "--authkey", key, "--hostname", "cl")
	waitFor(t, "the exit node offered to the client", 60*time.Second, func() bool {
		out, _ := l.try(l.cl, l.tailscale, "--socket="+filepath.Join(l.clDir, "sock"), "status")
		return strings.Contains(out, "offers exit node")
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
	for _, n := range []string{l.cl, l.caf, l.sr, l.web, l.cafe, l.gw, l.hs, l.inet} {
		if n != "" {
			exec.Command("ip", "netns", "del", n).Run()
		}
	}
	if now, _ := os.ReadFile("/etc/resolv.conf"); l.resolv != nil && !bytes.Equal(now, l.resolv) {
		os.WriteFile("/etc/resolv.conf", l.resolv, 0o644)
		l.t.Log("put /etc/resolv.conf back")
	}
	// TSROAM_E2E_LOGS=1 shows them on success too.
	if l.t.Failed() || os.Getenv("TSROAM_E2E_LOGS") == "1" {
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

// seen is the address the web host sees the client as ("" unreachable).
func (l *lab) seen(host string) string {
	out, err := l.try(l.cl, "timeout", "3", "bash", "-c", "exec 3<>/dev/tcp/"+host+"/80; head -1 <&3")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (l *lab) ts(args ...string) (string, error) {
	return l.try(l.cl, append([]string{l.tailscale, "--socket=" + filepath.Join(l.clDir, "sock")}, args...)...)
}

// exitNode says whether the client uses an exit node now.
func (l *lab) exitNode() bool {
	out, err := l.ts("status", "--json")
	if err != nil {
		return false
	}
	var st struct{ ExitNodeStatus *struct{ ID string } }
	json.Unmarshal([]byte(out), &st)
	return st.ExitNodeStatus != nil && st.ExitNodeStatus.ID != ""
}

// killSwitch says whether tsroam's table is in place.
func (l *lab) killSwitch() bool {
	out, _ := l.try(l.cl, "nft", "list", "tables")
	return strings.Contains(out, "table inet tsroam")
}

func (l *lab) tsroam(args ...string) *proc {
	return l.start(l.cl, nil, append([]string{tsroam(l.t)}, args...)...)
}

func (l *lab) tsroamCmd(args ...string) (string, error) {
	return l.try(l.cl, append([]string{tsroam(l.t)}, args...)...)
}

// The home gateway's and the café gateway's public addresses.
const viaHome, viaCafe = "10.51.0.2", "10.52.0.2"

func TestTailscaleLab(t *testing.T) {
	l := newLab(t)
	learned, err := exec.Command("ip", "netns", "exec", l.cl, tsroam(t), "learn").Output()
	line := strings.TrimSpace(string(learned))
	if err != nil || !strings.HasPrefix(line, "trusted 192.168.77.0/24 gateway-mac 02:00:5e:77:00:01") {
		t.Fatalf("learn: %v, printed %q", err, line)
	}
	conf := filepath.Join(l.dir, "tsroam.conf")
	os.WriteFile(conf, []byte("exit-node sr\nkill-switch on\n"+line+"\n"), 0o644)
	common := []string{"--config", conf, "--tailscale", l.tailscale, "--socket", filepath.Join(l.clDir, "sock"), "--state-dir", filepath.Join(l.dir, "run")}
	// A long interval: the reactions below come from network events.
	td := l.tsroam(append([]string{"run", "--interval", "10m"}, common...)...)
	wait := func(t *testing.T, what string, ok func() bool) {
		t.Helper()
		waitFor(t, what, 30*time.Second, ok)
	}

	t.Run("home: no exit node, no kill switch", func(t *testing.T) {
		wait(t, "the first pass", func() bool { return strings.Contains(td.out.String(), "trusted:") })
		if l.exitNode() || l.killSwitch() {
			t.Errorf("exit node %v, kill switch %v at home", l.exitNode(), l.killSwitch())
		}
		if s := l.seen("9.9.9.2"); s != viaHome {
			t.Errorf("the web host sees %q", s)
		}
		if out, err := l.tsroamCmd(append([]string{"check"}, common...)...); err != nil || !strings.Contains(out, "network:     trusted") {
			t.Errorf("check at home: %v\n%s", err, out)
		}
	})

	t.Run("café: the exit node and the kill switch", func(t *testing.T) {
		l.moveClient(true)
		wait(t, "the exit node", l.exitNode)
		wait(t, "the kill switch", l.killSwitch)
		wait(t, "the web host through the exit node", func() bool { return l.seen("9.9.9.2") == viaHome })
		if s := l.seen("192.168.77.20"); s != "" {
			t.Errorf("the café's own host is reachable (it saw %q): the kill switch lets the café LAN through", s)
		}
		out, err := l.tsroamCmd(append([]string{"check"}, common...)...)
		if code := exitCode(err); code != 4 || !strings.Contains(out, "network:     untrusted") {
			t.Errorf("check at the café: exit %d\n%s", code, out)
		}
	})

	t.Run("café: tailscaled killed, nothing leaks; restarted, traffic comes back", func(t *testing.T) {
		l.clTS.stop(syscall.SIGKILL)
		time.Sleep(time.Second)
		if s := l.seen("9.9.9.2"); s != "" {
			t.Fatalf("with tailscaled dead the web host saw %q: a leak", s)
		}
		l.clTS = l.tailscaledIn(l.cl, l.clDir)
		wait(t, "the web host through the exit node again", func() bool { return l.seen("9.9.9.2") == viaHome })
	})

	t.Run("café: tailscale down, nothing leaks", func(t *testing.T) {
		if _, err := l.ts("down"); err != nil {
			t.Fatal(err)
		}
		if s := l.seen("9.9.9.2"); s != "" {
			t.Errorf("with tailscale down the web host saw %q", s)
		}
		if out, err := l.ts("up"); err != nil {
			t.Fatalf("up: %v %s", err, out)
		}
		wait(t, "the web host through the exit node again", func() bool { return l.seen("9.9.9.2") == viaHome })
	})

	t.Run("café: the exit node turned off by hand comes back", func(t *testing.T) {
		if _, err := l.ts("set", "--exit-node="); err != nil {
			t.Fatal(err)
		}
		wait(t, "the exit node back", l.exitNode)
	})

	t.Run("café: tsroam off, then home ends the pause", func(t *testing.T) {
		if out, err := l.tsroamCmd(append([]string{"off"}, common...)...); err != nil {
			t.Fatalf("off: %v %s", err, out)
		}
		wait(t, "direct traffic", func() bool { return l.seen("9.9.9.2") == viaCafe })
		time.Sleep(2 * time.Second)
		if l.exitNode() || l.killSwitch() {
			t.Errorf("run forced them during the pause: exit node %v, kill switch %v", l.exitNode(), l.killSwitch())
		}
	})

	t.Run("home: both off", func(t *testing.T) {
		l.moveClient(false)
		wait(t, "trusted", func() bool { return strings.Count(td.out.String(), "trusted: the trusted network") >= 2 })
		if l.exitNode() || l.killSwitch() {
			t.Errorf("exit node %v, kill switch %v", l.exitNode(), l.killSwitch())
		}
		if s := l.seen("9.9.9.2"); s != viaHome {
			t.Errorf("the web host sees %q", s)
		}
	})

	t.Run("a crash of tsroam leaves the kill switch; cleanup removes it", func(t *testing.T) {
		l.moveClient(true)
		wait(t, "the kill switch", l.killSwitch)
		td.stop(syscall.SIGKILL)
		l.moveClient(false)
		time.Sleep(2 * time.Second)
		if !l.killSwitch() {
			t.Fatal("the kill switch went with tsroam")
		}
		if out, err := l.tsroamCmd("cleanup", "--state-dir", filepath.Join(l.dir, "run")); err != nil || l.killSwitch() {
			t.Errorf("cleanup: %v %s", err, out)
		}
	})

	t.Run("a clean stop removes the kill switch", func(t *testing.T) {
		l.moveClient(true)
		td = l.tsroam(append([]string{"run", "--interval", "10m"}, common...)...)
		wait(t, "the kill switch", l.killSwitch)
		td.stop(syscall.SIGTERM)
		if l.killSwitch() || !strings.Contains(td.out.String(), "stopped") {
			t.Errorf("kill switch %v; log:\n%s", l.killSwitch(), td.out)
		}
		l.moveClient(false)
	})
}

func exitCode(err error) int {
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
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
		Subject:               pkix.Name{CommonName: "tsroam lab"},
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
