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

// maxPending bounds the bytes of TCP answers waiting on slow readers, in
// all; past it, a connection that would add more is closed.
const maxPending = 64 << 20

// MaxQuery is the largest query taken. Queries are small (a name, an OPT
// record); allowing 64 KB would let each one cost that much memory.
const MaxQuery = 4096

// udpBuf holds the largest datagram taken: a PROXY v2 header and a query.
const udpBuf = 16 + 4096 + MaxQuery

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
	o          Options
	limiter    *ratelimit.Limiter
	inflight   chan struct{}
	conns      chan struct{}
	ctx        context.Context // cancelled when shutdown runs out of time
	cancel     context.CancelFunc
	closing    atomic.Bool
	pending    atomic.Int64   // bytes of TCP answers not yet written
	maxPending atomic.Int64   // maxPending (less in tests)
	wg         sync.WaitGroup // every goroutine that handles a query or connection
	loops      sync.WaitGroup // the accept and read loops

	mu        sync.Mutex
	bound     []*bound
	open      map[net.Conn]struct{}
	perClient map[netip.Prefix]*clientState // by client: an IPv4 address, an IPv6 subnet_v6

	logMu   sync.Mutex
	logLast map[string]time.Time
}

type bound struct {
	conf config.Listener
	udp  []*udpSock
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
	s := &Server{
		o:         o,
		limiter:   ratelimit.New(*l.UDPQPS, l.SubnetV4, l.SubnetV6, l.Clients),
		inflight:  make(chan struct{}, l.MaxInflight),
		conns:     make(chan struct{}, l.MaxConns),
		ctx:       ctx,
		cancel:    cancel,
		open:      map[net.Conn]struct{}{},
		perClient: map[netip.Prefix]*clientState{},
		logLast:   map[string]time.Time{},
	}
	s.maxPending.Store(maxPending)
	return s
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
		if len(b.udp) > 0 {
			s.loops.Done()
			for _, u := range b.udp {
				s.loops.Add(1)
				go func() {
					defer s.loops.Done()
					s.serveUDP(b, u)
				}()
			}
		} else {
			go func() {
				defer s.loops.Done()
				s.serveTCP(b)
			}()
		}
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
		b.udp, err = listenUDP(l.Address)
	} else {
		b.ln, err = net.Listen("tcp", l.Address)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (b *bound) addr() net.Addr {
	if len(b.udp) > 0 {
		return b.udp[0].pc.LocalAddr()
	}
	return b.ln.Addr()
}

func (b *bound) close() {
	for _, u := range b.udp {
		u.pc.Close()
	}
	if b.ln != nil {
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
		for _, u := range b.udp {
			u.pc.SetReadDeadline(time.Now()) // stop reading; answers can still go out
		}
		if b.ln != nil {
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
		for _, u := range b.udp {
			u.pc.Close()
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
	s.o.Metrics.UpstreamOK()
	return ans, metrics.Answered
}

// isDial reports whether err is a failure to connect to AdGuard Home.
func isDial(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (s *Server) serveUDP(b *bound, u *udpSock) {
	buf := make([]byte, udpBuf+1) // one more, to tell a datagram that is too big
	name := b.conf.Name
	for {
		n, to, from, err := u.read(buf)
		if err != nil {
			if s.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logEvery("udp-read-"+name, "reading a UDP datagram failed", "listener", name, "error", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		peer := addrPort(from)
		if n > udpBuf {
			s.o.Metrics.Query(name, metrics.Malformed)
			continue
		}
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
		// The limit comes first: junk counts against it too.
		if !s.limiter.Allow(client) {
			s.o.Metrics.Query(name, metrics.RateLimited)
			continue
		}
		if len(pkt) > MaxQuery || dnsmsg.CheckQuery(pkt) != nil {
			s.o.Metrics.Query(name, metrics.Malformed)
			continue
		}
		msg := append([]byte(nil), pkt...)
		q, err := dnsmsg.Parse(msg)
		if err != nil {
			s.o.Metrics.Query(name, metrics.Malformed)
			continue
		}
		cs := s.clientFor(client)
		busy := true
		select {
		case cs.inflight <- struct{}{}:
			select {
			case s.inflight <- struct{}{}:
				busy = false
			default:
				<-cs.inflight
			}
		default:
		}
		if busy {
			s.release(cs)
			s.o.Metrics.Query(name, metrics.Busy)
			u.write(dnsmsg.Reply(q, dnsmsg.RcodeServFail, false), to, from)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(cs)
			defer func() { <-cs.inflight }()
			ans, result := s.exchange(msg, q, client, b.conf.ClientID)
			<-s.inflight
			if fit := dnsmsg.FitUDP(q, ans); len(fit) != len(ans) {
				ans, result = fit, metrics.Truncated
			}
			if _, err := u.write(ans, to, from); err != nil && !s.closing.Load() {
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

// clientState is what one client has open: connections, and queries not
// yet answered (counted until the answer is written, so a client that is
// slow to read only uses up its own share).
type clientState struct {
	key      netip.Prefix
	refs     int
	conns    int
	inflight chan struct{}
}

func (s *Server) clientKey(a netip.Addr) netip.Prefix {
	bits := 32
	if !a.Is4() {
		bits = s.o.Limits.SubnetV6
	}
	p, _ := a.WithZone("").Prefix(bits)
	return p
}

// clientFor returns client's state, to be given back with s.release.
func (s *Server) clientFor(a netip.Addr) *clientState {
	k := s.clientKey(a)
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.perClient[k]
	if cs == nil {
		cs = &clientState{key: k, inflight: make(chan struct{}, s.o.Limits.MaxInflightPerClient)}
		s.perClient[k] = cs
	}
	cs.refs++
	return cs
}

func (s *Server) release(cs *clientState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs.refs--; cs.refs <= 0 {
		delete(s.perClient, cs.key)
	}
}

// addConn counts a connection for the client, unless it has too many.
func (s *Server) addConn(cs *clientState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs.conns >= s.o.Limits.MaxConnsPerClient {
		return false
	}
	cs.conns++
	return true
}

func (s *Server) dropConn(cs *clientState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs.conns--
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
		c.SetReadDeadline(time.Time{}) // the handshake has its own timeout
	}
	cs := s.clientFor(client)
	defer s.release(cs)
	if !s.addConn(cs) {
		s.o.Metrics.ConnRefused(name, "max_conns_per_client")
		s.logEvery("max-conns-client-"+name, "closed a connection: the client has limits.max_conns_per_client open "+
			"(behind a proxy with no PROXY header, every connection is from the proxy)", "listener", name, "client", client)
		return
	}
	defer s.dropConn(cs)
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
	s.queries(b, conn, r, client, cs, id)
}

// queries reads queries off one connection until it is idle, closed or
// broken, answering each as AdGuard Home does, in any order (RFC 7766).
func (s *Server) queries(b *bound, conn net.Conn, r *bufio.Reader, client netip.Addr, cs *clientState, id string) {
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
		if n > MaxQuery {
			s.o.Metrics.Query(name, metrics.Malformed)
			return
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(r, msg); err != nil {
			return
		}
		if err := dnsmsg.CheckQuery(msg); err != nil {
			s.o.Metrics.Query(name, metrics.Malformed)
			return
		}
		q, err := dnsmsg.Parse(msg)
		if err != nil {
			s.o.Metrics.Query(name, metrics.Malformed)
			return
		}
		// All three block: a busy connection, or client, stops being read
		// until its queries are answered.
		perConn <- struct{}{}
		select {
		case cs.inflight <- struct{}{}:
		case <-s.ctx.Done():
			<-perConn
			return
		}
		select {
		case s.inflight <- struct{}{}:
		case <-s.ctx.Done():
			<-cs.inflight
			<-perConn
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-perConn; <-cs.inflight }()
			ans, result := s.exchange(msg, q, client, id)
			// The global slot is for queries waiting on AdGuard Home, not on
			// a slow reader.
			<-s.inflight
			if s.pending.Add(int64(len(ans))) > s.maxPending.Load() {
				s.pending.Add(-int64(len(ans)))
				s.o.Metrics.Query(name, metrics.Busy)
				s.logEvery("pending-"+name, "closed a connection: too many answers are waiting on slow readers", "listener", name, "client", client)
				conn.Close()
				return
			}
			defer s.pending.Add(-int64(len(ans)))
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
