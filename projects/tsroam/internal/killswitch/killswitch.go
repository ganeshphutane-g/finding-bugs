// Package killswitch is tsroam's nftables table: while it is in place,
// nothing leaves the machine, whether its own traffic or traffic it
// forwards (containers, VMs), except through Tailscale's interface, from
// tailscaled itself (its packets carry Tailscale's mark), from tsroam's own
// trust probes (their mark), to loopback, and what a network needs to work
// (a DHCP client's requests, IPv6 neighbour and router discovery on the
// link). It touches only its own table, "inet tsroam".
package killswitch

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Table is the name of tsroam's table (family inet), and Chain its chain.
const (
	Table = "tsroam"
	Chain = "out"
)

// ProbeMask is the part of the mark tsroam's probes set (ProbeMark); it is
// outside Tailscale's 0xff0000.
const ProbeMask = 0xff000000

// Rules says what may leave.
type Rules struct {
	// Tun is Tailscale's interface (tailscale0).
	Tun string
	// Mark and Mask match tailscaled's own packets (0x80000/0xff0000).
	Mark, Mask uint32
	// ProbeMark, if not zero, lets tsroam's trust probes out (they carry
	// it, under ProbeMask), so a trusted network can be recognised with the
	// kill switch on.
	ProbeMark uint32
	// Iface, if set, is the only interface DHCP may use.
	Iface string
	// LAN, if valid, may also be reached directly (allow-lan).
	LAN netip.Prefix
}

// Switch installs and removes the table.
type Switch struct {
	// NetNS, if not zero, is a network namespace's fd (for tests).
	NetNS int
}

func (s *Switch) conn() (*nftables.Conn, error) {
	if s.NetNS != 0 {
		return nftables.New(nftables.WithNetNSFd(s.NetNS))
	}
	return nftables.New()
}

func table() *nftables.Table {
	return &nftables.Table{Name: Table, Family: nftables.TableFamilyINet}
}

// On installs the table, replacing any earlier one in the same transaction:
// there is no moment without it. The chain is on the postrouting hook, so
// forwarded traffic is covered as well as the machine's own, and its policy
// is drop: emptied, it still lets nothing out.
func (s *Switch) On(r Rules) error {
	if r.Tun == "" || len(r.Tun) >= unix.IFNAMSIZ || len(r.Iface) >= unix.IFNAMSIZ {
		return fmt.Errorf("interface name %q", r.Tun)
	}
	c, err := s.conn()
	if err != nil {
		return err
	}
	t := table()
	// Adding first makes the delete valid when there was no table.
	c.AddTable(t)
	c.DelTable(t)
	t = c.AddTable(table())
	drop := nftables.ChainPolicyDrop
	ch := c.AddChain(&nftables.Chain{
		Name: Chain, Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityFilter, Policy: &drop,
	})
	for _, e := range ruleset(r) {
		c.AddRule(&nftables.Rule{Table: t, Chain: ch, Exprs: e})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("installing the kill switch: %w", err)
	}
	return nil
}

// Off removes the table, if it is there.
func (s *Switch) Off() error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	c.AddTable(table())
	c.DelTable(table())
	if err := c.Flush(); err != nil {
		return fmt.Errorf("removing the kill switch: %w", err)
	}
	return nil
}

// Active says whether the table is in place.
func (s *Switch) Active() (bool, error) {
	c, err := s.conn()
	if err != nil {
		return false, err
	}
	ts, err := c.ListTablesOfFamily(nftables.TableFamilyINet)
	if err != nil {
		return false, err
	}
	for _, t := range ts {
		if t.Name == Table {
			return true, nil
		}
	}
	return false, nil
}

// Intact says whether the table is in place with all of r's rules (not
// flushed, not edited).
func (s *Switch) Intact(r Rules) (bool, error) {
	on, err := s.Active()
	if err != nil || !on {
		return false, err
	}
	c, err := s.conn()
	if err != nil {
		return false, err
	}
	ch, err := c.ListChain(table(), Chain)
	if err != nil || ch.Policy == nil || *ch.Policy != nftables.ChainPolicyDrop || ch.Hooknum == nil || *ch.Hooknum != *nftables.ChainHookPostrouting {
		return false, nil
	}
	rules, err := c.GetRule(table(), ch)
	if err != nil {
		return false, err
	}
	return len(rules) == len(ruleset(r)), nil
}

