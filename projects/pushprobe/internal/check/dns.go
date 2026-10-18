package check

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type dnsCheck struct{ c config.DNS }

// ServerAddress is a DNS server as host:port (port 53 unless given; IPv6
// with or without brackets).
func ServerAddress(s string) string {
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(strings.Trim(s, "[]"), "53")
}

var types = map[string]dnsmessage.Type{"A": dnsmessage.TypeA, "AAAA": dnsmessage.TypeAAAA, "CNAME": dnsmessage.TypeCNAME,
	"MX": dnsmessage.TypeMX, "TXT": dnsmessage.TypeTXT, "NS": dnsmessage.TypeNS}

func (d dnsCheck) Run(ctx context.Context) Result {
	start := time.Now()
	typ := strings.ToUpper(d.c.Type)
	var answers []string
	var err error
	if d.c.Server != "" {
		// Straight to that server: no hosts file, no search domains, so the
		// answer is the server's.
		answers, err = query(ctx, ServerAddress(d.c.Server), d.c.Query, types[typ])
	} else {
		answers, err = system(ctx, d.c.Query, typ)
	}
	name := d.c.Query
	if err != nil {
		return fail(start, "%s: %v", name, err)
	}
	if len(answers) == 0 {
		return fail(start, "%s: no %s record", name, typ)
	}
	sort.Strings(answers)
	got := strings.Join(answers, ", ")
	if len(d.c.Expect) > 0 {
		want := map[string]bool{}
		for _, e := range d.c.Expect {
			want[normal(e)] = true
		}
		same := len(want) == len(answers)
		for _, a := range answers {
			same = same && want[normal(a)]
		}
		if !same {
			return fail(start, "%s is %s, expected %s", name, got, strings.Join(d.c.Expect, ", "))
		}
	}
	return ok(start, "%s is %s", name, got)
}

func normal(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".") }

// errNoSuchName is a name the server says does not exist.
var errNoSuchName = errors.New("no such name")

// system asks the system's resolver (hosts file included, as programs on
// this machine would see it).
func system(ctx context.Context, name, typ string) ([]string, error) {
	r := net.DefaultResolver
	var out []string
	var err error
	switch typ {
	case "A", "AAAA":
		network := "ip4"
		if typ == "AAAA" {
			network = "ip6"
		}
		var ips []net.IP
		ips, err = r.LookupIP(ctx, network, name)
		for _, ip := range ips {
			out = append(out, ip.String())
		}
	case "CNAME":
		var cname string
		cname, err = r.LookupCNAME(ctx, name)
		// Go answers the name itself when there is no CNAME.
		if err == nil && normal(cname) != normal(name) {
			out = append(out, normal(cname))
		}
	case "MX":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, name)
		for _, m := range mx {
			out = append(out, fmt.Sprintf("%d %s", m.Pref, normal(m.Host)))
		}
	case "TXT":
		out, err = r.LookupTXT(ctx, name)
	case "NS":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, name)
		for _, n := range ns {
			out = append(out, normal(n.Host))
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return nil, errNoSuchName
		case dnsErr.IsTimeout:
			return nil, errors.New("timed out")
		}
		return nil, errors.New(dnsErr.Err)
	}
	return out, err
}

// query sends one question to server, over UDP, and over TCP when the
// answer did not fit.
func query(ctx context.Context, server, name string, typ dnsmessage.Type) ([]string, error) {
	fqdn, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return nil, fmt.Errorf("%q is not a DNS name", name)
	}
	id := uint16(rand.Uint32())
	msg := dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: fqdn, Type: typ, Class: dnsmessage.ClassINET}}}
	packet, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	reply, err := exchange(ctx, "udp", server, packet, id)
	if err == nil && reply.Truncated {
		reply, err = exchange(ctx, "tcp", server, packet, id)
	}
	if err != nil {
		return nil, err
	}
	switch reply.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, errNoSuchName
	default:
		return nil, fmt.Errorf("the server answered %s", strings.TrimPrefix(reply.RCode.String(), "RCode"))
	}
	var out []string
	for _, a := range reply.Answers {
		if a.Header.Type != typ {
			continue // a CNAME on the way to the A record, say
		}
		switch b := a.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(b.A).String())
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(b.AAAA).String())
		case *dnsmessage.CNAMEResource:
			out = append(out, normal(b.CNAME.String()))
		case *dnsmessage.MXResource:
			out = append(out, fmt.Sprintf("%d %s", b.Pref, normal(b.MX.String())))
		case *dnsmessage.TXTResource:
			out = append(out, strings.Join(b.TXT, ""))
		case *dnsmessage.NSResource:
			out = append(out, normal(b.NS.String()))
		}
	}
	return out, nil
}

func exchange(ctx context.Context, network, server string, packet []byte, id uint16) (*dnsmessage.Message, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, server)
	if err != nil {
		return nil, errors.New(reason(err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if network == "tcp" {
		// Over TCP each message comes after its length.
		packet = append(binary.BigEndian.AppendUint16(nil, uint16(len(packet))), packet...)
	}
	if _, err := conn.Write(packet); err != nil {
		return nil, errors.New(reason(err))
	}
	buf := make([]byte, 65535)
	for {
		var n int
		if network == "tcp" {
			var size [2]byte
			if _, err := io.ReadFull(conn, size[:]); err != nil {
				return nil, errors.New(reason(err))
			}
			n = int(binary.BigEndian.Uint16(size[:]))
			if _, err := io.ReadFull(conn, buf[:n]); err != nil {
				return nil, errors.New(reason(err))
			}
		} else if n, err = conn.Read(buf); err != nil {
			return nil, errors.New(reason(err))
		}
		var m dnsmessage.Message
		if err := m.Unpack(buf[:n]); err != nil || m.Header.ID != id || !m.Header.Response {
			if network == "tcp" {
				return nil, errors.New("an unreadable answer")
			}
			continue // not ours (UDP); wait for the real one
		}
		return &m, nil
	}
}
