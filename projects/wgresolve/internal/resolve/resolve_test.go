package resolve

import (
	"context"
	"errors"
	"net"
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

	a := r.Lookup(ctx, "home.example")
	if a.Err != nil || len(a.Addrs) != 2 || a.Addrs[0].String() != "203.0.113.7" || a.Addrs[1].String() != "2001:db8::7" {
		t.Fatalf("%+v", a)
	}
	if a.Expires != now.Add(60*time.Second) {
		t.Fatalf("ttl: %v", a.Expires.Sub(now))
	}
	// Cached until the TTL runs out.
	n := queries.Load()
	r.Lookup(ctx, "home.example")
	if queries.Load() != n {
		t.Fatal("asked again within the TTL")
	}
	now = now.Add(61 * time.Second)
	r.Lookup(ctx, "home.example")
	if queries.Load() == n {
		t.Fatal("not asked again after the TTL")
	}
	// TTL 5 is held for MinTTL.
	if a := r.Lookup(ctx, "v4only.example"); a.Err != nil || len(a.Addrs) != 1 || a.Expires != now.Add(MinTTL) {
		t.Fatalf("%+v", a)
	}
	// Truncated over UDP: asked again over TCP.
	if a := r.Lookup(ctx, "big.example"); a.Err != nil || a.Addrs[0].String() != "203.0.113.9" {
		t.Fatalf("%+v", a)
	}
	// A single label goes through the search domains.
	if a := r.Lookup(ctx, "short"); a.Err != nil || a.Addrs[0].String() != "10.0.0.5" {
		t.Fatalf("%+v", a)
	}
	// Missing.
	if a := r.Lookup(ctx, "gone.example"); !errors.Is(a.Err, ErrNoSuchName) {
		t.Fatalf("%+v", a)
	}
	// Prefer and family.
	r6 := &Resolver{Servers: []string{srv}, Prefer: "ipv6"}
	if a := r6.Lookup(ctx, "home.example"); a.Addrs[0].String() != "2001:db8::7" {
		t.Fatalf("%+v", a)
	}
	r4 := &Resolver{Servers: []string{srv}, Family: "ipv4"}
	if a := r4.Lookup(ctx, "home.example"); len(a.Addrs) != 1 || !a.Addrs[0].Is4() {
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
	a := r.Lookup(context.Background(), "x.example")
	if a.Err != nil || a.Server != good {
		t.Fatalf("%+v", a)
	}
	r = &Resolver{Servers: []string{dead, bad}, Timeout: 300 * time.Millisecond}
	a = r.Lookup(context.Background(), "x.example")
	if a.Err == nil || !strings.Contains(a.Err.Error(), "SERVFAIL") {
		t.Fatalf("%+v", a)
	}
	// A failure is not cached: the next round asks again.
	if a.Expires.After(time.Now().Add(time.Second)) {
		t.Fatal("a failure was cached")
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
	if a := r.Lookup(context.Background(), "x.example"); a.Err != nil || len(a.Addrs) != 1 {
		t.Fatalf("%+v", a)
	}
}
