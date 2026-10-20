// Package server takes DNS queries over UDP, TCP and DoT, works out the real
// client (from a PROXY header, or the connection), and passes each query on
// to AdGuard Home.
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/certs"
	"github.com/ganeshphutane-g/xffdns/internal/config"
	"github.com/ganeshphutane-g/xffdns/internal/dnsmsg"
	"github.com/ganeshphutane-g/xffdns/internal/metrics"
	"github.com/ganeshphutane-g/xffdns/internal/proxyproto"
	"github.com/ganeshphutane-g/xffdns/internal/ratelimit"
	"github.com/ganeshphutane-g/xffdns/internal/upstream"
)

// writeTimeout bounds writing one answer to a TCP or DoT client.
const writeTimeout = 10 * time.Second

// udpBuf holds the largest datagram: a PROXY v2 header and a DNS message.
const udpBuf = 16 + 4096 + 65535

// Exchanger sends a query on to AdGuard Home (upstream.Client).
type Exchanger interface {
	Exchange(ctx context.Context, query []byte, client netip.Addr, clientID string) ([]byte, error)
}

// Options are what a Server needs.
type Options struct {
	Upstream Exchanger
	Limits   config.Limits
	Metrics  *metrics.Metrics
	Log      *slog.Logger
}

// Server serves listeners.
type Server struct {
	o        Options
	limiter  *ratelimit.Limiter
	inflight chan struct{}
	conns    chan struct{}
	ctx      context.Context // cancelled when shutdown runs out of time
	cancel   context.CancelFunc
	closing  atomic.Bool
	wg       sync.WaitGroup // every goroutine that handles a query or connection
	loops    sync.WaitGroup // the accept and read loops

	mu    sync.Mutex
	bound []*bound
	open  map[net.Conn]struct{}

	logMu   sync.Mutex
	logLast map[string]time.Time
}

type bound struct {
	conf config.Listener
	pc   net.PacketConn
	ln   net.Listener
	tls  *tls.Config
}

// New makes a Server.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := o.Limits
	return &Server{
		o:        o,
		limiter:  ratelimit.New(*l.UDPQPS, l.SubnetV4, l.SubnetV6, l.Clients),
		inflight: make(chan struct{}, l.MaxInflight),
		conns:    make(chan struct{}, l.MaxConns),
		ctx:      ctx,
		cancel:   cancel,
		open:     map[net.Conn]struct{}{},
		logLast:  map[string]time.Time{},
	}
}

// Start binds every listener (all or none) and serves them.
func (s *Server) Start(ls []config.Listener) error {
	var bs []*bound
	for _, l := range ls {
		b, err := s.bind(l)
		if err != nil {
			for _, b := range bs {
				b.close()
			}
			return fmt.Errorf("listener %s: %w", l.Name, err)
		}
		bs = append(bs, b)
	}
	s.mu.Lock()
	s.bound = append(s.bound, bs...)
	s.mu.Unlock()
	for _, b := range bs {
		s.loops.Add(1)
		go func() {
			defer s.loops.Done()
			if b.pc != nil {
				s.serveUDP(b)
			} else {
				s.serveTCP(b)
			}
		}()
		s.o.Log.Info("listening", "listener", b.conf.Name, "protocol", b.conf.Protocol, "address", b.addr().String(),
			"proxy_protocol", b.conf.ProxyProtocol)
	}
	return nil
}

func (s *Server) bind(l config.Listener) (*bound, error) {
	b := &bound{conf: l}
	if l.Protocol == "dot" {
		r, err := certs.New(l.CertFile, l.KeyFile)
		if err != nil {
			return nil, err
		}
		r.OnReload = func(err error) {
			s.o.Metrics.CertReload(l.Name, err == nil)
			if err != nil {
				s.o.Log.Error("the certificate files changed but cannot be used; keeping the old certificate", "listener", l.Name, "error", err)
			} else {
				s.o.Log.Info("certificate reloaded", "listener", l.Name)
			}
		}
		base := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
		withALPN := base.Clone()
		withALPN.NextProtos = []string{"dot"}
		// Offer ALPN "dot" only to clients that ask for it: with NextProtos
		// set, Go refuses a client that offers only other protocols.
		b.tls = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			if slices.Contains(h.SupportedProtos, "dot") {
				return withALPN, nil
			}
			return base, nil
		}}
	}
	var err error
	if l.Protocol == "udp" {
		b.pc, err = net.ListenPacket("udp", l.Address)
	} else {
		b.ln, err = net.Listen("tcp", l.Address)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (b *bound) addr() net.Addr {
	if b.pc != nil {
		return b.pc.LocalAddr()
	}
	return b.ln.Addr()
}

func (b *bound) close() {
	if b.pc != nil {
		b.pc.Close()
	} else {
		b.ln.Close()
	}
}

// Addr is where the listener named name listens (for tests, with port 0).
func (s *Server) Addr(name string) net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bound {
		if b.conf.Name == name {
			return b.addr()
		}
	}
	return nil
}

