// Package resolve looks up endpoint names with their TTLs, so an answer is
// reused only as long as DNS says it holds.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// TTL bounds: an answer is reused for at least MinTTL (no DNS storm from
// TTL 0 records) and at most MaxTTL.
const (
	MinTTL = 30 * time.Second
	MaxTTL = time.Hour
)

// Answer is what a name resolved to.
type Answer struct {
	Addrs []netip.Addr
	// Expires is when the answer must be looked up again.
	Expires time.Time
	// Server is the resolver that answered.
	Server string
	Err    error
}

// ErrNoSuchName means the name does not exist (NXDOMAIN) or has no address.
var ErrNoSuchName = errors.New("no such name")

// Resolver asks DNS servers in turn and caches answers until their TTL
// runs out.
type Resolver struct {
	// Servers are host:port; Search are search domains for single-label
	// names.
	Servers []string
	Search  []string
	Timeout time.Duration
	// Prefer orders addresses: "ipv4" (the default) or "ipv6". Family keeps
	// only "ipv4" or "ipv6" addresses ("" keeps both).
	Prefer string
	Family string
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]Answer
}

// FromResolvConf makes a resolver from a resolv.conf file.
func FromResolvConf(path string) (*Resolver, error) {
	c, err := dns.ClientConfigFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	r := &Resolver{Search: c.Search}
	for _, s := range c.Servers {
		r.Servers = append(r.Servers, net.JoinHostPort(s, c.Port))
	}
	if len(r.Servers) == 0 {
		return nil, fmt.Errorf("%s names no nameserver", path)
	}
	return r, nil
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Lookup resolves host, from the cache while its answer holds.
func (r *Resolver) Lookup(ctx context.Context, host string) Answer {
	r.mu.Lock()
	if a, ok := r.cache[host]; ok && r.now().Before(a.Expires) {
		r.mu.Unlock()
		return a
	}
	r.mu.Unlock()
	a := r.lookup(ctx, host)
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]Answer{}
	}
	r.cache[host] = a
	r.mu.Unlock()
	return a
}

// Forget drops a cached answer (after a change did not help, say).
func (r *Resolver) Forget(host string) {
	r.mu.Lock()
	delete(r.cache, host)
	r.mu.Unlock()
}

func (r *Resolver) lookup(ctx context.Context, host string) Answer {
	names := []string{dns.Fqdn(host)}
	if !strings.Contains(host, ".") {
		names = nil
		for _, s := range r.Search {
			names = append(names, dns.Fqdn(host+"."+strings.TrimSuffix(s, ".")))
		}
		names = append(names, dns.Fqdn(host))
	}
	var last Answer
	for _, n := range names {
		a := r.lookupName(ctx, n)
		if a.Err == nil {
			return a
		}
		last = a
		if !errors.Is(a.Err, ErrNoSuchName) {
			break
		}
	}
	return last
}

func (r *Resolver) lookupName(ctx context.Context, name string) Answer {
	now := r.now()
	fail := func(err error) Answer {
		// A failure is retried at the next round, a missing name after MinTTL.
		exp := now
		if errors.Is(err, ErrNoSuchName) {
			exp = now.Add(MinTTL)
		}
		return Answer{Err: err, Expires: exp}
	}
	var errs []string
	for _, srv := range r.Servers {
		var v4, v6 []netip.Addr
		ttl := uint32(MaxTTL / time.Second)
		nx := 0
		ok := true
		for _, qt := range r.types() {
			addrs, t, err := r.query(ctx, srv, name, qt)
			if errors.Is(err, ErrNoSuchName) {
				nx++
				continue
			}
			if err != nil {
				errs = append(errs, err.Error())
				ok = false
				break
			}
			ttl = min(ttl, t)
			if qt == dns.TypeA {
				v4 = addrs
			} else {
				v6 = addrs
			}
		}
		if !ok {
			continue // ask the next server
		}
		if nx == len(r.types()) || len(v4)+len(v6) == 0 {
			return fail(fmt.Errorf("%s: %w (asked %s)", strings.TrimSuffix(name, "."), ErrNoSuchName, srv))
		}
		addrs := append(v4, v6...)
		if r.Prefer == "ipv6" {
			addrs = append(v6, v4...)
		}
		d := min(max(time.Duration(ttl)*time.Second, MinTTL), MaxTTL)
		return Answer{Addrs: addrs, Expires: now.Add(d), Server: srv}
	}
	if len(r.Servers) == 0 {
		return fail(errors.New("no DNS servers to ask"))
	}
	return fail(fmt.Errorf("%s: no DNS server answered: %s", strings.TrimSuffix(name, "."), strings.Join(errs, "; ")))
}

func (r *Resolver) types() []uint16 {
	switch r.Family {
	case "ipv4":
		return []uint16{dns.TypeA}
	case "ipv6":
		return []uint16{dns.TypeAAAA}
	}
	return []uint16{dns.TypeA, dns.TypeAAAA}
}

// query asks one server for one type, over UDP and then TCP if the answer
// was cut short. It returns the addresses for the name (following CNAMEs in
// the answer) and the smallest TTL on the way.
func (r *Resolver) query(ctx context.Context, srv, name string, qt uint16) ([]netip.Addr, uint32, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	m := new(dns.Msg)
	m.SetQuestion(name, qt)
	m.RecursionDesired = true
	c := &dns.Client{Timeout: timeout}
	resp, _, err := c.ExchangeContext(ctx, m, srv)
	if err == nil && resp.Truncated {
		c.Net = "tcp"
		resp, _, err = c.ExchangeContext(ctx, m, srv)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %v", srv, err)
	}
	switch resp.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		return nil, 0, ErrNoSuchName
	default:
		return nil, 0, fmt.Errorf("%s: %s", srv, dns.RcodeToString[resp.Rcode])
	}
	ttl := uint32(MaxTTL / time.Second)
	want := strings.ToLower(name)
	var out []netip.Addr
	// CNAMEs come first in a resolver's answer; follow them in order.
	for hops := 0; hops < 10; hops++ {
		next := ""
		for _, rr := range resp.Answer {
			h := rr.Header()
			if strings.ToLower(h.Name) != want {
				continue
			}
			switch v := rr.(type) {
			case *dns.CNAME:
				next = strings.ToLower(v.Target)
				ttl = min(ttl, h.Ttl)
			case *dns.A:
				if a, ok := netip.AddrFromSlice(v.A.To4()); ok && qt == dns.TypeA {
					out = append(out, a)
					ttl = min(ttl, h.Ttl)
				}
			case *dns.AAAA:
				if a, ok := netip.AddrFromSlice(v.AAAA); ok && qt == dns.TypeAAAA {
					out = append(out, a)
					ttl = min(ttl, h.Ttl)
				}
			}
		}
		if len(out) > 0 || next == "" {
			break
		}
		want = next
	}
	return out, ttl, nil
}
