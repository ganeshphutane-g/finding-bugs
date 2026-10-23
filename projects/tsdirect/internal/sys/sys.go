// Package sys is what tsdirect needs from Linux: connected routes, the
// gateway's MAC (asked fresh, with ARP), probes from a given interface, its
// own ip rules, and change events. IPv4 only.
package sys

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
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

// CheckKernel refuses kernels older than 4.17: they ignore a rule's
// protocol value, so tsdirect could not find its own rules to remove them.
func CheckKernel() error {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return err
	}
	return kernelOK(unix.ByteSliceToString(u.Release[:]))
}

func kernelOK(release string) error {
	f := strings.FieldsFunc(release, func(r rune) bool { return r < '0' || r > '9' })
	if len(f) < 2 {
		return fmt.Errorf("cannot read the kernel version %q", release)
	}
	major, _ := strconv.Atoi(f[0])
	minor, _ := strconv.Atoi(f[1])
	if major < 4 || major == 4 && minor < 17 {
		return fmt.Errorf("Linux %s is too old: tsdirect needs 4.17 or newer, which keeps a rule's protocol value (how tsdirect finds its own rules)", release)
	}
	return nil
}

// route is a connected route, as far as Connected cares.
type route struct {
	dst   netip.Prefix
	index int
	name  string
	// usable: the link is up and the route not marked linkdown.
	usable bool
}

// pick chooses, among connected routes, the one covering p (the most
// specific such); with none, the widest inside p, so the caller can say p
// is only partly there.
func pick(routes []route, p netip.Prefix) (route, bool) {
	var best route
	found, score := false, 0
	for _, r := range routes {
		if !r.usable || !r.dst.Overlaps(p) {
			continue
		}
		s := r.dst.Bits()
		if r.dst.Bits() > p.Bits() {
			s = -r.dst.Bits() - 1
		}
		if !found || s > score {
			best, found, score = r, true, s
		}
	}
	return best, found
}

// Connected finds the interface whose directly connected route in the main
// table covers p (see pick).
func (s *Linux) Connected(p netip.Prefix) (iface string, index int, connected netip.Prefix, err error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return "", 0, netip.Prefix{}, err
	}
	var cands []route
	for _, r := range routes {
		if r.Dst == nil || r.Scope != netlink.SCOPE_LINK || r.Gw != nil || r.Type != unix.RTN_UNICAST || len(r.MultiPath) > 0 {
			continue
		}
		dst, ok := prefixOf(r.Dst)
		if !ok || !dst.Addr().Is4() {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil || s.Skip != nil && s.Skip(link.Attrs().Name) {
			continue
		}
		// Up, or unknown (interfaces that do not report it); not down, not
		// without carrier, not dormant (Wi-Fi not yet authenticated).
		st := link.Attrs().OperState
		usable := (st == netlink.OperUp || st == netlink.OperUnknown) && r.Flags&unix.RTNH_F_LINKDOWN == 0
		cands = append(cands, route{dst: dst, index: r.LinkIndex, name: link.Attrs().Name, usable: usable})
	}
	if r, ok := pick(cands, p); ok {
		return r.name, r.index, r.dst, nil
	}
	return "", 0, netip.Prefix{}, nil
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

// Rule is one of tsdirect's rules.
type Rule struct {
	Prefix   netip.Prefix
	Priority int
}

// Rules lists tsdirect's rules: "to PREFIX lookup main" with its protocol
// value.
func (s *Linux) Rules() ([]Rule, error) {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for _, r := range rules {
		if r.Protocol != s.Proto || r.Dst == nil || r.Table != unix.RT_TABLE_MAIN {
			continue
		}
		if p, ok := prefixOf(r.Dst); ok {
			out = append(out, Rule{Prefix: p, Priority: r.Priority})
		}
	}
	return out, nil
}

func (s *Linux) rule(r Rule) *netlink.Rule {
	nr := netlink.NewRule()
	nr.Priority = r.Priority
	nr.Table = unix.RT_TABLE_MAIN
	nr.Dst = &net.IPNet{IP: r.Prefix.Addr().AsSlice(), Mask: net.CIDRMask(r.Prefix.Bits(), 32)}
	nr.Protocol = s.Proto
	nr.Family = netlink.FAMILY_V4
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
	const rcvbuf = 1 << 20 // route bursts (Tailscale's table 52) are large
	routes := make(chan netlink.RouteUpdate, 64)
	addrs := make(chan netlink.AddrUpdate, 16)
	links := make(chan netlink.LinkUpdate, 16)
	if err := netlink.RouteSubscribeWithOptions(routes, done, netlink.RouteSubscribeOptions{ReceiveBufferSize: rcvbuf}); err != nil {
		stop()
		return nil, err
	}
	if err := netlink.AddrSubscribeWithOptions(addrs, done, netlink.AddrSubscribeOptions{ReceiveBufferSize: rcvbuf}); err != nil {
		stop()
		return nil, err
	}
	if err := netlink.LinkSubscribeWithOptions(links, done, netlink.LinkSubscribeOptions{ReceiveBufferSize: rcvbuf}); err != nil {
		stop()
		return nil, err
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