// Shutdown stops taking queries and waits for those in flight until ctx
// ends; then it drops the rest.
func (s *Server) Shutdown(ctx context.Context) {
	s.closing.Store(true)
	s.mu.Lock()
	for _, b := range s.bound {
		if b.pc != nil {
			b.pc.SetReadDeadline(time.Now()) // stop reading; answers can still go out
		} else {
			b.ln.Close()
		}
	}
	for c := range s.open {
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		s.mu.Lock()
		for c := range s.open {
			c.Close()
		}
		s.mu.Unlock()
		<-done
	}
	s.cancel()
	for _, b := range s.bound {
		if b.pc != nil {
			b.pc.Close()
		}
	}
}

// logEvery logs at most once a minute for each key.
func (s *Server) logEvery(key, msg string, args ...any) {
	s.logMu.Lock()
	now := time.Now()
	if now.Sub(s.logLast[key]) < time.Minute {
		s.logMu.Unlock()
		return
	}
	s.logLast[key] = now
	s.logMu.Unlock()
	s.o.Log.Warn(msg+" (logged at most once a minute)", args...)
}

func addrPort(a net.Addr) netip.AddrPort {
	var ap netip.AddrPort
	switch a := a.(type) {
	case *net.UDPAddr:
		ap = a.AddrPort()
	case *net.TCPAddr:
		ap = a.AddrPort()
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func trusted(from []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range from {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIDFromSNI takes the client ID from an SNI of <id>.<server>.
func clientIDFromSNI(sni, server string) string {
	sni = strings.TrimSuffix(strings.ToLower(sni), ".")
	id, ok := strings.CutSuffix(sni, "."+server)
	if !ok || !config.ValidClientID(id) {
		return ""
	}
	return id
}

// exchange asks AdGuard Home; on failure the answer is SERVFAIL.
func (s *Server) exchange(msg []byte, q *dnsmsg.Info, client netip.Addr, id string) ([]byte, string) {
	start := time.Now()
	ans, err := s.o.Upstream.Exchange(s.ctx, msg, client, id)
	s.o.Metrics.Upstream(time.Since(start))
	if err != nil {
		kind := "other"
		switch {
		case errors.Is(err, upstream.ErrTimeout):
			kind = "timeout"
		case errors.Is(err, upstream.ErrStatus):
			kind = "http_status"
		case errors.Is(err, upstream.ErrAnswer):
			kind = "bad_answer"
		case errors.Is(err, context.Canceled):
			kind = "shutdown"
		case isDial(err):
			kind = "connect"
		}
		s.o.Metrics.UpstreamError(kind)
		s.logEvery("upstream-"+kind, "AdGuard Home did not answer a query", "error", err)
		return dnsmsg.Reply(q, dnsmsg.RcodeServFail, false), metrics.ServFail
	}
	return ans, metrics.Answered
}

// isDial reports whether err is a failure to connect to AdGuard Home.
func isDial(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (s *Server) serveUDP(b *bound) {
	buf := make([]byte, udpBuf)
	name := b.conf.Name
	for {
		n, from, err := b.pc.ReadFrom(buf)
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logEvery("udp-read-"+name, "reading a UDP datagram failed", "listener", name, "error", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		peer := addrPort(from)
		pkt := buf[:n]
		client := peer.Addr()
		if b.conf.ProxyProtocol {
			if !trusted(b.conf.Trusted, client) {
				s.o.Metrics.ProxyError(name, "untrusted_source")
				s.logEvery("proxy-untrusted-"+name, "dropped a datagram from a source not in proxy_from", "listener", name, "source", client)
				continue
			}
			h, payload, err := proxyproto.ReadDatagram(pkt)
			if err != nil {
				s.o.Metrics.ProxyError(name, "bad_header")
				s.logEvery("proxy-bad-"+name, "dropped a datagram with no valid PROXY header", "listener", name, "source", client, "error", err)
				continue
			}
			if !h.Local {
				client = h.Source.Addr().Unmap()
			}
			pkt = payload
		}
		msg := append([]byte(nil), pkt...)
		q, err := dnsmsg.Parse(msg)
		if err != nil || !q.IsQuery() {
			s.o.Metrics.Query(name, metrics.Malformed)
			continue
		}
		if !s.limiter.Allow(client) {
			s.o.Metrics.Query(name, metrics.RateLimited)
			continue
		}
		select {
		case s.inflight <- struct{}{}:
		default:
			s.o.Metrics.Query(name, metrics.Busy)
			b.pc.WriteTo(dnsmsg.Reply(q, dnsmsg.RcodeServFail, false), from)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.inflight }()
			ans, result := s.exchange(msg, q, client, b.conf.ClientID)
			if fit := dnsmsg.FitUDP(q, ans); len(fit) != len(ans) {
				ans, result = fit, metrics.Truncated
			}
			if _, err := b.pc.WriteTo(ans, from); err != nil && !s.closing.Load() {
				s.logEvery("udp-write-"+name, "sending a UDP answer failed", "listener", name, "error", err)
			}
			s.o.Metrics.Query(name, result)
		}()
	}
}

func (s *Server) serveTCP(b *bound) {
	name := b.conf.Name
	for {
		c, err := b.ln.Accept()
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logEvery("accept-"+name, "accepting a connection failed", "listener", name, "error", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		peer := addrPort(c.RemoteAddr())
		if b.conf.ProxyProtocol && !trusted(b.conf.Trusted, peer.Addr()) {
			s.o.Metrics.ProxyError(name, "untrusted_source")
			s.logEvery("proxy-untrusted-"+name, "closed a connection from a source not in proxy_from", "listener", name, "source", peer.Addr())
			c.Close()
			continue
		}
		select {
		case s.conns <- struct{}{}:
		default:
			s.o.Metrics.ConnRefused(name, "max_conns")
			s.logEvery("max-conns", "closed a connection: limits.max_conns are open", "listener", name)
			c.Close()
			continue
		}
		s.track(c, true)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.conns }()
			defer s.track(c, false)
			s.handleConn(b, c, peer)
		}()
	}
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.open[c] = struct{}{}
		if s.closing.Load() {
			c.SetReadDeadline(time.Now())
		}
	} else {
		delete(s.open, c)
	}
}

// bufConn reads through r: what it buffered after a PROXY header belongs to
// the TLS handshake.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (s *Server) handleConn(b *bound, c net.Conn, peer netip.AddrPort) {
	defer c.Close()
	s.o.Metrics.Conn(1)
	defer s.o.Metrics.Conn(-1)
	name := b.conf.Name
	hs := time.Duration(s.o.Limits.HandshakeTimeout)
	client := peer.Addr()
	var r *bufio.Reader
	var conn net.Conn = c
	if b.conf.ProxyProtocol {
		c.SetReadDeadline(time.Now().Add(hs))
		r = bufio.NewReader(c)
		h, err := proxyproto.Read(r)
		if err != nil {
			s.o.Metrics.ProxyError(name, "bad_header")
			s.logEvery("proxy-bad-"+name, "closed a connection with no valid PROXY header", "listener", name, "source", client, "error", err)
			return
		}
		if !h.Local {
			client = h.Source.Addr().Unmap()
		}
		conn = &bufConn{Conn: c, r: r}
	}
	id := b.conf.ClientID
	if b.tls != nil {
		tc := tls.Server(conn, b.tls)
		ctx, cancel := context.WithTimeout(s.ctx, hs)
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			s.o.Metrics.ConnRefused(name, "tls_handshake")
			s.logEvery("tls-"+name, "a TLS handshake failed", "listener", name, "client", client, "error", err)
			return
		}
		if b.conf.ServerName != "" {
			id = clientIDFromSNI(tc.ConnectionState().ServerName, b.conf.ServerName)
		}
		conn, r = tc, bufio.NewReader(tc)
	} else if r == nil {
		r = bufio.NewReader(c)
	}
	s.queries(b, conn, r, client, id)
}

// queries reads queries off one connection until it is idle, closed or
// broken, answering each as AdGuard Home does, in any order (RFC 7766).
func (s *Server) queries(b *bound, conn net.Conn, r *bufio.Reader, client netip.Addr, id string) {
	name := b.conf.Name
	idle := time.Duration(s.o.Limits.IdleTimeout)
	perConn := make(chan struct{}, s.o.Limits.MaxInflightPerConn)
	var wmu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		if s.closing.Load() {
			return
		}
		var lb [2]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lb[:]))
		msg := make([]byte, n)
		if _, err := io.ReadFull(r, msg); err != nil {
			return
		}
		q, err := dnsmsg.Parse(msg)
		if err != nil || !q.IsQuery() {
			s.o.Metrics.Query(name, metrics.Malformed)
			return
		}
		// Both block: a busy connection stops being read until its
		// queries are answered.
		perConn <- struct{}{}
		select {
		case s.inflight <- struct{}{}:
		case <-s.ctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-s.inflight; <-perConn }()
			ans, result := s.exchange(msg, q, client, id)
			out := make([]byte, 2+len(ans))
			binary.BigEndian.PutUint16(out, uint16(len(ans)))
			copy(out[2:], ans)
			wmu.Lock()
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			_, err := conn.Write(out)
			wmu.Unlock()
			if err != nil {
				conn.Close() // the reader stops too
			}
			s.o.Metrics.Query(name, result)
		}()
	}
}
