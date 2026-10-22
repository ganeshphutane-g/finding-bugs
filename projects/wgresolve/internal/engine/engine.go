// Package engine decides when a peer's endpoint should change. The rule is
// conservative: a peer whose handshake is fresh is working and is never
// touched (WireGuard may have roamed to the address the peer really uses);
// only a peer that has gone quiet, or never had an endpoint, is pointed at
// what its name resolves to now.
package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// DefaultStale is how old a handshake may be before the peer counts as not
// working: WireGuard re-handshakes every 2 minutes while traffic flows, and
// gives up on a handshake attempt after 5 s plus jitter (the 135 s the
// wireguard-tools script uses).
const DefaultStale = 135 * time.Second

// Peer is what the device says about a peer now.
type Peer struct {
	Endpoint      netip.AddrPort // zero when it has none
	LastHandshake time.Time      // zero when it never had one
}

// Memory is what the engine remembers about a peer between rounds.
type Memory struct {
	// SetAt and Set are the last change made, if any.
	SetAt time.Time
	Set   netip.AddrPort
}

// Decision is what to do.
type Decision struct {
	// Set is the new endpoint; zero means leave it.
	Set    netip.AddrPort
	Reason string
	// Retry: the change did not help, look the name up again before the TTL
	// runs out.
	Retry bool
}

// Decide applies the rules. addrs are the name's addresses, best first.
func Decide(now time.Time, p Peer, m Memory, addrs []netip.Addr, port uint16, stale time.Duration) Decision {
	if len(addrs) == 0 {
		return Decision{Reason: "the name has no addresses; nothing to change to"}
	}
	if stale <= 0 {
		stale = DefaultStale
	}
	first := netip.AddrPortFrom(addrs[0], port)
	if !p.Endpoint.IsValid() {
		return Decision{Set: first, Reason: "it has no endpoint yet (the name did not resolve when it was set up)"}
	}
	age := now.Sub(p.LastHandshake)
	if !p.LastHandshake.IsZero() && age < stale {
		return Decision{Reason: fmt.Sprintf("working (handshake %s ago)", age.Round(time.Second))}
	}
	quiet := "no handshake yet"
	if !p.LastHandshake.IsZero() {
		quiet = fmt.Sprintf("no handshake for %s", age.Round(time.Second))
	}
	i := slices.IndexFunc(addrs, func(a netip.Addr) bool { return a == p.Endpoint.Addr().Unmap() })
	switch {
	case i < 0 || p.Endpoint.Port() != port:
		return Decision{Set: first, Reason: fmt.Sprintf("%s, and %s is not what the name resolves to now", quiet, p.Endpoint)}
	case m.Set == p.Endpoint && now.Sub(m.SetAt) < stale:
		return Decision{Reason: fmt.Sprintf("%s; changed to %s %s ago, waiting for a handshake", quiet, p.Endpoint, now.Sub(m.SetAt).Round(time.Second))}
	case m.Set == p.Endpoint && len(addrs) > 1:
		next := netip.AddrPortFrom(addrs[(i+1)%len(addrs)], port)
		return Decision{Set: next, Retry: true, Reason: fmt.Sprintf("%s since changing to %s; trying the name's next address", quiet, p.Endpoint)}
	case m.Set == p.Endpoint:
		return Decision{Retry: true, Reason: fmt.Sprintf("%s at %s, the name's only address: the peer itself may be down", quiet, p.Endpoint)}
	}
	return Decision{Reason: fmt.Sprintf("%s, but %s is what the name resolves to: the peer itself may be down", quiet, p.Endpoint)}
}
