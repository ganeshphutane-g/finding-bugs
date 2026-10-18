package check

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type dnsCheck struct{ c config.DNS }

func (d dnsCheck) resolver() *net.Resolver {
	if d.c.Server == "" {
		return net.DefaultResolver
	}
	server := d.c.Server
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "53")
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, server)
	}}
}

func (d dnsCheck) Run(ctx context.Context) Result {
	start := time.Now()
	r := d.resolver()
	name := d.c.Query
	var answers []string
	var err error
	switch strings.ToUpper(d.c.Type) {
	case "A", "AAAA":
		network := "ip4"
		if strings.EqualFold(d.c.Type, "AAAA") {
			network = "ip6"
		}
		var ips []net.IP
		ips, err = r.LookupIP(ctx, network, name)
		for _, ip := range ips {
			answers = append(answers, ip.String())
		}
	case "CNAME":
		var cname string
		cname, err = r.LookupCNAME(ctx, name)
		answers = append(answers, strings.TrimSuffix(cname, "."))
	case "MX":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, name)
		for _, m := range mx {
			answers = append(answers, fmt.Sprintf("%d %s", m.Pref, strings.TrimSuffix(m.Host, ".")))
		}
	case "TXT":
		answers, err = r.LookupTXT(ctx, name)
	case "NS":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, name)
		for _, n := range ns {
			answers = append(answers, strings.TrimSuffix(n.Host, "."))
		}
	default:
		return fail(start, "unsupported record type %s", d.c.Type)
	}
	if err != nil {
		if dnsErr, isDNS := err.(*net.DNSError); isDNS {
			switch {
			case dnsErr.IsNotFound:
				return fail(start, "%s: no such name or no %s record", name, strings.ToUpper(d.c.Type))
			case dnsErr.IsTimeout:
				return fail(start, "%s: timed out", name)
			}
			return fail(start, "%s: %s", name, dnsErr.Err)
		}
		return fail(start, "%s: %s", name, reason(err))
	}
	sort.Strings(answers)
	got := strings.Join(answers, ", ")
	if len(d.c.Expect) > 0 {
		want := map[string]bool{}
		for _, e := range d.c.Expect {
			want[strings.TrimSuffix(strings.ToLower(e), ".")] = true
		}
		for _, a := range answers {
			if !want[strings.ToLower(a)] {
				return fail(start, "%s is %s, expected %s", name, got, strings.Join(d.c.Expect, ", "))
			}
		}
		if len(answers) == 0 || len(want) != len(answers) {
			return fail(start, "%s is %s, expected %s", name, got, strings.Join(d.c.Expect, ", "))
		}
	}
	return ok(start, "%s is %s", name, got)
}
