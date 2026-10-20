package ratelimit

import (
	"net/netip"
	"testing"
	"time"
)

func TestAllow(t *testing.T) {
	l := New(5, 24, 56, 100)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	count := func(ip string, n int) int {
		ok := 0
		for range n {
			if l.Allow(netip.MustParseAddr(ip)) {
				ok++
			}
		}
		return ok
	}
	if got := count("192.0.2.10", 20); got != 5 {
		t.Fatalf("a burst: %d allowed", got)
	}
	// The same /24, and the same address mapped into IPv6: the same bucket.
	if count("192.0.2.99", 1) != 0 || count("::ffff:192.0.2.10", 1) != 0 {
		t.Fatal("the same subnet had its own bucket")
	}
	if count("192.0.3.10", 5) != 5 {
		t.Fatal("another subnet was limited")
	}
	now = now.Add(time.Second / 5)
	if got := count("192.0.2.10", 5); got != 1 {
		t.Fatalf("after 0.2s: %d allowed", got)
	}
	now = now.Add(time.Hour)
	if got := count("192.0.2.10", 20); got != 5 {
		t.Fatalf("after a long wait: %d allowed (the burst must not grow)", got)
	}
	if count("2001:db8:0:1::1", 5) != 5 || count("2001:db8:0:2::1", 1) != 0 || count("2001:db8:1::1", 1) != 1 {
		t.Fatal("IPv6 is not counted by /56")
	}
}

func TestBounded(t *testing.T) {
	l := New(1, 32, 128, 10)
	for i := range 1000 {
		l.Allow(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}))
	}
	if l.Len() > 20 {
		t.Fatalf("%d subnets kept", l.Len())
	}
	// A subnet from the older generation keeps its count.
	a := netip.MustParseAddr("10.9.9.9")
	l.Allow(a)
	for i := range 10 {
		l.Allow(netip.AddrFrom4([4]byte{10, 1, 0, byte(i)}))
	}
	if l.Allow(a) {
		t.Fatal("moving generations reset a bucket")
	}
}

func TestOff(t *testing.T) {
	l := New(0, 24, 56, 1)
	for range 100 {
		if !l.Allow(netip.MustParseAddr("192.0.2.1")) {
			t.Fatal("limited with rate 0")
		}
	}
	var nl *Limiter
	if !nl.Allow(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("a nil limiter limited")
	}
}
