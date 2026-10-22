package resolve

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// server runs a DNS server on 127.0.0.1 (UDP and TCP on one port) with the
// given handler.
func server(t *testing.T, h dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	us := &dns.Server{PacketConn: pc, Handler: h}
	ts := &dns.Server{Listener: l, Handler: h}
	go us.ActivateAndServe()
	go ts.ActivateAndServe()
	t.Cleanup(func() { us.Shutdown(); ts.Shutdown() })
	return pc.LocalAddr().String()
}

func rr(s string) dns.RR {
	r, err := dns.NewRR(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestLookup(t *testing.T) {
	var queries atomic.Int32
	srv := server(t, func(w dns.ResponseWriter, q *dns.Msg) {
		queries.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		name, qt := q.Question[0].Name, q.Question[0].Qtype
		switch {
		case name == "home.example.":
			m.Answer = append(m.Answer, rr("home.example. 300 IN CNAME dyn.example."))
			if qt == dns.TypeA {
				m.Answer = append(m.Answer, rr("dyn.example. 60 IN A 203.0.113.7"))
			} else {
				m.Answer = append(m.Answer, rr("dyn.example. 60 IN AAAA 2001:db8::7"))
			}
		case name == "v4only.example." && qt == dns.TypeA:
			m.Answer = append(m.Answer, rr("v4only.example. 5 IN A 203.0.113.8"))
		case name == "v4only.example.":
		case name == "big.example." && qt == dns.TypeA:
			if _, tcp := w.RemoteAddr().(*net.TCPAddr); !tcp {
				m.Truncated = true
			} else {
				m.Answer = append(m.Answer, rr("big.example. 600 IN A 203.0.113.9"))
			}
		case name == "big.example.":
		case name == "short.lan.":
			if qt == dns.TypeA {
				m.Answer = append(m.Answer, rr("short.lan. 100 IN A 10.0.0.5"))
			}
		default:
			m.Rcode = dns.RcodeNameError
		}
		w.WriteMsg(m)
	})
	now := time.Unix(1_000_000, 0)
	r := &Resolver{Servers: []string{srv}, Search: []string{"lan"}, Now: func() time.Time { return now }}
	ctx := context.Background()

	a := r.Lookup(ctx, "home.example", 0)
	if a.Err != nil || len(a.Addrs) != 2 || a.Addrs[0].String() != "203.0.113.7" || a.Addrs[1].String() != "2001:db8::7" {
		t.Fatalf("%+v", a)
	}
	if a.Expires != now.Add(60*time.Second) {
		t.Fatalf("ttl: %v", a.Expires.Sub(now))
	}
	// Cached until the TTL runs out.
	n := queries.Load()
	r.Lookup(ctx, "home.example", 0)
	if queries.Load() != n {
		t.Fatal("asked again within the TTL")
	}
	now = now.Add(61 * time.Second)
	r.Lookup(ctx, "home.example", 0)
	if queries.Load() == n {
		t.Fatal("not asked again after the TTL")
	}
	// TTL 5 is held for MinTTL.
	if a := r.Lookup(ctx, "v4only.example", 0); a.Err != nil || len(a.Addrs) != 1 || a.Expires != now.Add(MinTTL) {
		t.Fatalf("%+v", a)
	}
	// Truncated over UDP: asked again over TCP.
	if a := r.Lookup(ctx, "big.example", 0); a.Err != nil || a.Addrs[0].String() != "203.0.113.9" {
		t.Fatalf("%+v", a)
	}
	// A single label goes through the search domains.
	if a := r.Lookup(ctx, "short", 0); a.Err != nil || a.Addrs[0].String() != "10.0.0.5" {
		t.Fatalf("%+v", a)
	}
	// Missing.
	if a := r.Lookup(ctx, "gone.example", 0); !errors.Is(a.Err, ErrNoSuchName) {
		t.Fatalf("%+v", a)
	}
	// Prefer and family.
	r6 := &Resolver{Servers: []string{srv}, Prefer: "ipv6"}
	if a := r6.Lookup(ctx, "home.example", 0); a.Addrs[0].String() != "2001:db8::7" {
		t.Fatalf("%+v", a)
	}
	r4 := &Resolver{Servers: []string{srv}, Family: "ipv4"}
	if a := r4.Lookup(ctx, "home.example", 0); len(a.Addrs) != 1 || !a.Addrs[0].Is4() {
		t.Fatalf("%+v", a)
	}
}

func TestFallback(t *testing.T) {
	bad := server(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(q, dns.RcodeServerFailure)
		w.WriteMsg(m)
	})
	good := server(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if q.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, rr(q.Question[0].Name+" 60 IN A 198.51.100.1"))
		}
		w.WriteMsg(m)
	})
	// A dead server (nothing listens) and a failing one, then a good one.
	dead := "127.0.0.1:1"
	r := &Resolver{Servers: []string{dead, bad, good}, Timeout: 300 * time.Millisecond}
	a := r.Lookup(context.Background(), "x.example", 0)
	if a.Err != nil || a.Server != good {
		t.Fatalf("%+v", a)
	}
	r = &Resolver{Servers: []string{dead, bad}, Timeout: 300 * time.Millisecond}
	a = r.Lookup(context.Background(), "x.example", 0)
	if a.Err == nil || !strings.Contains(a.Err.Error(), "SERVFAIL") {
		t.Fatalf("%+v", a)
	}
	// A failure is kept for 10 s, then 20 s: a dead server does not make
	// every round wait for its timeouts.
	if d := time.Until(a.Expires); d < 9*time.Second || d > 11*time.Second {
		t.Fatalf("kept for %s", d)
	}
	r.Forget("x.example")
	a = r.Lookup(context.Background(), "x.example", 0)
	if d := time.Until(a.Expires); d < 19*time.Second || d > 21*time.Second {
		t.Fatalf("no back-off: %s", d)
	}
	if Class(a.Err) != "SERVFAIL" && Class(a.Err) != "refused" {
		t.Fatalf("%q from %v", Class(a.Err), a.Err)
	}
}

