// Package resolve looks up endpoint names with their TTLs, so an answer is
// reused only as long as DNS says it holds.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

// TTL bounds: an answer is reused for at least MinTTL (no DNS storm from
// TTL 0 records) and at most MaxTTL.
const (
	MinTTL = 30 * time.Second
	MaxTTL = time.Hour
)

// Failures are not asked again for a while, longer each time, so a dead
// DNS server does not make every round wait for its timeouts.
const (
	failFirst = 10 * time.Second
	failMax   = 5 * time.Minute
)

// Answer is what a name resolved to.
type Answer struct {
	Addrs []netip.Addr
	// Dropped are addresses DNS gave that cannot be an endpoint (0.0.0.0,
	// loopback, multicast: a DNS sinkhole or a captive portal).
	Dropped []netip.Addr
	// Expires is when the answer must be looked up again.
	Expires time.Time
	// Server is the resolver that answered.
	Server string
	Err    error
}

// ErrNoSuchName means the name does not exist (NXDOMAIN) or has no address.
var ErrNoSuchName = errors.New("no such name")

// ErrUnusable means every address the name has cannot be an endpoint.
var ErrUnusable = errors.New("resolves only to addresses that cannot be an endpoint")

// Resolver asks DNS servers in turn and caches answers until their TTL
// runs out.
type Resolver struct {
	// Servers are host:port; Search are search domains for single-label
	// names.
	Servers []string
	Search  []string
	// ConfPath, when set, is a resolv.conf to read servers and search
	// domains from, again whenever it changes (a laptop on another network).
	ConfPath string
	Timeout  time.Duration
	// Prefer orders addresses: "ipv4" (the default) or "ipv6". Family keeps
	// only "ipv4" or "ipv6" addresses ("" keeps both).
	Prefer string
	Family string
	// MinTTL overrides the shortest time an answer is kept (default MinTTL).
	MinTTL time.Duration
	Now    func() time.Time

	mu       sync.Mutex
	cache    map[string]Answer
	failures map[string]int
	confStat os.FileInfo
}

