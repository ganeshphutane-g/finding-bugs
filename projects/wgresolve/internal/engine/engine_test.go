package engine

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	t0     = time.Unix(1_000_000, 0)
	a1, a2 = netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2")
)

func ep(a netip.Addr) netip.AddrPort { return netip.AddrPortFrom(a, 51820) }

func TestWorkingByCounters(t *testing.T) {
	// A session the other side started, renewed every 165 s, with only
	// keepalives from this side: the handshake is often older than 135 s,
	// but bytes keep arriving with each renewal and reply.
	var m Memory
	p := Peer{Endpoint: ep(a2), LastHandshake: t0, Rx: 100}
	m = Observe(t0, p, m)
	for s := 30; s <= 600; s += 30 {
		now := t0.Add(time.Duration(s) * time.Second)
		if s%165 < 30 { // a renewal: a handshake and bytes
			p.LastHandshake, p.Rx = now, p.Rx+92
		}
		m = Observe(now, p, m)
		if !Working(now, m, DefaultStale) {
			t.Fatalf("%ds: a working peer counts as quiet", s)
		}
	}
	// Nothing arrives any more: quiet after the stale window.
	now := t0.Add(600 * time.Second)
	for s := 0; s <= 200; s += 30 {
		m = Observe(now.Add(time.Duration(s)*time.Second), p, m)
	}
	if Working(now.Add(200*time.Second), m, DefaultStale) {
		t.Fatal("a dead peer counts as working")
	}
}

func TestClockBack(t *testing.T) {
	// The clock stepped back: a handshake two hours "in the future" says
	// nothing about the peer.
	m := Observe(t0, Peer{Endpoint: ep(a1), LastHandshake: t0.Add(2 * time.Hour)}, Memory{})
	if Working(t0, m, DefaultStale) {
		t.Fatal("a future handshake counts as fresh")
	}
}

func TestDecide(t *testing.T) {
	quiet := func() Memory { return Observe(t0, Peer{LastHandshake: t0.Add(-time.Hour)}, Memory{}) }
	now := t0
	ans := func(as ...netip.Addr) Answer { return Answer{Addrs: as} }
	for _, c := range []struct {
		name string
		p    Peer
		m    Memory
		a    Answer
		set  netip.AddrPort
		why  string
	}{
		{"no endpoint", Peer{}, quiet(), ans(a1), ep(a1), "no endpoint"},
		{"quiet, address changed", Peer{Endpoint: ep(a2)}, quiet(), ans(a1), ep(a1), "not what the name resolves to"},
		{"quiet, port changed", Peer{Endpoint: netip.AddrPortFrom(a1, 4000)}, quiet(), ans(a1), ep(a1), "not what the name"},
		{"quiet, right address, peer down", Peer{Endpoint: ep(a1)}, quiet(), ans(a1), netip.AddrPort{}, "may be down"},
		{"v4-mapped endpoint matches", Peer{Endpoint: netip.AddrPortFrom(netip.AddrFrom16(a1.As16()), 51820)}, quiet(), ans(a1), netip.AddrPort{}, "may be down"},
		{"partial answer: the failed family may be right", Peer{Endpoint: ep(a1)}, quiet(), Answer{Addrs: []netip.Addr{netip.MustParseAddr("2001:db8::1")}, Failed: []string{"ipv4"}}, netip.AddrPort{}, "lookup failed"},
		{"no addresses", Peer{Endpoint: ep(a2)}, quiet(), ans(), netip.AddrPort{}, "no usable addresses"},
	} {
		m := c.m
		d := Decide(now, c.p, &m, c.a, 51820, 0)
		if d.Set != c.set || !strings.Contains(d.Reason, c.why) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestRotation(t *testing.T) {
	// The endpoint is the name's first address and nothing comes back:
	// with packets sent, the next address is tried, once through the list.
	p := Peer{Endpoint: ep(a1), Tx: 100}
	m := Observe(t0, p, Memory{})
	m = Changed(t0, p, m, ep(a1))
	now := t0.Add(DefaultStale + time.Second)
	p.Tx = 500 // sent, nothing received
	m = Observe(now, p, m)
	d := Decide(now, p, &m, Answer{Addrs: []netip.Addr{a1, a2}}, 51820, 0)
	if d.Set != ep(a2) {
		t.Fatalf("%+v", d)
	}
	m = Changed(now, p, m, ep(a2))
	// Still nothing: the pass is over, no more flipping.
	p.Endpoint, p.Tx = ep(a2), 900
	now = now.Add(DefaultStale + time.Second)
	m = Observe(now, p, m)
	if d := Decide(now, p, &m, Answer{Addrs: []netip.Addr{a1, a2}}, 51820, 0); d.Set.IsValid() {
		t.Fatalf("flipped again: %+v", d)
	}
	// An idle peer (nothing sent) is not rotated at all.
	q := Peer{Endpoint: ep(a1), Tx: 100}
	mq := Observe(t0, q, Memory{})
	now = t0.Add(time.Hour)
	mq = Observe(now, q, mq)
	if d := Decide(now, q, &mq, Answer{Addrs: []netip.Addr{a1, a2}}, 51820, 0); d.Set.IsValid() {
		t.Fatalf("an idle peer was rotated: %+v", d)
	}
	// Waiting after a change.
	r := Peer{Endpoint: ep(a1)}
	mr := Changed(t0, r, Observe(t0, r, Memory{}), ep(a1))
	if d := Decide(t0.Add(time.Minute), r, &mr, Answer{Addrs: []netip.Addr{a2}}, 51820, 0); !d.Waiting || d.Set.IsValid() {
		t.Fatalf("%+v", d)
	}
}
