// Package killswitch is tsroam's nftables table: while it is in place,
// nothing leaves the machine except through Tailscale's interface, from
// tailscaled itself (its packets carry Tailscale's mark), to loopback, and
// what a network needs to work (DHCP, IPv6 neighbour and router discovery).
// It touches only its own table, "inet tsroam".
package killswitch

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// Table is the name of tsroam's table (family inet).
const Table = "tsroam"

// Rules says what may leave.
type Rules struct {
	// Tun is Tailscale's interface (tailscale0).
	Tun string
	// Mark and Mask match tailscaled's own packets (0x80000/0xff0000).
	Mark, Mask uint32
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
// there is no moment without it.
func (s *Switch) On(r Rules) error {
	if r.Tun == "" || len(r.Tun) >= unix.IFNAMSIZ {
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
	accept := nftables.ChainPolicyAccept
	ch := c.AddChain(&nftables.Chain{
		Name: "out", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter, Policy: &accept,
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

func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}

func accept() expr.Any { return &expr.Verdict{Kind: expr.VerdictAccept} }

// ruleset is the chain's rules, in order:
//
//	oifname "lo" accept
//	oifname TUN accept
//	meta mark & MASK == MARK accept
//	udp dport { 67, 547 } accept            (DHCP, DHCPv6)
//	icmpv6 type 133-136 accept              (router and neighbour discovery)
//	ip daddr LAN accept                     (allow-lan only)
//	counter drop
func ruleset(r Rules) [][]expr.Any {
	out := [][]expr.Any{
		{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname("lo")}, accept()},
		{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(r.Tun)}, accept()},
		{
			&expr.Meta{Key: expr.MetaKeyMARK, Register: 1},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(r.Mask), Xor: binaryutil.NativeEndian.PutUint32(0)},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(r.Mark & r.Mask)},
			accept(),
		},
	}
	for _, port := range []uint16{67, 547} {
		out = append(out, []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(port)},
			accept(),
		})
	}
	out = append(out, []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_ICMPV6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 1},
		&expr.Range{Op: expr.CmpOpEq, Register: 1, FromData: []byte{133}, ToData: []byte{136}},
		accept(),
	})
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