func TestOneTypeFails(t *testing.T) {
	// A resolver that answers A but refuses AAAA.
	srv := server(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		if q.Question[0].Qtype == dns.TypeAAAA {
			m.SetRcode(q, dns.RcodeRefused)
		} else {
			m.SetReply(q)
			m.Answer = append(m.Answer, rr(q.Question[0].Name+" 60 IN A 198.51.100.4"))
		}
		w.WriteMsg(m)
	})
	r := &Resolver{Servers: []string{srv}}
	if a := r.Lookup(context.Background(), "x.example", 0); a.Err != nil || len(a.Addrs) != 1 {
		t.Fatalf("%+v", a)
	}
}

func TestSinkholeAndClasses(t *testing.T) {
	srv := server(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		n := q.Question[0].Name
		if q.Question[0].Qtype == dns.TypeA {
			switch n {
			case "sink.example.":
				m.Answer = append(m.Answer, rr(n+" 60 IN A 0.0.0.0"))
			case "mixed.example.":
				m.Answer = append(m.Answer, rr(n+" 60 IN A 127.0.0.1"), rr(n+" 60 IN A 198.51.100.3"))
			}
		}
		w.WriteMsg(m)
	})
	r := &Resolver{Servers: []string{srv}}
	a := r.Lookup(context.Background(), "sink.example", 0)
	if !errors.Is(a.Err, ErrUnusable) || len(a.Dropped) != 1 || Class(a.Err) != "unusable" {
		t.Fatalf("%+v", a)
	}
	a = r.Lookup(context.Background(), "mixed.example", 0)
	if a.Err != nil || len(a.Addrs) != 1 || a.Addrs[0].String() != "198.51.100.3" || len(a.Dropped) != 1 {
		t.Fatalf("%+v", a)
	}
	// A dead server times out; the class is stable across rounds.
	silent, err := net.ListenPacket("udp", "127.0.0.1:0") // never answers
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	dead := &Resolver{Servers: []string{silent.LocalAddr().String()}, Timeout: 200 * time.Millisecond}
	if c := Class(dead.Lookup(context.Background(), "x.example", 0).Err); c != "timeout" {
		t.Fatalf("%q", c)
	}
}

func TestRefresh(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(conf, []byte("nameserver 192.0.2.1\nsearch a.example\n"), 0o644)
	r, err := FromResolvConf(conf)
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := r.Refresh(); changed {
		t.Fatal("changed without a change")
	}
	// The laptop moves to another network: resolv.conf is rewritten.
	os.WriteFile(conf+".new", []byte("nameserver 198.51.100.53\nnameserver 2001:db8::53\n"), 0o644)
	os.Rename(conf+".new", conf)
	if changed, err := r.Refresh(); !changed || err != nil {
		t.Fatal(changed, err)
	}
	if srv, search := r.servers(); strings.Join(srv, " ") != "198.51.100.53:53 [2001:db8::53]:53" || len(search) != 0 {
		t.Fatal(srv, search)
	}
	// Unreadable now: the servers known so far stay.
	os.Remove(conf)
	if _, err := r.Refresh(); err == nil {
		t.Fatal("no error")
	}
	if srv, _ := r.servers(); len(srv) != 2 {
		t.Fatal(srv)
	}
}
