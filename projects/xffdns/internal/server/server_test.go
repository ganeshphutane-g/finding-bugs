package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/config"
	"github.com/ganeshphutane-g/xffdns/internal/dnsmsg"
	"github.com/ganeshphutane-g/xffdns/internal/metrics"
	"github.com/ganeshphutane-g/xffdns/internal/upstream"
)

// call is one exchange the fake upstream saw.
type call struct {
	client netip.Addr
	id     string
	name   string
}

// fake answers each query with the query itself, QR set, after a delay
// that queries for slow.* ask for; big.* gets a 2000-byte answer and fail.*
// an error.
type fake struct {
	mu    sync.Mutex
	calls []call
}

func (f *fake) Exchange(ctx context.Context, q []byte, client netip.Addr, id string) ([]byte, error) {
	in, err := dnsmsg.Parse(q)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{client, id, in.Name})
	f.mu.Unlock()
	if strings.HasPrefix(in.Name, "slow.") {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if strings.HasPrefix(in.Name, "fail.") {
		return nil, upstream.ErrTimeout
	}
	a := append([]byte(nil), q...)
	a[2] |= 0x80
	if strings.HasPrefix(in.Name, "big.") {
		a = append(a, make([]byte, 2000)...)
	}
	return a, nil
}

func (f *fake) last(t *testing.T) call {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("no query reached the upstream")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func query(id uint16, name string) []byte {
	m := make([]byte, 12)
	binary.BigEndian.PutUint16(m, id)
	m[2] = 1 // RD
	m[5] = 1
	for _, l := range strings.Split(name, ".") {
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	return append(m, 0, 0, 1, 0, 1)
}

func v2(src string, udp bool) []byte {
	ap := netip.MustParseAddrPort(src)
	b := []byte("\r\n\r\n\x00\r\nQUIT\n")
	fam := byte(0x11)
	if udp {
		fam = 0x12
	}
	b = append(b, 0x21, fam, 0, 12)
	b = append(b, ap.Addr().AsSlice()...)
	b = append(b, 127, 0, 0, 1)
	b = binary.BigEndian.AppendUint16(b, ap.Port())
	return binary.BigEndian.AppendUint16(b, 53)
}

func limits(mod func(*config.Limits)) config.Limits {
	qps := 0
	l := config.Limits{UDPQPS: &qps, SubnetV4: 24, SubnetV6: 56, Clients: 100, MaxConns: 100, MaxInflightPerConn: 8,
		MaxInflight: 100, IdleTimeout: config.Duration(5 * time.Second), HandshakeTimeout: config.Duration(2 * time.Second)}
	if mod != nil {
		mod(&l)
	}
	return l
}

func start(t *testing.T, f *fake, lim config.Limits, ls ...config.Listener) (*Server, *metrics.Metrics) {
	t.Helper()
	m := metrics.New()
	s := New(Options{Upstream: f, Limits: lim, Metrics: m})
	for i := range ls {
		ls[i].Address = "127.0.0.1:0"
		for _, p := range ls[i].ProxyFrom {
			ls[i].Trusted = append(ls[i].Trusted, netip.MustParsePrefix(p))
		}
	}
	if err := s.Start(ls); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, m
}

func udpAsk(t *testing.T, addr net.Addr, pkt []byte) []byte {
	t.Helper()
	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(pkt)
	c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func metricsText(m *metrics.Metrics) string {
	var b strings.Builder
	m.Write(&b)
	return b.String()
}

func TestUDP(t *testing.T) {
	f := &fake{}
	s, m := start(t, f, limits(nil),
		config.Listener{Name: "u", Protocol: "udp", ClientID: "lan"},
		config.Listener{Name: "up", Protocol: "udp", ProxyProtocol: true, ProxyFrom: []string{"127.0.0.1/32"}},
		config.Listener{Name: "uo", Protocol: "udp", ProxyProtocol: true, ProxyFrom: []string{"10.0.0.0/8"}})

	ans := udpAsk(t, s.Addr("u"), query(0x1111, "example.com"))
	if dnsmsg.ID(ans) != 0x1111 || ans[2]&0x80 == 0 {
		t.Fatalf("%x", ans)
	}
	if c := f.last(t); c.client.String() != "127.0.0.1" || c.id != "lan" || c.name != "example.com." {
		t.Fatalf("%+v", c)
	}

	// From the PROXY header; the answer goes back plain.
	ans = udpAsk(t, s.Addr("up"), append(v2("192.0.2.10:5000", true), query(0x2222, "proxied.example")...))
	if dnsmsg.ID(ans) != 0x2222 {
		t.Fatalf("%x", ans)
	}
	if c := f.last(t); c.client.String() != "192.0.2.10" || c.id != "" {
		t.Fatalf("%+v", c)
	}
	// No header, or a source not in proxy_from: dropped.
	n := f.count()
	if udpAsk(t, s.Addr("up"), query(3, "nohdr.example")) != nil ||
		udpAsk(t, s.Addr("uo"), append(v2("192.0.2.10:5000", true), query(4, "untrusted.example")...)) != nil {
		t.Fatal("answered")
	}
	if f.count() != n {
		t.Fatal("a dropped query reached the upstream")
	}
	// An answer too big for 512 bytes is truncated; an upstream failure is SERVFAIL.
	ans = udpAsk(t, s.Addr("u"), query(5, "big.example"))
	if len(ans) > 512 || ans[2]&0x02 == 0 || dnsmsg.ID(ans) != 5 {
		t.Fatalf("%d %x", len(ans), ans[:4])
	}
	ans = udpAsk(t, s.Addr("u"), query(6, "fail.example"))
	if ans == nil || ans[3]&0xF != dnsmsg.RcodeServFail {
		t.Fatalf("%x", ans)
	}
	// Not a query: dropped.
	resp := query(7, "x.example")
	resp[2] |= 0x80
	if udpAsk(t, s.Addr("u"), resp) != nil || udpAsk(t, s.Addr("u"), []byte{1, 2, 3}) != nil {
		t.Fatal("answered junk")
	}
	out := metricsText(m)
	for _, want := range []string{
		`xffdns_queries_total{listener="u",result="truncated"} 1`,
		`xffdns_queries_total{listener="u",result="servfail"} 1`,
		`xffdns_queries_total{listener="u",result="malformed"} 2`,
		`xffdns_proxy_errors_total{listener="up",reason="bad_header"} 1`,
		`xffdns_proxy_errors_total{listener="uo",reason="untrusted_source"} 1`,
		`xffdns_upstream_errors_total{kind="timeout"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
}

func TestUDPRateLimit(t *testing.T) {
	f := &fake{}
	s, m := start(t, f, limits(func(l *config.Limits) { n := 2; l.UDPQPS = &n }),
		config.Listener{Name: "u", Protocol: "udp", ProxyProtocol: true, ProxyFrom: []string{"127.0.0.1/32"}})
	// A burst of 4 (sent before any answer is read): 2 get through.
	c, err := net.Dial("udp", s.Addr("u").String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 4 {
		c.Write(append(v2("192.0.2.10:5000", true), query(uint16(i), "a.example")...))
	}
	ok := 0
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		if _, err := c.Read(make([]byte, 512)); err != nil {
			break
		}
		ok++
	}
	// Another client, behind the same proxy, has its own budget.
	if ok != 2 || udpAsk(t, s.Addr("u"), append(v2("198.51.100.7:5000", true), query(9, "b.example")...)) == nil {
		t.Fatalf("%d answered", ok)
	}
	if !strings.Contains(metricsText(m), `result="rate_limited"} 2`) {
		t.Fatal(metricsText(m))
	}
}

// tcpConn writes framed queries and reads framed answers.
type tcpConn struct {
	net.Conn
}

func (c tcpConn) send(t *testing.T, qs ...[]byte) {
	t.Helper()
	var out []byte
	for _, q := range qs {
		out = binary.BigEndian.AppendUint16(out, uint16(len(q)))
		out = append(out, q...)
	}
	if _, err := c.Write(out); err != nil {
		t.Fatal(err)
	}
}

func (c tcpConn) recv(t *testing.T) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var lb [2]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		t.Fatal(err)
	}
	m := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(c, m); err != nil {
		t.Fatal(err)
	}
	return m
}

func closed(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

func TestTCP(t *testing.T) {
	f := &fake{}
	s, _ := start(t, f, limits(nil),
		config.Listener{Name: "t", Protocol: "tcp"},
		config.Listener{Name: "tp", Protocol: "tcp", ProxyProtocol: true, ProxyFrom: []string{"127.0.0.0/8"}})
	c, err := net.Dial("tcp", s.Addr("t").String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tcpConn{c}
	// Pipelined: the slow one is answered last; a big answer is not truncated.
	tc.send(t, query(1, "slow.example"), query(2, "fast.example"), query(3, "big.example"))
	got := map[uint16]int{}
	var order []uint16
	for range 3 {
		a := tc.recv(t)
		got[dnsmsg.ID(a)] = len(a)
		order = append(order, dnsmsg.ID(a))
	}
	if order[2] != 1 || got[3] < 2000 || len(got) != 3 {
		t.Fatalf("order %v sizes %v", order, got)
	}

	// PROXY v1, with the query in the same write as the header.
	p, err := net.Dial("tcp", s.Addr("tp").String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q := query(4, "v1.example")
	p.Write(append([]byte("PROXY TCP6 2001:db8::5 2001:db8::1 40000 53\r\n\x00"+string(rune(len(q)))), q...))
	if a := (tcpConn{p}).recv(t); dnsmsg.ID(a) != 4 {
		t.Fatalf("%x", a)
	}
	if c := f.last(t); c.client.String() != "2001:db8::5" {
		t.Fatalf("%+v", c)
	}
	// v2 LOCAL (a health check): the connection's own address.
	p2, _ := net.Dial("tcp", s.Addr("tp").String())
	defer p2.Close()
	local := []byte("\r\n\r\n\x00\r\nQUIT\n\x20\x00\x00\x00")
	p2.Write(local)
	(tcpConn{p2}).send(t, query(5, "local.example"))
	(tcpConn{p2}).recv(t)
	if c := f.last(t); c.client.String() != "127.0.0.1" {
		t.Fatalf("%+v", c)
	}
	// No header: closed without an answer.
	p3, _ := net.Dial("tcp", s.Addr("tp").String())
	defer p3.Close()
	(tcpConn{p3}).send(t, query(6, "nohdr.example"))
	if !closed(p3) {
		t.Fatal("a connection without a header stayed open")
	}
	// A malformed query closes the connection.
	c2, _ := net.Dial("tcp", s.Addr("t").String())
	defer c2.Close()
	(tcpConn{c2}).send(t, []byte{1, 2, 3})
	if !closed(c2) {
		t.Fatal("still open after junk")
	}
}

func TestTCPLimits(t *testing.T) {
	f := &fake{}
	s, m := start(t, f, limits(func(l *config.Limits) {
		l.MaxConns = 1
		l.IdleTimeout = config.Duration(300 * time.Millisecond)
	}), config.Listener{Name: "t", Protocol: "tcp"},
		config.Listener{Name: "tu", Protocol: "tcp", ProxyProtocol: true, ProxyFrom: []string{"10.0.0.0/8"}})
	c, _ := net.Dial("tcp", s.Addr("t").String())
	defer c.Close()
	(tcpConn{c}).send(t, query(1, "a.example"))
	(tcpConn{c}).recv(t)
	c2, _ := net.Dial("tcp", s.Addr("t").String())
	defer c2.Close()
	if !closed(c2) {
		t.Fatal("a connection over max_conns stayed open")
	}
	// Idle: closed after the timeout.
	start := time.Now()
	if !closed(c) || time.Since(start) > time.Second {
		t.Fatal("an idle connection stayed open")
	}
	// A source not in proxy_from is closed at once.
	u, _ := net.Dial("tcp", s.Addr("tu").String())
	defer u.Close()
	if !closed(u) {
		t.Fatal("an untrusted source stayed open")
	}
	out := metricsText(m)
	if !strings.Contains(out, `xffdns_conns_refused_total{listener="t",reason="max_conns"} 1`) ||
		!strings.Contains(out, `reason="untrusted_source"} 1`) {
		t.Fatal(out)
	}
}

// cert writes a self-signed certificate for *.dns.example.test.
func cert(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dns.example.test"},
		DNSNames: []string{"dns.example.test", "*.dns.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	pool := x509.NewCertPool()
	x, _ := x509.ParseCertificate(der)
	pool.AddCert(x)
	return cf, kf, pool
}

func TestDoT(t *testing.T) {
	cf, kf, pool := cert(t)
	f := &fake{}
	s, _ := start(t, f, limits(nil),
		config.Listener{Name: "d", Protocol: "dot", CertFile: cf, KeyFile: kf, ServerName: "dns.example.test"},
		config.Listener{Name: "dp", Protocol: "dot", CertFile: cf, KeyFile: kf, ServerName: "dns.example.test",
			ProxyProtocol: true, ProxyFrom: []string{"127.0.0.1/32"}})
	for sni, want := range map[string]string{"kid.dns.example.test": "kid", "dns.example.test": "", "a.b.dns.example.test": ""} {
		conf := &tls.Config{RootCAs: pool, ServerName: sni, NextProtos: []string{"dot"}}
		if sni == "a.b.dns.example.test" {
			conf.InsecureSkipVerify = true // the wildcard does not cover two labels
		}
		c, err := tls.Dial("tcp", s.Addr("d").String(), conf)
		if err != nil {
			t.Fatal(sni, err)
		}
		if c.ConnectionState().NegotiatedProtocol != "dot" {
			t.Fatal("ALPN dot was not chosen")
		}
		(tcpConn{c}).send(t, query(1, "example.com"))
		(tcpConn{c}).recv(t)
		c.Close()
		if got := f.last(t); got.id != want {
			t.Fatalf("SNI %s: client ID %q", sni, got.id)
		}
	}
	// A client offering only another protocol is still served.
	c, err := tls.Dial("tcp", s.Addr("d").String(), &tls.Config{RootCAs: pool, ServerName: "dns.example.test", NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// Behind a passthrough proxy: the PROXY header, then TLS.
	raw, err := net.Dial("tcp", s.Addr("dp").String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write(v2("203.0.113.9:4444", false))
	tc := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "phone.dns.example.test"})
	(tcpConn{tc}).send(t, query(2, "example.com"))
	if a := (tcpConn{tc}).recv(t); dnsmsg.ID(a) != 2 {
		t.Fatalf("%x", a)
	}
	if got := f.last(t); got.client.String() != "203.0.113.9" || got.id != "phone" {
		t.Fatalf("%+v", got)
	}
	// Plain DNS on a DoT port: the handshake fails and the connection closes.
	p, _ := net.Dial("tcp", s.Addr("d").String())
	defer p.Close()
	(tcpConn{p}).send(t, query(3, "plain.example"))
	if !closed(p) {
		t.Fatal("plain DNS on DoT stayed open")
	}
}

func TestShutdownWaits(t *testing.T) {
	f := &fake{}
	m := metrics.New()
	s := New(Options{Upstream: f, Limits: limits(nil), Metrics: m})
	if err := s.Start([]config.Listener{{Name: "t", Protocol: "tcp", Address: "127.0.0.1:0"}, {Name: "u", Protocol: "udp", Address: "127.0.0.1:0"}}); err != nil {
		t.Fatal(err)
	}
	c, _ := net.Dial("tcp", s.Addr("t").String())
	defer c.Close()
	(tcpConn{c}).send(t, query(1, "slow.example"))
	u, _ := net.Dial("udp", s.Addr("u").String())
	defer u.Close()
	u.Write(query(2, "slow.example"))
	for f.count() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		close(done)
	}()
	if a := (tcpConn{c}).recv(t); dnsmsg.ID(a) != 1 {
		t.Fatal("the TCP answer was lost")
	}
	u.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	if n, err := u.Read(buf); err != nil || dnsmsg.ID(buf[:n]) != 2 {
		t.Fatal("the UDP answer was lost", err)
	}
	<-done
	if _, err := net.Dial("tcp", s.Addr("t").String()); err == nil {
		t.Fatal("still accepting after shutdown")
	}
}

func TestShutdownGivesUp(t *testing.T) {
	f := &fake{}
	s := New(Options{Upstream: f, Limits: limits(nil)})
	s.Start([]config.Listener{{Name: "t", Protocol: "tcp", Address: "127.0.0.1:0"}})
	c, _ := net.Dial("tcp", s.Addr("t").String())
	defer c.Close()
	(tcpConn{c}).send(t, query(1, "slow.example"))
	for f.count() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Shutdown(ctx)
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("shutdown took %s", time.Since(start))
	}
}

func TestStartFails(t *testing.T) {
	busy, _ := net.Listen("tcp", "127.0.0.1:0")
	defer busy.Close()
	s := New(Options{Upstream: &fake{}, Limits: limits(nil)})
	err := s.Start([]config.Listener{{Name: "ok", Protocol: "udp", Address: "127.0.0.1:0"}, {Name: "taken", Protocol: "tcp", Address: busy.Addr().String()}})
	if err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatal(err)
	}
	err = s.Start([]config.Listener{{Name: "nocert", Protocol: "dot", Address: "127.0.0.1:0", CertFile: "/nonexistent", KeyFile: "/nonexistent"}})
	if err == nil {
		t.Fatal("started without a certificate")
	}
}

func TestClientIDFromSNI(t *testing.T) {
	for sni, want := range map[string]string{"kid.dns.example.com": "kid", "KID.DNS.example.com.": "kid", "dns.example.com": "",
		"kid.other.com": "", "a_b.dns.example.com": "", "xdns.example.com": ""} {
		if got := clientIDFromSNI(sni, "dns.example.com"); got != want {
			t.Errorf("%s: %q", sni, got)
		}
	}
}
