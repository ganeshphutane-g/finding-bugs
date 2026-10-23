package sys

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestHtons(t *testing.T) {
	// htons(v) must lay v out in memory big-endian, on any CPU.
	b := binary.NativeEndian.AppendUint16(nil, htons(0x0806))
	if b[0] != 0x08 || b[1] != 0x06 {
		t.Errorf("htons(0x0806) is % x in memory", b)
	}
}

func TestARP(t *testing.T) {
	me := net.HardwareAddr{2, 0, 0, 0, 0, 0x30}
	gw := netip.MustParseAddr("192.168.1.1")
	req := arpRequest(me, netip.MustParseAddr("192.168.1.30"), gw)
	want := []byte{0, 1, 8, 0, 6, 4, 0, 1, 2, 0, 0, 0, 0, 0x30, 192, 168, 1, 30, 0, 0, 0, 0, 0, 0, 192, 168, 1, 1}
	if string(req) != string(want) {
		t.Fatalf("request\n% x, want\n% x", req, want)
	}
	// The gateway's reply: op 2, sender = the gateway.
	reply := []byte{0, 1, 8, 0, 6, 4, 0, 2, 0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56, 192, 168, 1, 1, 2, 0, 0, 0, 0, 0x30, 192, 168, 1, 30}
	mac, ok := arpReplyFrom(reply, gw)
	if !ok || mac.String() != "3c:7c:3f:12:34:56" {
		t.Errorf("reply: %v %v", mac, ok)
	}
	reply[0x10], reply[0x11] = 1, 2 // from 192.168.1.2
	if _, ok := arpReplyFrom(reply, gw); ok {
		t.Error("a reply from another address counted")
	}
	if _, ok := arpReplyFrom(req, gw); ok {
		t.Error("a request counted as a reply")
	}
	if _, ok := arpReplyFrom(reply[:20], gw); ok {
		t.Error("a short packet counted")
	}
}

func TestPick(t *testing.T) {
	// r is a route: "PREFIX dev [down] [via]".
	r := func(spec string) route {
		f := strings.Fields(spec)
		x := route{dst: netip.MustParsePrefix(f[0]), name: f[1], index: int(f[1][len(f[1])-1]), connected: true, usable: true}
		for _, o := range f[2:] {
			switch o {
			case "down":
				x.usable = false
			case "via":
				x.connected = false
			}
		}
		return x
	}
	def := r("0.0.0.0/0 wlan0 via")
	for _, c := range []struct {
		name   string
		routes []route
		lan    string
		want   string // the interface, "" for none
		ok     bool
		clash  string
	}{
		{"home", []route{def, r("192.168.1.0/24 wlan0")}, "192.168.1.0/24", "wlan0", true, ""},
		{"a wider connected network", []route{r("10.0.0.0/8 eth1")}, "10.1.0.0/16", "eth1", true, ""},
		{"the most specific covering one", []route{r("10.0.0.0/8 eth1"), r("10.1.0.0/16 eth2")}, "10.1.2.0/24", "eth2", true, ""},
		{"not here: only the default route", []route{def}, "192.168.1.0/24", "", false, ""},
		{"partly here", []route{def, r("192.168.1.0/25 wlan0"), r("192.168.1.0/26 eth1")}, "192.168.1.0/24", "wlan0", false, ""},
		// The kernel uses a linkdown route too: no skipping to another.
		{"its link is down", []route{r("192.168.1.0/24 eth0 down"), r("192.168.1.0/24 eth1"), r("192.168.0.0/16 eth2")}, "192.168.1.0/24", "", false, ""},
		{"the same prefix on two links", []route{r("192.168.1.0/24 eth0"), r("192.168.1.0/24 eth1")}, "192.168.1.0/24", "eth0", true, "sends 192.168.1.0/24 to eth1"},
		{"part of it on another link", []route{r("192.168.1.0/24 eth0"), r("192.168.1.0/25 wlan1")}, "192.168.1.0/24", "eth0", true, "sends 192.168.1.0/25 to wlan1"},
		{"part of it via another link's gateway", []route{r("192.168.1.0/24 eth0"), r("192.168.1.128/25 wlan1 via")}, "192.168.1.0/24", "eth0", true, "192.168.1.128/25 to wlan1"},
		{"part of it via the LAN's own gateway", []route{r("192.168.1.0/24 eth0"), r("192.168.1.128/25 eth0 via")}, "192.168.1.0/24", "eth0", true, ""},
		{"a route covering only another part of a wider network", []route{r("10.0.0.0/8 eth1"), r("10.9.0.0/16 eth2")}, "10.1.0.0/16", "eth1", true, ""},
	} {
		got, ok, clash := pick(c.routes, netip.MustParsePrefix(c.lan))
		if got.name != c.want || ok != c.ok || (c.clash == "") != (clash == "") || !strings.Contains(clash, c.clash) {
			t.Errorf("%s: %q %v %q, want %q %v %q", c.name, got.name, ok, clash, c.want, c.ok, c.clash)
		}
	}
}

func TestPrefixOf(t *testing.T) {
	for _, n := range []*net.IPNet{
		{IP: net.IPv4(192, 168, 1, 7).To4(), Mask: net.CIDRMask(24, 32)},
		{IP: net.IPv4(192, 168, 1, 7), Mask: net.CIDRMask(120, 128)}, // the 16-byte form
	} {
		if p, ok := prefixOf(n); !ok || p.String() != "192.168.1.0/24" {
			t.Errorf("%v: %v %v", n, p, ok)
		}
	}
}

func TestKernel(t *testing.T) {
	for v, ok := range map[string]bool{
		"6.18.44-fc-v153": true, "4.17.0": true, "5.4.0-150-generic": true,
		"4.15.0-213-generic": false, "3.10.0-1160.el7.x86_64": false, "garbage": false,
	} {
		if err := kernelOK(v); (err == nil) != ok {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func TestProbeRefusesOwnAddress(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	ap := netip.MustParseAddrPort(ln.Addr().String())
	err = (&Linux{}).Probe(context.Background(), "lo", ap)
	if err == nil || !strings.Contains(err.Error(), "this machine's own address") {
		t.Errorf("probe to a local listener: %v", err)
	}
}
