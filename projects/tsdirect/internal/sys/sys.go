// Package sys is what tsdirect needs from Linux: connected routes, the
// gateway's MAC (asked fresh, with ARP), probes from a given interface, its
// own ip rules, and change events.
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
	// Proto marks tsdirect's rules (FRA_PROTOCOL), so it finds exactly its
	// own.
	Proto uint8
	// Skip says which interfaces are tunnels, not LANs (tailscale0).
	Skip func(name string) bool
}

// Connected finds the interface whose directly connected route in the main
// table covers p.
func (s *Linux) Connected(p netip.Prefix) (iface string, index int, connected netip.Prefix, err error) {
	family := netlink.FAMILY_V4
	if p.Addr().Is6() {
		family = netlink.FAMILY_V6
	}
	routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return "", 0, netip.Prefix{}, err
	}
	best := -1
	for _, r := range routes {
		if r.Dst == nil || r.Scope != netlink.SCOPE_LINK || r.Gw != nil || r.Type != unix.RTN_UNICAST {
			continue
		}
		dst, ok := prefixOf(r.Dst)
		if !ok || dst.Bits() > p.Bits() || !dst.Contains(p.Addr()) || dst.Addr().Is4() != p.Addr().Is4() {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil || link.Attrs().OperState == netlink.OperDown || s.Skip != nil && s.Skip(link.Attrs().Name) {
			continue
		}
		if dst.Bits() > best { // the most specific
			best = dst.Bits()
			iface, index, connected = link.Attrs().Name, r.LinkIndex, dst
		}
	}
	return iface, index, connected, nil
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones).Masked(), true
}

// DefaultGateway is the main table's default gateway through the link, if
// any.
func (s *Linux) DefaultGateway(index int, v6 bool) (netip.Addr, bool) {
	family := netlink.FAMILY_V4
	if v6 {
		family = netlink.FAMILY_V6
	}
	routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: unix.RT_TABLE_MAIN, LinkIndex: index}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF)
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

// GatewayMAC asks the address for its MAC now: an ARP request for IPv4
// (the kernel's neighbour table can still hold the last network's entry),
// and a reachable neighbour entry, refreshed by a probe, for IPv6.
func (s *Linux) GatewayMAC(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	if addr.Is4() {
		return arpAsk(ctx, iface, addr)
	}
	return ndAsk(ctx, iface, addr)
}

// Probe makes a TCP connection to ap from iface (bound to it, so not
// through the tailnet).
func (s *Linux) Probe(ctx context.Context, iface string, ap netip.AddrPort) error {
	d := net.Dialer{Timeout: 3 * time.Second, Control: bindTo(iface)}
	c, err := d.DialContext(ctx, "tcp", ap.String())
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

// Rule is one of tsdirect's rules.
type Rule struct {
	Prefix   netip.Prefix
	Priority int
}

// Rules lists tsdirect's rules (by their protocol mark).
func (s *Linux) Rules() ([]Rule, error) {
	var out []Rule
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(fam)
		if err != nil {
			return nil, err
		}
		for _, r := range rules {
			if r.Protocol != s.Proto || r.Dst == nil {
				continue
			}
			if p, ok := prefixOf(r.Dst); ok {
				out = append(out, Rule{Prefix: p, Priority: r.Priority})
			}
		}
	}
	return out, nil
}

func (s *Linux) rule(r Rule) *netlink.Rule {
	nr := netlink.NewRule()
	nr.Priority = r.Priority
	nr.Table = unix.RT_TABLE_MAIN
	nr.Dst = &net.IPNet{IP: r.Prefix.Addr().AsSlice(), Mask: net.CIDRMask(r.Prefix.Bits(), r.Prefix.Addr().BitLen())}
	nr.Protocol = s.Proto
	nr.Family = netlink.FAMILY_V4
	if r.Prefix.Addr().Is6() {
		nr.Family = netlink.FAMILY_V6
	}
	return nr
}

// AddRule adds "to prefix lookup main" at the priority.
func (s *Linux) AddRule(r Rule) error {
	err := netlink.RuleAdd(s.rule(r))
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

// DelRule removes one of tsdirect's rules.
func (s *Linux) DelRule(r Rule) error {
	err := netlink.RuleDel(s.rule(r))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// Watch sends on the channel when addresses, routes, links or neighbours
// change, until ctx ends.
func (s *Linux) Watch(ctx context.Context) (<-chan struct{}, error) {
	out := make(chan struct{}, 1)
	poke := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	done := ctx.Done()
	routes := make(chan netlink.RouteUpdate, 16)
	addrs := make(chan netlink.AddrUpdate, 16)
	links := make(chan netlink.LinkUpdate, 16)
	if err := netlink.RouteSubscribe(routes, done); err != nil {
		return nil, err
	}
	if err := netlink.AddrSubscribe(addrs, done); err != nil {
		return nil, err
	}
	if err := netlink.LinkSubscribe(links, done); err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case _, ok := <-routes:
				if !ok {
					return
				}
				poke()
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

// errNoAnswer means the address did not answer.
var errNoAnswer = errors.New("no answer")

func ifaceAddr(iface string, v4 bool) (*net.Interface, netip.Addr, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, netip.Addr{}, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() == v4 {
				return ifi, ip.Unmap(), nil
			}
		}
	}
	return nil, netip.Addr{}, fmt.Errorf("%s has no address to ask from", iface)
}
