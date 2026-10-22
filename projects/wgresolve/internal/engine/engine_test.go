package engine

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	a1, a2 := netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")
	ep := func(a netip.Addr, port uint16) netip.AddrPort { return netip.AddrPortFrom(a, port) }
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, c := range []struct {
		name  string
		p     Peer
		m     Memory
		addrs []netip.Addr
		set   netip.AddrPort
		why   string
	}{
		{"no endpoint", Peer{}, Memory{}, []netip.Addr{a1}, ep(a1, 51820), "no endpoint"},
		{"fresh, DNS differs: roaming wins", Peer{ep(a2, 51820), ago(time.Minute)}, Memory{}, []netip.Addr{a1}, netip.AddrPort{}, "working"},
		{"stale, address changed", Peer{ep(a2, 51820), ago(10 * time.Minute)}, Memory{}, []netip.Addr{a1}, ep(a1, 51820), "not what the name resolves to"},
		{"never handshaked, wrong address", Peer{ep(a2, 51820), time.Time{}}, Memory{}, []netip.Addr{a1}, ep(a1, 51820), "no handshake yet"},
		{"stale, port changed", Peer{ep(a1, 4000), ago(10 * time.Minute)}, Memory{}, []netip.Addr{a1}, ep(a1, 51820), "not what the name"},
		{"stale, right address: peer down", Peer{ep(a1, 51820), ago(10 * time.Minute)}, Memory{}, []netip.Addr{a1}, netip.AddrPort{}, "may be down"},
		{"just changed: wait", Peer{ep(a1, 51820), ago(10 * time.Minute)}, Memory{SetAt: ago(time.Minute), Set: ep(a1, 51820)}, []netip.Addr{a1, a2}, netip.AddrPort{}, "waiting"},
		{"changed, still stale: next address", Peer{ep(a1, 51820), ago(10 * time.Minute)}, Memory{SetAt: ago(3 * time.Minute), Set: ep(a1, 51820)}, []netip.Addr{a1, a2}, ep(a2, 51820), "next address"},
		{"next wraps around", Peer{ep(a2, 51820), ago(10 * time.Minute)}, Memory{SetAt: ago(3 * time.Minute), Set: ep(a2, 51820)}, []netip.Addr{a1, a2}, ep(a1, 51820), "next address"},
		{"only address, still stale", Peer{ep(a1, 51820), ago(10 * time.Minute)}, Memory{SetAt: ago(3 * time.Minute), Set: ep(a1, 51820)}, []netip.Addr{a1}, netip.AddrPort{}, "only address"},
		{"no addresses", Peer{ep(a2, 51820), ago(10 * time.Minute)}, Memory{}, nil, netip.AddrPort{}, "no addresses"},
		{"v4-mapped endpoint matches", Peer{netip.AddrPortFrom(netip.AddrFrom16(a1.As16()), 51820), ago(10 * time.Minute)}, Memory{}, []netip.Addr{a1}, netip.AddrPort{}, "may be down"},
	} {
		d := Decide(now, c.p, c.m, c.addrs, 51820, 0)
		if d.Set != c.set || !strings.Contains(d.Reason, c.why) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}
