package watch

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ganeshphutane-g/wgresolve/internal/device"
	"github.com/ganeshphutane-g/wgresolve/internal/resolve"
	"github.com/ganeshphutane-g/wgresolve/internal/source"
)

type fakeDevices struct {
	mu    sync.Mutex
	peers map[string]map[wgtypes.Key]device.Peer
	sets  []string
}

func (f *fakeDevices) Device(iface string) (*device.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.peers[iface]
	if !ok {
		return nil, device.ErrNoDevice
	}
	out := map[wgtypes.Key]device.Peer{}
	for k, v := range m {
		out[k] = v
	}
	return &device.Device{Peers: out}, nil
}

func (f *fakeDevices) SetEndpoint(iface string, k wgtypes.Key, ep netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.peers[iface][k]
	p.Endpoint = ep
	f.peers[iface][k] = p
	f.sets = append(f.sets, iface+" "+ep.String())
	return nil
}

func (f *fakeDevices) Close() error { return nil }

// dnsServer answers A queries for any name with the current address.
type dnsServer struct {
	mu   sync.Mutex
	addr string // "" answers NXDOMAIN
	n    int
}

func (d *dnsServer) start(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.n++
		m := new(dns.Msg)
		m.SetReply(q)
		if d.addr == "" {
			m.Rcode = dns.RcodeNameError
		} else if q.Question[0].Qtype == dns.TypeA {
			rr, _ := dns.NewRR(q.Question[0].Name + " 60 IN A " + d.addr)
			m.Answer = append(m.Answer, rr)
		}
		w.WriteMsg(m)
	})}
	go s.ActivateAndServe()
	t.Cleanup(func() { s.Shutdown() })
	return pc.LocalAddr().String()
}

func (d *dnsServer) set(a string) { d.mu.Lock(); d.addr = a; d.mu.Unlock() }

func TestRounds(t *testing.T) {
	k1, _ := wgtypes.GeneratePrivateKey()
	k2, _ := wgtypes.GeneratePrivateKey()
	named, fixed := k1.PublicKey(), k2.PublicKey()
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time { return now }
	ds := &dnsServer{addr: "203.0.113.1"}
	srv := ds.start(t)
	dev := &fakeDevices{peers: map[string]map[wgtypes.Key]device.Peer{"wg0": {
		named: {Endpoint: netip.MustParseAddrPort("203.0.113.1:51820"), LastHandshake: now.Add(-time.Minute)},
		fixed: {Endpoint: netip.MustParseAddrPort("198.51.100.5:51820"), LastHandshake: now.Add(-time.Hour)},
	}}}
	var log bytes.Buffer
	w := &Watcher{
		Load: func() (*source.Result, error) {
			r := &source.Result{}
			r.Add("wg0", named.String(), "home.example:51820", "test")
			r.Add("wg0", fixed.String(), "198.51.100.5:51820", "test")
			r.Add("wg9", named.String(), "home.example:51820", "test")
			return r, nil
		},
		Resolver: &resolve.Resolver{Servers: []string{srv}, Now: clock},
		Devices:  dev, Log: &log, Now: clock,
	}
	if err := w.Reload(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w.Round(ctx)
	st := w.Statuses()
	if len(st) != 2 || st[0].State != "ok" || st[1].State != "no device" || ds.n != 0 {
		t.Fatalf("%+v (dns asked %d times)", st, ds.n)
	}
	// The server moves; the handshake goes stale.
	ds.set("203.0.113.2")
	now = now.Add(5 * time.Minute)
	w.Round(ctx)
	if len(dev.sets) != 1 || dev.sets[0] != "wg0 203.0.113.2:51820" {
		t.Fatalf("%v\n%s", dev.sets, log.String())
	}
	if !strings.Contains(log.String(), "endpoint 203.0.113.1:51820 -> 203.0.113.2:51820") {
		t.Fatal(log.String())
	}
	// Next round: waiting for a handshake, no change.
	now = now.Add(30 * time.Second)
	w.Round(ctx)
	if len(dev.sets) != 1 || w.Statuses()[0].State != "waiting" {
		t.Fatalf("%v %+v", dev.sets, w.Statuses()[0])
	}
	// The handshake comes back.
	dev.peers["wg0"][named] = device.Peer{Endpoint: netip.MustParseAddrPort("203.0.113.2:51820"), LastHandshake: now}
	w.Round(ctx)
	if s := w.Statuses()[0]; s.State != "ok" || s.Changes != 1 {
		t.Fatalf("%+v", s)
	}
	// The fixed-address peer was never touched.
	if p := dev.peers["wg0"][fixed]; p.Endpoint.String() != "198.51.100.5:51820" {
		t.Fatal(p)
	}
	// A name that stops resolving: reported once in the log, not on every round.
	ds.set("")
	now = now.Add(10 * time.Minute)
	n := strings.Count(log.String(), "cannot resolve")
	w.Round(ctx)
	now = now.Add(time.Minute)
	w.Resolver.Forget("home.example")
	w.Round(ctx)
	if c := strings.Count(log.String(), "cannot resolve"); c != n+1 || w.Statuses()[0].State != "dns error" {
		t.Fatalf("%d\n%s", c, log.String())
	}
}

func TestDryRunAndNoEndpoint(t *testing.T) {
	k1, _ := wgtypes.GeneratePrivateKey()
	pub := k1.PublicKey()
	now := time.Unix(1_000_000, 0)
	ds := &dnsServer{addr: "203.0.113.9"}
	srv := ds.start(t)
	dev := &fakeDevices{peers: map[string]map[wgtypes.Key]device.Peer{"wg0": {pub: {}}}}
	w := &Watcher{
		Load: func() (*source.Result, error) {
			r := &source.Result{}
			r.Add("wg0", pub.String(), "late.example:51820", "test")
			return r, nil
		},
		Resolver: &resolve.Resolver{Servers: []string{srv}},
		Devices:  dev, DryRun: true, Now: func() time.Time { return now },
	}
	w.Reload()
	w.Round(context.Background())
	if s := w.Statuses()[0]; s.State != "would change" || len(dev.sets) != 0 || !strings.Contains(s.Reason, "no endpoint yet") {
		t.Fatalf("%+v %v", s, dev.sets)
	}
	w.DryRun = false
	w.Round(context.Background())
	if len(dev.sets) != 1 {
		t.Fatal(dev.sets)
	}
}
