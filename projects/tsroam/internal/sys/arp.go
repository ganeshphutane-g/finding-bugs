package sys

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// htons is v in network byte order, as the socket calls want it.
func htons(v uint16) uint16 {
	return binary.NativeEndian.Uint16(binary.BigEndian.AppendUint16(nil, v))
}

// arpRequest is the ARP payload asking who has dst (the kernel adds the
// Ethernet header for SOCK_DGRAM).
func arpRequest(srcMAC net.HardwareAddr, src, dst netip.Addr) []byte {
	req := make([]byte, 28)
	binary.BigEndian.PutUint16(req[0:], 1)      // Ethernet
	binary.BigEndian.PutUint16(req[2:], 0x0800) // IPv4
	req[4], req[5] = 6, 4
	binary.BigEndian.PutUint16(req[6:], 1) // request
	copy(req[8:], srcMAC)
	s, d := src.As4(), dst.As4()
	copy(req[14:], s[:])
	copy(req[24:], d[:])
	return req
}

// arpReplyFrom is the sender's MAC if b is an ARP reply from addr.
func arpReplyFrom(b []byte, addr netip.Addr) (net.HardwareAddr, bool) {
	if len(b) < 28 || !bytes.Equal(b[0:6], []byte{0, 1, 8, 0, 6, 4}) || binary.BigEndian.Uint16(b[6:]) != 2 {
		return nil, false
	}
	if netip.AddrFrom4([4]byte(b[14:18])) != addr {
		return nil, false
	}
	return net.HardwareAddr(bytes.Clone(b[8:14])), true
}

// arpAsk sends an ARP request for addr out of iface and returns the MAC in
// the first reply, or ErrNoAnswer after a second.
func arpAsk(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	ifi, src, err := ifaceAddr(iface, addr)
	if err != nil {
		return nil, err
	}
	if len(ifi.HardwareAddr) != 6 {
		return nil, fmt.Errorf("%s has no Ethernet address (ARP needs one)", iface)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return nil, fmt.Errorf("ARP socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifi.Index}); err != nil {
		return nil, fmt.Errorf("ARP socket: %w", err)
	}
	req := arpRequest(ifi.HardwareAddr, src, addr)
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
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			left := time.Until(wait)
			if left <= 0 {
				break // ask again
			}
			// At most 50 ms at a time, so a cancelled ctx is seen soon.
			ms := int(min(left, 50*time.Millisecond)/time.Millisecond) + 1
			if _, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, ms); err != nil && err != unix.EINTR {
				return nil, fmt.Errorf("ARP on %s: %w", iface, err)
			}
			for {
				n, from, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
				if err != nil {
					break // nothing more for now
				}
				// Before bind, the socket hears every interface.
				if ll, ok := from.(*unix.SockaddrLinklayer); !ok || ll.Ifindex != ifi.Index {
					continue
				}
				if mac, ok := arpReplyFrom(buf[:n], addr); ok {
					return mac, nil
				}
			}
		}
	}
	return nil, ErrNoAnswer
}
