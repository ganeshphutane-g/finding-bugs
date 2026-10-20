package server

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// udpSock is one UDP socket. On a wildcard address it learns each query's
// destination address and answers from it: otherwise the kernel picks the
// source, and on a host with several addresses clients drop the answer.
type udpSock struct {
	pc net.PacketConn
	p4 *ipv4.PacketConn
	p6 *ipv6.PacketConn
}

// dest is where a datagram was sent to (unset when not known).
type dest struct {
	ip      net.IP
	ifIndex int
}

// listenUDP binds address. A wildcard address (":53", "0.0.0.0:53",
// "[::]:53") gets an IPv4 socket and, for ":53" and "[::]:53", an IPv6-only
// one, each told to report destinations; an IPv6 socket that cannot be had
// (no IPv6 on the host) is left out.
func listenUDP(address string) ([]*udpSock, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ip netip.Addr
	if host != "" {
		if ip, err = netip.ParseAddr(host); err != nil {
			// A host name: bind it as given.
			pc, err := net.ListenPacket("udp", address)
			if err != nil {
				return nil, err
			}
			return []*udpSock{{pc: pc}}, nil
		}
	}
	if host != "" && !ip.IsUnspecified() {
		pc, err := net.ListenPacket("udp", address)
		if err != nil {
			return nil, err
		}
		return []*udpSock{{pc: pc}}, nil
	}
	var socks []*udpSock
	pc4, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", port))
	if err != nil {
		return nil, err
	}
	u := &udpSock{pc: pc4, p4: ipv4.NewPacketConn(pc4)}
	if u.p4.SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true) != nil {
		u.p4 = nil // not on this system: answer as the kernel chooses
	}
	socks = append(socks, u)
	if host == "" || ip.Is6() {
		// The same port as the IPv4 socket got (port 0: any free one).
		port := fmt.Sprint(pc4.LocalAddr().(*net.UDPAddr).Port)
		pc6, err := net.ListenPacket("udp6", net.JoinHostPort("::", port))
		switch {
		case err == nil:
			u := &udpSock{pc: pc6, p6: ipv6.NewPacketConn(pc6)}
			if u.p6.SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true) != nil {
				u.p6 = nil
			}
			socks = append(socks, u)
		case errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EPROTONOSUPPORT):
			// No IPv6 here.
		default:
			pc4.Close()
			return nil, err
		}
	}
	return socks, nil
}

func (u *udpSock) read(b []byte) (int, dest, net.Addr, error) {
	switch {
	case u.p4 != nil:
		n, cm, from, err := u.p4.ReadFrom(b)
		if cm != nil {
			return n, dest{cm.Dst, cm.IfIndex}, from, err
		}
		return n, dest{}, from, err
	case u.p6 != nil:
		n, cm, from, err := u.p6.ReadFrom(b)
		if cm != nil {
			return n, dest{cm.Dst, cm.IfIndex}, from, err
		}
		return n, dest{}, from, err
	}
	n, from, err := u.pc.ReadFrom(b)
	return n, dest{}, from, err
}

func (u *udpSock) write(b []byte, d dest, to net.Addr) (int, error) {
	switch {
	case u.p4 != nil && d.ip != nil:
		return u.p4.WriteTo(b, &ipv4.ControlMessage{Src: d.ip}, to)
	case u.p6 != nil && d.ip != nil:
		return u.p6.WriteTo(b, &ipv6.ControlMessage{Src: d.ip, IfIndex: d.ifIndex}, to)
	}
	return u.pc.WriteTo(b, to)
}
