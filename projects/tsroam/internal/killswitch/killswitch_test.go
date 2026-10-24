package killswitch

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

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

func TestKillSwitch(t *testing.T) {
	ns, s := lab(t)
	if !ping(ns, "10.77.0.2") {
		t.Fatal("the lab does not work without the kill switch")
	}
	r := Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000}
	if err := s.On(r); err != nil {
		t.Fatal(err)
	}
	if on, err := s.Active(); !on || err != nil {
		t.Fatalf("active %v %v", on, err)
	}
	if ping(ns, "10.77.0.2") {
		t.Error("traffic left with the kill switch on")
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
	// Installing again replaces the table: allow-lan.
	r.LAN = netip.MustParsePrefix("10.77.0.0/24")
	if err := s.On(r); err != nil {
		t.Fatal(err)
	}
	if !ping(ns, "10.77.0.2") {
		t.Error("the LAN is blocked with allow-lan")
	}
	if out, _ := exec.Command("ip", "netns", "exec", ns, "nft", "list", "ruleset").CombinedOutput(); strings.Count(string(out), "table inet tsroam") != 1 {
		t.Errorf("ruleset:\n%s", out)
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

func TestOthersTablesAreLeftAlone(t *testing.T) {
	ns, s := lab(t)
	if out, err := exec.Command("ip", "netns", "exec", ns, "nft", "add", "table", "inet", "someone").CombinedOutput(); err != nil {
		t.Skipf("nft: %v %s", err, out)
	}
	s.On(Rules{Tun: "tailscale0", Mark: 0x80000, Mask: 0xff0000})
	s.Off()
	if out, _ := exec.Command("ip", "netns", "exec", ns, "nft", "list", "tables").CombinedOutput(); !strings.Contains(string(out), "table inet someone") {
		t.Errorf("tables:\n%s", out)
	}
}
