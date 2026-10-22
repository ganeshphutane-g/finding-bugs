// Package engine decides when a peer's endpoint should change. The rule is
// conservative: a peer that is working is never touched (WireGuard may have
// roamed to the address the peer really uses); only a peer that has gone
// quiet, or never had an endpoint, is pointed at what its name resolves to
// now.
//
// "Working" is judged by what arrives from the peer: its receive counter
// growing between rounds, or a handshake younger than the stale window.
// Handshake age alone is not enough: a session the other side started is
// renewed only every 165 s when this side just sends keepalives, and an idle
// tunnel has no handshakes at all.
package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// DefaultStale is how long nothing may arrive from a peer before it counts
// as not working: past WireGuard's 180 s session lifetime, after which no
// traffic can flow without a new handshake.
const DefaultStale = 190 * time.Second

// Peer is what the device says about a peer now.
type Peer struct {
	Endpoint      netip.AddrPort // zero when it has none
	LastHandshake time.Time      // zero when it never had one
	Rx, Tx        int64          // bytes received from and sent to it
}

// Memory is what the engine remembers about a peer between rounds.
type Memory struct {
	seen bool
	rx   int64
	// Active is when something last arrived from the peer, as far as can be
	// told (a handshake, or its receive counter growing).
	Active time.Time
	// SetAt and Set are the last change made, if any; txMark is the send
	// counter then (or when watching began).
	SetAt  time.Time
	Set    netip.AddrPort
	txMark int64
	// tried counts addresses tried without a reply, for one pass through
	// the name's addresses; answer is the addresses that pass is over.
	tried  int
	answer string
}

// Observe updates the memory with the device's view. Call it every round.
func Observe(now time.Time, p Peer, m Memory) Memory {
	hs := p.LastHandshake
	if hs.After(now.Add(time.Minute)) {
		hs = time.Time{} // the clock went back: the handshake time says nothing
	}
	if hs.After(m.Active) {
		m.Active = hs
	}
	if m.seen && p.Rx > m.rx {
		m.Active = now
	}
	if !m.seen {
		m.txMark = p.Tx
	}
	m.seen, m.rx = true, p.Rx
	return m
}

// Working reports whether something arrived from the peer within stale.
func Working(now time.Time, m Memory, stale time.Duration) bool {
	return !m.Active.IsZero() && now.Sub(m.Active) < stale
}

// Decision is what to do.
type Decision struct {
	// Set is the new endpoint; zero means leave it.
	Set    netip.AddrPort
	Reason string
	// Waiting: a change was made and its effect is not known yet.
	Waiting bool
}

// Changed records a change made.
func Changed(now time.Time, p Peer, m Memory, to netip.AddrPort) Memory {
	m.SetAt, m.Set, m.txMark = now, to, p.Tx
	return m
}

// Answer is what the name resolves to, for Decide.
type Answer struct {
	Addrs []netip.Addr
	// Failed lists the address families whose lookup failed (a partial
	// answer): an endpoint in such a family may still be right.
	Failed []string
}

func family(a netip.Addr) string {
	if a.Unmap().Is4() {
		return "ipv4"
	}
	return "ipv6"
}

// Decide applies the rules to a peer that is not working (Working false),
// or has no endpoint.
func Decide(now time.Time, p Peer, m *Memory, ans Answer, port uint16, stale time.Duration) Decision {
	if stale <= 0 {
		stale = DefaultStale
	}
	addrs := ans.Addrs
	if len(addrs) == 0 {
		return Decision{Reason: "the name has no usable addresses; nothing to change to"}
	}
	key := fmt.Sprint(addrs)
	if m.answer != key {
		m.answer, m.tried = key, 0 // a new answer: a new pass
	}
	first := netip.AddrPortFrom(addrs[0], port)
	if !p.Endpoint.IsValid() {
		return Decision{Set: first, Reason: "it has no endpoint yet (the name did not resolve when it was set up)"}
	}
	ep := netip.AddrPortFrom(p.Endpoint.Addr().Unmap(), p.Endpoint.Port())
	quiet := "nothing received from it yet"
	if !m.Active.IsZero() {
		quiet = fmt.Sprintf("nothing received from it for %s", now.Sub(m.Active).Round(time.Second))
	}
	if Working(now, *m, stale) {
		return Decision{Reason: "working"}
	}
	if m.Set == ep && now.Sub(m.SetAt) < stale {
		return Decision{Waiting: true, Reason: fmt.Sprintf("%s; changed to %s %s ago, waiting for it to answer", quiet, ep, now.Sub(m.SetAt).Round(time.Second))}
	}
	i := slices.Index(addrs, ep.Addr())
	if i < 0 || ep.Port() != port {
		if slices.Contains(ans.Failed, family(ep.Addr())) {
			return Decision{Reason: fmt.Sprintf("%s, but the name's %s lookup failed, so %s may still be right", quiet, family(ep.Addr()), ep)}
		}
		return Decision{Set: first, Reason: fmt.Sprintf("%s, and %s is not what the name resolves to now", quiet, ep)}
	}
	// The endpoint is one of the name's addresses and gets no answer. With
	// several addresses, try the next, once through the list, and only if
	// packets were sent there and none came back.
	if len(addrs) > 1 && m.tried < len(addrs)-1 && p.Tx > m.txMark {
		m.tried++
		next := netip.AddrPortFrom(addrs[(i+1)%len(addrs)], port)
		return Decision{Set: next, Reason: fmt.Sprintf("%s at %s; trying the name's next address", quiet, ep)}
	}
	return Decision{Reason: fmt.Sprintf("%s, at %s, which is what the name resolves to: the peer itself may be down", quiet, ep)}
}
