// Package sys is what tsroam needs from Linux to tell which network the
// machine is on: the default route, connected routes, the gateway's MAC
// (asked fresh, with ARP), probes from a given interface, and change
// events. IPv4 only.
package sys

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Linux is the real system.
type Linux struct {
	// Skip says which interfaces are tunnels, not LANs (tailscale0).
	Skip func(name string) bool
}

// route is a main-table route, as far as Connected cares.
type route struct {
	dst   netip.Prefix
	index int
	name  string
	// connected: directly connected (scope link, no gateway, unicast).
	connected bool
	// usable: the link is up and the route not marked linkdown.
	usable bool
}

// ConflictError says the prefix is connected, but other main-table routes
// would take some of its traffic once it is sent to the main table.
type ConflictError struct {
	Iface     string
	Index     int
	Connected netip.Prefix
	Why       string
}

func (e *ConflictError) Error() string { return e.Why }

// pick finds the route the kernel uses for p in the main table: the most
// specific one covering p. It must be a connected route on a usable link,
// and no other route may take part of p elsewhere: one with the same
// prefix on another interface, or a more specific one inside p that is not
// on the same interface (the rule would hand that traffic to it). With no
// connected route covering p, the widest connected one inside p is
// returned with ok false, so the caller can say p is only partly there.
func pick(routes []route, p netip.Prefix) (best route, ok bool, conflict string) {
	found := false
	for _, r := range routes {
		if r.dst.Bits() > p.Bits() || !r.dst.Contains(p.Addr()) {
			continue
		}
		if !found || r.dst.Bits() > best.dst.Bits() { // ties: the kernel's first
			best, found = r, true
		}
	}
	if !found || !best.connected || !best.usable {
		// Not on p (or its link is down): report a usable connected route
		// inside p, if any, as partial.
		var part route
		for _, r := range routes {
			if r.connected && r.usable && r.dst.Bits() > p.Bits() && p.Contains(r.dst.Addr()) && (part.name == "" || r.dst.Bits() < part.dst.Bits()) {
				part = r
			}
		}
		return part, false, ""
	}
	for _, r := range routes {
		if r.index == best.index {
			continue
		}
		if r.dst == best.dst || r.dst.Bits() > best.dst.Bits() && r.dst.Overlaps(p) {
			return best, true, fmt.Sprintf("the main table also sends %s to %s, not %s", r.dst, r.name, best.name)
		}
	}
	return best, true, ""
}

// Connected finds the interface whose directly connected route in the main
// table covers p (see pick). A *ConflictError means it does, but the rule
// would not keep all of p on it.
func (s *Linux) Connected(p netip.Prefix) (iface string, index int, connected netip.Prefix, err error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return "", 0, netip.Prefix{}, err
	}
	type link struct {
		name string
		up   bool
	}
	links := map[int]link{}
	var cands []route
	for _, r := range routes {
		dst := netip.PrefixFrom(netip.IPv4Unspecified(), 0) // the default route
		if r.Dst != nil {
			var ok bool
			if dst, ok = prefixOf(r.Dst); !ok || !dst.Addr().Is4() {
				continue
			}
		}
		l, seen := links[r.LinkIndex]
		if !seen && r.LinkIndex != 0 {
			if nl, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
				// Up, or unknown (interfaces that do not report it); not
				// down, not without carrier, not dormant.
				st := nl.Attrs().OperState
				l = link{name: nl.Attrs().Name, up: st == netlink.OperUp || st == netlink.OperUnknown}
			}
			links[r.LinkIndex] = l
		}
		name, up := l.name, l.up
		if name == "" {
			name = fmt.Sprintf("a %s route", routeKind(r.Type))
		}
		c := route{dst: dst, index: r.LinkIndex, name: name}
		c.connected = r.Scope == netlink.SCOPE_LINK && r.Gw == nil && r.Type == unix.RTN_UNICAST && len(r.MultiPath) == 0 &&
			r.LinkIndex != 0 && !(s.Skip != nil && s.Skip(name))
		c.usable = up && r.Flags&unix.RTNH_F_LINKDOWN == 0
		cands = append(cands, c)
	}
	r, ok, conflict := pick(cands, p)
	switch {
	case conflict != "":
		return r.name, r.index, r.dst, &ConflictError{Iface: r.name, Index: r.index, Connected: r.dst, Why: conflict}
	case ok, r.name != "":
		return r.name, r.index, r.dst, nil
	}
	return "", 0, netip.Prefix{}, nil
}

func routeKind(t int) string {
	switch t {
	case unix.RTN_BLACKHOLE:
		return "blackhole"
	case unix.RTN_UNREACHABLE:
		return "unreachable"
	case unix.RTN_PROHIBIT:
		return "prohibit"
	}
	return "multipath"
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, bits := n.Mask.Size()
	if bits == 0 {
		return netip.Prefix{}, false
	}
	a = a.Unmap()
	if a.Is4() && bits == 128 {
		ones -= 96
	}
	return netip.PrefixFrom(a, ones).Masked(), true
}

// DefaultRoute is the main table's IPv4 default route the kernel uses (the
// lowest metric, the first on a tie), outside Tailscale's interfaces: the
// network the machine's traffic leaves on. ok is false when there is none
// (offline). A multipath default route is reported with iface "".
func (s *Linux) DefaultRoute() (iface string, index int, gateway netip.Addr, ok bool, err error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return "", 0, netip.Addr{}, false, err
	}
	var best *netlink.Route
	for i := range routes {
		r := &routes[i]
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		if r.Type != unix.RTN_UNICAST {
			continue
		}
		if r.LinkIndex != 0 && s.Skip != nil {
			if l, err := netlink.LinkByIndex(r.LinkIndex); err == nil && s.Skip(l.Attrs().Name) {
				continue
			}
		}
		if best == nil || r.Priority < best.Priority {
			best = r
		}
	}
	if best == nil {
		return "", 0, netip.Addr{}, false, nil
	}
	if len(best.MultiPath) > 0 || best.LinkIndex == 0 {
		return "", 0, netip.Addr{}, true, nil
	}
	l, err := netlink.LinkByIndex(best.LinkIndex)
	if err != nil {
		return "", 0, netip.Addr{}, false, err
	}
	if a, ok := netip.AddrFromSlice(best.Gw); ok {
		gateway = a.Unmap()
	}
	return l.Attrs().Name, best.LinkIndex, gateway, true, nil
}