// FromResolvConf makes a resolver that reads a resolv.conf file, now and
// whenever it changes.
func FromResolvConf(path string) (*Resolver, error) {
	r := &Resolver{ConfPath: path}
	if err := r.readConf(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Resolver) readConf() error {
	st, err := os.Stat(r.ConfPath)
	if err != nil {
		return fmt.Errorf("%s: %v", r.ConfPath, err)
	}
	c, err := dns.ClientConfigFromFile(r.ConfPath)
	if err != nil {
		return fmt.Errorf("%s: %v", r.ConfPath, err)
	}
	var servers []string
	for _, s := range c.Servers {
		servers = append(servers, net.JoinHostPort(s, c.Port))
	}
	if len(servers) == 0 {
		return fmt.Errorf("%s names no nameserver", r.ConfPath)
	}
	r.mu.Lock()
	r.Servers, r.Search, r.confStat = servers, c.Search, st
	r.mu.Unlock()
	return nil
}

// Refresh reads resolv.conf again if it changed since it was read. A file
// that cannot be read now keeps the servers known so far.
func (r *Resolver) Refresh() (changed bool, err error) {
	if r.ConfPath == "" {
		return false, nil
	}
	st, err := os.Stat(r.ConfPath)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	old := r.confStat
	r.mu.Unlock()
	if old != nil && os.SameFile(old, st) && old.ModTime().Equal(st.ModTime()) && old.Size() == st.Size() {
		return false, nil
	}
	if err := r.readConf(); err != nil {
		return false, err
	}
	r.mu.Lock()
	r.cache, r.failures = nil, nil // ask the new servers
	r.mu.Unlock()
	return true, nil
}

// ServerList is the servers and search domains in use now.
func (r *Resolver) ServerList() ([]string, []string) { return r.servers() }

func (r *Resolver) servers() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.Servers...), append([]string(nil), r.Search...)
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Lookup resolves host, from the cache while its answer holds. mark, when
// not zero, is set on the queries' sockets (SO_MARK): with the WireGuard
// device's firewall mark, they bypass a full tunnel's policy routing just as
// WireGuard's own packets do, so a dead tunnel cannot swallow them.
func (r *Resolver) Lookup(ctx context.Context, host string, mark int) Answer {
	r.mu.Lock()
	if a, ok := r.cache[host]; ok && r.now().Before(a.Expires) {
		r.mu.Unlock()
		return a
	}
	r.mu.Unlock()
	a := r.lookup(ctx, host, mark)
	r.mu.Lock()
	if r.cache == nil {
		r.cache, r.failures = map[string]Answer{}, map[string]int{}
	}
	if a.Err != nil && !errors.Is(a.Err, ErrNoSuchName) && !errors.Is(a.Err, ErrUnusable) {
		// Back off: 10 s, 20 s, 40 s … up to 5 minutes.
		n := r.failures[host]
		r.failures[host] = n + 1
		a.Expires = r.now().Add(min(failFirst<<min(n, 10), failMax))
	} else {
		delete(r.failures, host)
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

func (r *Resolver) lookup(ctx context.Context, host string, mark int) Answer {
	servers, search := r.servers()
	names := []string{dns.Fqdn(host)}
	if !strings.Contains(host, ".") {
		names = nil
		for _, s := range search {
			names = append(names, dns.Fqdn(host+"."+strings.TrimSuffix(s, ".")))
		}
		names = append(names, dns.Fqdn(host))
	}
	var last Answer
	for _, n := range names {
		a := r.lookupName(ctx, servers, n, mark)
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

// usable: an address a peer can be reached at.
func usable(a netip.Addr) bool {
	return a.IsValid() && !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast() &&
		!a.IsLinkLocalUnicast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

type typeResult struct {
	qt    uint16
	addrs []netip.Addr
	ttl   uint32
	err   error
}

func (r *Resolver) lookupName(ctx context.Context, servers []string, name string, mark int) Answer {
	now := r.now()
	short := strings.TrimSuffix(name, ".")
	fail := func(err error) Answer {
		exp := now
		if errors.Is(err, ErrNoSuchName) || errors.Is(err, ErrUnusable) {
			exp = now.Add(r.minTTL())
		}
		return Answer{Err: err, Expires: exp}
	}
	if len(servers) == 0 {
		return fail(errors.New("no DNS servers to ask"))
	}
	var errs []string
	for _, srv := range servers {
		// A and AAAA at once.
		types := r.types()
		results := make(chan typeResult, len(types))
		for _, qt := range types {
			go func(qt uint16) {
				addrs, ttl, err := r.query(ctx, srv, name, qt, mark)
				results <- typeResult{qt, addrs, ttl, err}
			}(qt)
		}
		var v4, v6, dropped []netip.Addr
		ttl := uint32(MaxTTL / time.Second)
		failed := 0
		for range types {
			res := <-results
			if errors.Is(res.err, ErrNoSuchName) {
				continue
			}
			if res.err != nil {
				// Some resolvers fail AAAA queries (or A): what the other
				// type gave is still good.
				errs = append(errs, res.err.Error())
				failed++
				continue
			}
			ttl = min(ttl, res.ttl)
			for _, a := range res.addrs {
				switch {
				case !usable(a):
					dropped = append(dropped, a)
				case res.qt == dns.TypeA:
					v4 = append(v4, a)
				default:
					v6 = append(v6, a)
				}
			}
		}
		if len(v4)+len(v6) == 0 {
			if failed > 0 {
				continue // ask the next server
			}
			if len(dropped) > 0 {
				a := fail(fmt.Errorf("%s %w: %s (a DNS sinkhole or a captive portal?)", short, ErrUnusable, list(dropped)))
				a.Dropped = dropped
				return a
			}
			return fail(fmt.Errorf("%s: %w (asked %s)", short, ErrNoSuchName, srv))
		}
		addrs := append(v4, v6...)
		if r.Prefer == "ipv6" {
			addrs = append(v6, v4...)
		}
		d := min(max(time.Duration(ttl)*time.Second, r.minTTL()), MaxTTL)
		return Answer{Addrs: addrs, Dropped: dropped, Expires: now.Add(d), Server: srv}
	}
	return fail(fmt.Errorf("%s: no DNS server answered: %s", short, strings.Join(errs, "; ")))
}

func list(as []netip.Addr) string {
	var s []string
	for _, a := range as {
		s = append(s, a.String())
	}
	return strings.Join(s, ", ")
}

func (r *Resolver) minTTL() time.Duration {
	if r.MinTTL > 0 {
		return r.MinTTL
	}
	return MinTTL
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

// Class is a stable name for an error, for logging a failure once rather
// than on every round (the text carries random ports).
func Class(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoSuchName):
		return "no such name"
	case errors.Is(err, ErrUnusable):
		return "unusable"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case strings.Contains(err.Error(), "i/o timeout"):
		return "timeout"
	case strings.Contains(err.Error(), "connection refused"):
		return "refused"
	}
	for code, name := range dns.RcodeToString {
		if code != dns.RcodeSuccess && strings.Contains(err.Error(), ": "+name) {
			return name
		}
	}
	return "other"
}

// query asks one server for one type, over UDP and then TCP if the answer
// was cut short. It returns the addresses for the name (following CNAMEs in
// the answer) and the smallest TTL on the way.
func (r *Resolver) query(ctx context.Context, srv, name string, qt uint16, mark int) ([]netip.Addr, uint32, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	m := new(dns.Msg)
	m.SetQuestion(name, qt)
	m.RecursionDesired = true
	c := &dns.Client{Timeout: timeout, Dialer: &net.Dialer{Timeout: timeout, Control: markControl(mark)}}
	resp, _, err := c.ExchangeContext(ctx, m, srv)
	if err == nil && resp.Truncated {
		c.Net = "tcp"
		resp, _, err = c.ExchangeContext(ctx, m, srv)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", srv, err)
	}
	if len(resp.Question) != 1 || !strings.EqualFold(resp.Question[0].Name, name) || resp.Question[0].Qtype != qt {
		return nil, 0, fmt.Errorf("%s: the answer is for another question", srv)
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
					out = append(out, a.Unmap())
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
