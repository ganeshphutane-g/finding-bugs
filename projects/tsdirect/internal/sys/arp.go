package sys

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// arpAsk sends an ARP request for addr out of iface and returns the MAC in
// the first reply, or errNoAnswer after a second.
func arpAsk(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	ifi, src, err := ifaceAddr(iface, true)
	if err != nil {
		return nil, err
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("%s has no Ethernet address (ARP needs one)", iface)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return nil, fmt.Errorf("ARP socket: %w", err)
	}
	defer unix.Close(fd)
	ll := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifi.Index}
	if err := unix.Bind(fd, ll); err != nil {
		return nil, err
	}
	// The ARP payload (the kernel adds the Ethernet header for SOCK_DGRAM).
	req := make([]byte, 28)
	binary.BigEndian.PutUint16(req[0:], 1)      // Ethernet
	binary.BigEndian.PutUint16(req[2:], 0x0800) // IPv4
	req[4], req[5] = 6, 4
	binary.BigEndian.PutUint16(req[6:], 1) // request
	copy(req[8:], ifi.HardwareAddr)
	s4, t4 := src.As4(), addr.As4()
	copy(req[14:], s4[:])
	copy(req[24:], t4[:])
	to := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifi.Index, Halen: 6, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buf := make([]byte, 128)
	for try := 0; try < 3 && time.Now().Before(deadline); try++ {
		if err := unix.Sendto(fd, req, 0, to); err != nil {
			return nil, fmt.Errorf("ARP request on %s: %w", iface, err)
		}
		wait := time.Now().Add(350 * time.Millisecond)
		if wait.After(deadline) {
			wait = deadline
		}
		for time.Now().Before(wait) {
			tv := unix.NsecToTimeval(time.Until(wait).Nanoseconds())
			unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				break // timed out: ask again
			}
			if n < 28 || binary.BigEndian.Uint16(buf[6:]) != 2 { // a reply
				continue
			}
			if netip.AddrFrom4([4]byte(buf[14:18])) != addr {
				continue
			}
			return net.HardwareAddr(append([]byte(nil), buf[8:14]...)), nil
		}
	}
	return nil, errNoAnswer
}

// ndAsk refreshes the neighbour entry for an IPv6 address (a probe makes
// the kernel ask) and returns its MAC once it is reachable.
func ndAsk(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	// Mark the entry for probing, then send a packet so the kernel asks.
	netlink.NeighSet(&netlink.Neigh{LinkIndex: ifi.Index, Family: netlink.FAMILY_V6, State: netlink.NUD_PROBE, IP: addr.AsSlice()})
	d := net.Dialer{Timeout: time.Second, Control: bindTo(iface)}
	if c, err := d.DialContext(ctx, "udp", netip.AddrPortFrom(addr, 9).String()); err == nil {
		c.Write([]byte{0})
		c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ns, err := netlink.NeighList(ifi.Index, netlink.FAMILY_V6)
		if err != nil {
			return nil, err
		}
		for _, n := range ns {
			if a, ok := netip.AddrFromSlice(n.IP); ok && a == addr && n.State&(netlink.NUD_REACHABLE|netlink.NUD_PERMANENT) != 0 && len(n.HardwareAddr) > 0 {
				return n.HardwareAddr, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, errNoAnswer
}