// DefaultGateway is the main table's default gateway through the link, if
// any.
func (s *Linux) DefaultGateway(index int) (netip.Addr, bool) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
	if err != nil {
		return netip.Addr{}, false
	}
	for _, r := range routes {
		if r.Gw == nil {
			continue
		}
		if r.Dst != nil {
			if ones, _ := r.Dst.Mask.Size(); ones != 0 {
				continue
			}
		}
		if a, ok := netip.AddrFromSlice(r.Gw); ok {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// GatewayMAC asks the address for its MAC now, with an ARP request: the
// kernel's neighbour table can still hold the last network's entry.
func (s *Linux) GatewayMAC(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	if !addr.Is4() {
		return nil, errors.New("only IPv4 gateways can be checked")
	}
	return arpAsk(ctx, iface, addr)
}

// Probe makes a TCP connection to ap from iface (bound to it, so not
// through the tailnet).
func (s *Linux) Probe(ctx context.Context, iface string, ap netip.AddrPort) error {
	// A connection to one of the machine's own addresses goes through
	// loopback whatever the binding: on a network that gave the machine the
	// probe's address, it would answer itself.
	if own, err := isOwn(ap.Addr()); err != nil {
		return err
	} else if own {
		return fmt.Errorf("%s is this machine's own address now", ap.Addr())
	}
	d := net.Dialer{Timeout: 3 * time.Second, Control: bindTo(iface)}
	c, err := d.DialContext(ctx, "tcp4", ap.String())
	if err != nil {
		return err
	}
	return c.Close()
}

func bindTo(iface string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
		}); err != nil {
			return err
		}
		return serr
	}
}

func isOwn(a netip.Addr) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, err
	}
	for _, x := range addrs {
		if n, ok := x.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a {
				return true, nil
			}
		}
	}
	return false, nil
}

// Watch sends on the channel when IPv4 addresses, routes or links change,
// until ctx ends. If a subscription fails (the kernel dropped events), the
// channel is closed: the caller can no longer rely on it.
func (s *Linux) Watch(ctx context.Context) (<-chan struct{}, error) {
	out := make(chan struct{}, 1)
	poke := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	// The subscriptions end with ctx, or on an error; either ends Watch.
	sub, stop := context.WithCancel(ctx)
	done := sub.Done()
	// Route bursts (Tailscale's table 52) are large: a big buffer, beyond
	// rmem_max when allowed (CAP_NET_ADMIN), else as big as allowed.
	const rcvbuf = 1 << 20
	routes := make(chan netlink.RouteUpdate, 64)
	addrs := make(chan netlink.AddrUpdate, 16)
	links := make(chan netlink.LinkUpdate, 16)
	subscribe := func(force bool) error {
		if err := netlink.RouteSubscribeWithOptions(routes, done, netlink.RouteSubscribeOptions{ReceiveBufferSize: rcvbuf, ReceiveBufferForceSize: force}); err != nil {
			return err
		}
		if err := netlink.AddrSubscribeWithOptions(addrs, done, netlink.AddrSubscribeOptions{ReceiveBufferSize: rcvbuf, ReceiveBufferForceSize: force}); err != nil {
			return err
		}
		return netlink.LinkSubscribeWithOptions(links, done, netlink.LinkSubscribeOptions{ReceiveBufferSize: rcvbuf, ReceiveBufferForceSize: force})
	}
	if err := subscribe(true); err != nil {
		// The subscriptions made so far end with sub; start again without
		// forcing.
		stop()
		sub, stop = context.WithCancel(ctx)
		done = sub.Done()
		routes = make(chan netlink.RouteUpdate, 64)
		addrs = make(chan netlink.AddrUpdate, 16)
		links = make(chan netlink.LinkUpdate, 16)
		if err := subscribe(false); err != nil {
			stop()
			return nil, err
		}
	}
	go func() {
		defer close(out)
		defer stop()
		for {
			select {
			case <-done:
				return
			case u, ok := <-routes:
				if !ok {
					return
				}
				if u.Table == unix.RT_TABLE_MAIN {
					poke()
				}
			case _, ok := <-addrs:
				if !ok {
					return
				}
				poke()
			case _, ok := <-links:
				if !ok {
					return
				}
				poke()
			}
		}
	}()
	return out, nil
}

// IsTailscale is the default Skip: Tailscale's interface names.
func IsTailscale(name string) bool { return strings.HasPrefix(name, "tailscale") }

// ErrNoAnswer means the address did not answer.
var ErrNoAnswer = errors.New("no answer")

// ifaceAddr is the interface and its IPv4 address on the same network as
// peer, to ask from.
func ifaceAddr(iface string, peer netip.Addr) (*net.Interface, netip.Addr, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, netip.Addr{}, err
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		p, ok := prefixOf(n)
		if !ok || !p.Addr().Is4() || !p.Contains(peer) {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP); ok {
			return ifi, ip.Unmap(), nil
		}
	}
	return nil, netip.Addr{}, fmt.Errorf("%s has no address on %s's network to ask from", iface, peer)
}