func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}

func accept() expr.Any { return &expr.Verdict{Kind: expr.VerdictAccept} }

func oif(name string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)}}
}

func markIs(mark, mask uint32) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(mask), Xor: binaryutil.NativeEndian.PutUint32(0)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(mark & mask)},
	}
}

// udpPorts matches UDP from sport to dport. The DHCP client ports are
// privileged: only root can send from them.
func udpPorts(sport, dport uint16) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: append(binaryutil.BigEndian.PutUint16(sport), binaryutil.BigEndian.PutUint16(dport)...)},
	}
}

// ip6DaddrIn matches an IPv6 destination inside p.
func ip6DaddrIn(p netip.Prefix) []expr.Any {
	a := p.Addr().As16()
	m := make([]byte, 16)
	for i := 0; i < p.Bits(); i++ {
		m[i/8] |= 0x80 >> (i % 8)
	}
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 24, Len: 16},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 16, Mask: m, Xor: make([]byte, 16)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: a[:]},
	}
}

func cat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ruleset is the chain's rules, in order:
//
//	oifname "lo" accept
//	oifname TUN accept
//	meta mark & MASK == MARK accept                      (tailscaled)
//	meta mark & 0xff000000 == PROBEMARK accept           (tsroam's probes)
//	[oifname IFACE] udp sport 68 dport 67 accept         (DHCP client)
//	[oifname IFACE] udp sport 546 dport 547 accept       (DHCPv6 client)
//	icmpv6 type 133-136 ip6 daddr fe80::/10 accept       (router and neighbour
//	icmpv6 type 133-136 ip6 daddr ff02::/16 accept        discovery, on the link)
//	ip daddr LAN accept                                  (allow-lan only)
//	counter drop
func ruleset(r Rules) [][]expr.Any {
	out := [][]expr.Any{
		cat(oif("lo"), []expr.Any{accept()}),
		cat(oif(r.Tun), []expr.Any{accept()}),
		cat(markIs(r.Mark, r.Mask), []expr.Any{accept()}),
	}
	if r.ProbeMark != 0 {
		out = append(out, cat(markIs(r.ProbeMark, ProbeMask), []expr.Any{accept()}))
	}
	var dhcpIf []expr.Any
	if r.Iface != "" {
		dhcpIf = oif(r.Iface)
	}
	out = append(out,
		cat(dhcpIf, udpPorts(68, 67), []expr.Any{accept()}),
		cat(dhcpIf, udpPorts(546, 547), []expr.Any{accept()}),
	)
	nd := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Range{Op: expr.CmpOpEq, Register: 1, FromData: []byte{133}, ToData: []byte{136}},
	}
	for _, p := range []string{"fe80::/10", "ff02::/16"} {
		out = append(out, cat(nd, ip6DaddrIn(netip.MustParsePrefix(p)), []expr.Any{accept()}))
	}
	if r.LAN.IsValid() && r.LAN.Addr().Is4() {
		lan := r.LAN.Masked()
		a := lan.Addr().As4()
		out = append(out, []expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: maskBytes(lan.Bits()), Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: a[:]},
			accept(),
		})
	}
	out = append(out, []expr.Any{&expr.Counter{}, &expr.Verdict{Kind: expr.VerdictDrop}})
	return out
}

func maskBytes(bits int) []byte {
	m := make([]byte, 4)
	for i := 0; i < bits; i++ {
		m[i/8] |= 0x80 >> (i % 8)
	}
	return m
}

// LANOK says whether p may be let through for allow-lan: a private or
// link-local IPv4 network, /8 or narrower. A hostile network's DHCP could
// otherwise hand out a netmask that makes half the internet "the LAN".
func LANOK(p netip.Prefix) bool {
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() < 8 {
		return false
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		rp := netip.MustParsePrefix(r)
		if rp.Bits() <= p.Bits() && rp.Contains(p.Addr()) {
			return true
		}
	}
	return false
}
