package sys

import (
	"encoding/binary"
	"net"
	"net/netip"
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
	r := func(p string, usable bool) route {
		return route{dst: netip.MustParsePrefix(p), name: p, usable: usable}
	}
	routes := []route{r("10.0.0.0/8", true), r("192.168.0.0/16", true), r("192.168.1.0/24", true), r("192.168.1.0/25", true), r("172.16.0.0/24", false)}
	for _, c := range []struct{ lan, want string }{
		{"192.168.1.0/24", "192.168.1.0/24"},   // the most specific covering
		{"192.168.1.0/25", "192.168.1.0/25"},   //
		{"192.168.1.128/25", "192.168.1.0/24"}, // covered by the /24
		{"10.1.0.0/16", "10.0.0.0/8"},          // a wider connected network
		{"172.16.0.0/24", ""},                  // its link is down
		{"192.168.0.0/15", "192.168.0.0/16"},   // only partly there: the widest inside
		{"8.8.8.0/24", ""},                     // not here
	} {
		got, ok := pick(routes, netip.MustParsePrefix(c.lan))
		if !ok && c.want != "" || ok && got.name != c.want {
			t.Errorf("%s: %q %v, want %q", c.lan, got.name, ok, c.want)
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
