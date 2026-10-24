package killswitch

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// lab is a network namespace with a veth to a peer namespace: 10.77.0.1
// here, 10.77.0.2 there.
func lab(t *testing.T) (ns string, s *Switch) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (network namespaces)")
	}
	ns = fmt.Sprintf("ksw%d", os.Getpid())
	peer := ns + "p"
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("ip", "netns", "add", ns)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", ns).Run(); exec.Command("ip", "netns", "del", peer).Run() })
	run("ip", "netns", "add", peer)
	run("ip", "link", "add", "kswa", "netns", ns, "type", "veth", "peer", "name", "kswb", "netns", peer)
	run("ip", "-n", ns, "addr", "add", "10.77.0.1/24", "dev", "kswa")
	run("ip", "-n", peer, "addr", "add", "10.77.0.2/24", "dev", "kswb")
	run("ip", "-n", ns, "link", "set", "kswa", "up")
	run("ip", "-n", peer, "link", "set", "kswb", "up")
	run("ip", "-n", ns, "link", "set", "lo", "up")
	// A container behind ns: 10.78.0.2, routed (and forwarded) by ns.
	ctr := ns + "c"
	run("ip", "netns", "add", ctr)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", ctr).Run() })
	run("ip", "link", "add", "kswc", "netns", ns, "type", "veth", "peer", "name", "kswd", "netns", ctr)
	run("ip", "-n", ns, "addr", "add", "10.78.0.1/24", "dev", "kswc")
	run("ip", "-n", ctr, "addr", "add", "10.78.0.2/24", "dev", "kswd")
	run("ip", "-n", ns, "link", "set", "kswc", "up")
	run("ip", "-n", ctr, "link", "set", "kswd", "up")
	run("ip", "-n", ctr, "route", "add", "default", "via", "10.78.0.1")
	run("ip", "-n", peer, "route", "add", "10.78.0.0/24", "via", "10.77.0.1")
	run("ip", "netns", "exec", ns, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	fd, err := unix.Open("/run/netns/"+ns, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return ns, &Switch{NetNS: fd}
}

func ping(ns string, args ...string) bool {
	return exec.Command("ip", append([]string{"netns", "exec", ns, "ping", "-c1", "-W1"}, args...)...).Run() == nil
}

// udp sends one datagram from sport to 10.77.0.2:dport and says whether the
// peer got it.
func udp(t *testing.T, ns string, sport, dport int) bool {
	t.Helper()
	got := make(chan bool, 1)
	lis := exec.Command("ip", "netns", "exec", ns+"p", "timeout", "2", "python3", "-c", fmt.Sprintf(
		"import socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)\ns.bind(('10.77.0.2',%d))\ns.recv(64)\nprint('got')", dport))
	go func() { out, _ := lis.Output(); got <- strings.Contains(string(out), "got") }()
	time.Sleep(300 * time.Millisecond)
	exec.Command("ip", "netns", "exec", ns, "python3", "-c", fmt.Sprintf(
		"import socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)\ns.bind(('0.0.0.0',%d))\ns.sendto(b'x',('10.77.0.2',%d))", sport, dport)).Run()
	return <-got
}

func TestKillSwitch(t *testing.T) {
	ns, s := lab(t)
	if !ping(ns, "10.77.0.2") || !pingFrom(ns+"c", "10.77.0.2") {
		t.Fatal("the lab does not work without the kill switch")
	}
	r := Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000, ProbeMark: 0x1000000}
	if err := s.On(r); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Intact(r); !ok || err != nil {
		t.Fatalf("intact %v %v", ok, err)
	}
	if ping(ns, "10.77.0.2") {
		t.Error("traffic left with the kill switch on")
	}
	if pingFrom(ns+"c", "10.77.0.2") {
		t.Error("forwarded traffic (a container) left with the kill switch on")
	}
	if !ping(ns, "127.0.0.1") {
		t.Error("loopback is blocked")
	}
	// tailscaled's own packets carry the mark, 0x80001 here (ping wants
	// decimal; the other bits are someone else's and do not matter).
	if !ping(ns, "-m", "524289", "10.77.0.2") {
		t.Error("tailscaled's marked packets are blocked")
	}
	if ping(ns, "-m", "262144", "10.77.0.2") {
		t.Error("another mark got through")
	}
	if !ping(ns, "-m", "16777216", "10.77.0.2") {
		t.Error("tsroam's probe mark is blocked")
	}
	// DHCP: only from the client's (privileged) port.
	if !udp(t, ns, 68, 67) {
		t.Error("a DHCP request is blocked")
	}
	if udp(t, ns, 40000, 67) {
		t.Error("UDP to port 67 from any port gets out: a way around the kill switch")
	}
	// Installing again replaces the table: allow-lan, DHCP on one interface.
	r.LAN, r.Iface = netip.MustParsePrefix("10.77.0.0/24"), "kswa"
	if err := s.On(r); err != nil {
		t.Fatal(err)
	}
	if !ping(ns, "10.77.0.2") {
		t.Error("the LAN is blocked with allow-lan")
	}
	if out, _ := exec.Command("ip", "netns", "exec", ns, "nft", "list", "ruleset").CombinedOutput(); strings.Count(string(out), "table inet tsroam") != 1 {
		t.Errorf("ruleset:\n%s", out)
	}
	// Flushed by someone else: the policy still drops, and Intact says so.
	r.LAN = netip.Prefix{}
	s.On(r)
	exec.Command("ip", "netns", "exec", ns, "nft", "flush", "table", "inet", "tsroam").Run()
	if ok, _ := s.Intact(r); ok {
		t.Error("a flushed table counts as intact")
	}
	if ping(ns, "-m", "524289", "10.77.0.2") {
		t.Error("a flushed table lets traffic out")
	}
	if err := s.Off(); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.Active(); on {
		t.Error("still active after Off")
	}
	if !ping(ns, "10.77.0.2") {
		t.Error("traffic still blocked after Off")
	}
	if err := s.Off(); err != nil {
		t.Errorf("Off without a table: %v", err)
	}
}

func pingFrom(ns, addr string) bool { return ping(ns, addr) }

func TestLANOK(t *testing.T) {
	for p, ok := range map[string]bool{"192.168.1.0/24": true, "10.0.0.0/8": true, "172.20.0.0/16": true, "169.254.0.0/16": true,
		"0.0.0.0/1": false, "128.0.0.0/1": false, "8.8.8.0/24": false, "172.0.0.0/8": false, "100.64.0.0/10": false, "10.0.0.0/7": false} {
		if LANOK(netip.MustParsePrefix(p)) != ok {
			t.Errorf("%s: want %v", p, ok)
		}
	}
}

func TestOthersTablesAreLeftAlone(t *testing.T) {
	ns, s := lab(t)
	if out, err := exec.Command("ip", "netns", "exec", ns, "nft", "add", "table", "inet", "someone").CombinedOutput(); err != nil {
		t.Skipf("nft: %v %s", err, out)
	}
	if err := s.On(Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000}); err != nil {
		t.Fatal(err)
	}
	if err := s.Off(); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("ip", "netns", "exec", ns, "nft", "list", "tables").CombinedOutput(); !strings.Contains(string(out), "table inet someone") {
		t.Errorf("tables:\n%s", out)
	}
}
