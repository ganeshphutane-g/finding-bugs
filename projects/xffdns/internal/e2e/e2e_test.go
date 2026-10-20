// Package e2e runs xffdns between real clients, proxies and AdGuard Home.
// It needs XFFDNS_INTEGRATION=1 and Docker.
package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/cli"
	"github.com/ganeshphutane-g/xffdns/internal/config"
	"github.com/ganeshphutane-g/xffdns/internal/metrics"
	"github.com/ganeshphutane-g/xffdns/internal/server"
	"github.com/ganeshphutane-g/xffdns/internal/testkit"
	"github.com/ganeshphutane-g/xffdns/internal/upstream"
)

const serverName = "dns.example.test"

func query(name string) []byte {
	m := make([]byte, 12)
	binary.BigEndian.PutUint16(m, uint16(time.Now().UnixNano()))
	m[2], m[5] = 1, 1
	for _, l := range strings.Split(name, ".") {
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	return append(m, 0, 0, 1, 0, 1)
}

// client is one way of asking: a protocol, an address, and the source
// address to ask from (127.0.0.0/8 is all loopback on Linux).
type client struct {
	proto string // udp, tcp, dot
	addr  string
	from  string
	sni   string
	pool  *x509.CertPool
}

func (c client) ask(t *testing.T, name string) []byte {
	t.Helper()
	q := query(name)
	d := net.Dialer{Timeout: 3 * time.Second}
	if c.proto == "udp" {
		d.LocalAddr = &net.UDPAddr{IP: net.ParseIP(c.from)}
		conn, err := d.Dial("udp", c.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write(q)
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			return nil
		}
		return buf[:n]
	}
	d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(c.from)}
	conn, err := d.Dial("tcp", c.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if c.proto == "dot" {
		tc := tls.Client(conn, &tls.Config{ServerName: c.sni, RootCAs: c.pool, NextProtos: []string{"dot"}})
		if err := tc.Handshake(); err != nil {
			t.Fatalf("TLS to %s: %v", c.addr, err)
		}
		conn = tc
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
	var lb [2]byte
	if _, err := io.ReadFull(conn, lb[:]); err != nil {
		return nil
	}
	a := make([]byte, binary.BigEndian.Uint16(lb[:]))
	if _, err := io.ReadFull(conn, a); err != nil {
		return nil
	}
	return a
}

func blockedAnswer(a []byte) bool {
	return len(a) > 16 && a[3]&0xF == 0 && binary.BigEndian.Uint16(a[6:]) == 1 && bytes.Equal(a[len(a)-4:], []byte{0, 0, 0, 0})
}

func writeCert(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: serverName},
		DNSNames: []string{serverName, "*." + serverName}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kb, _ := x509.MarshalECPrivateKey(key)
	cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	pool := x509.NewCertPool()
	x, _ := x509.ParseCertificate(der)
	pool.AddCert(x)
	return cf, kf, pool
}

func TestThroughProxies(t *testing.T) {
	ag := testkit.StartAdGuard(t)
	// Names under example.com are blocked by a local rule, so they are
	// answered (and logged) without an upstream resolver.
	ag.SetRules(t, "||example.com^")
	ag.AddClient(t, "laptop", []string{"127.0.0.85"}, []string{"youtube"})
	ag.AddClient(t, "kidtab", []string{"kid"}, []string{"tiktok"})
	ag.Disallow(t, "127.0.0.87")

	// xffdns, in this process.
	cf, kf, pool := writeCert(t, t.TempDir())
	port := map[string]int{}
	for _, n := range []string{"udp", "tcp", "dot", "pp", "ppdot"} {
		port[n] = testkit.FreePort(t)
	}
	at := func(n string) string { return fmt.Sprintf("127.0.0.1:%d", port[n]) }
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	listeners := []config.Listener{
		{Name: "udp", Protocol: "udp", Address: at("udp")},
		{Name: "tcp", Protocol: "tcp", Address: at("tcp")},
		{Name: "dot", Protocol: "dot", Address: at("dot"), CertFile: cf, KeyFile: kf, ServerName: serverName},
		// Behind proxies: TCP and UDP on one port (dnsdist uses both), DoT passthrough.
		{Name: "pp-tcp", Protocol: "tcp", Address: at("pp"), ProxyProtocol: true, Trusted: trusted},
		{Name: "pp-udp", Protocol: "udp", Address: at("pp"), ProxyProtocol: true, Trusted: trusted},
		{Name: "pp-dot", Protocol: "dot", Address: at("ppdot"), CertFile: cf, KeyFile: kf, ServerName: serverName, ProxyProtocol: true, Trusted: trusted},
	}
	conf, err := config.Parse([]byte("upstream: {url: x}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	up, err := upstream.New(upstream.Options{URL: ag.Web + "/dns-query", Timeout: 4 * time.Second, MaxConns: conf.Limits.MaxInflight})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	srv := server.New(server.Options{Upstream: up, Limits: conf.Limits, Metrics: m,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err := srv.Start(listeners); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// The proxies, in front of it.
	dir := testkit.Dir(t)
	hap1, hap2, trf, dd := testkit.FreePort(t), testkit.FreePort(t), testkit.FreePort(t), testkit.FreePort(t)
	testkit.WriteFile(t, dir, "haproxy.cfg", fmt.Sprintf(`defaults
  mode tcp
  timeout connect 2s
  timeout client 10s
  timeout server 10s
frontend v1
  bind 127.0.0.1:%d
  default_backend b1
frontend v2
  bind 127.0.0.1:%d
  default_backend b2
backend b1
  server s %s send-proxy
backend b2
  server s %s send-proxy-v2
`, hap1, hap2, at("pp"), at("ppdot")))
	testkit.Run(t, "haproxy", "-v", dir+"/haproxy.cfg:/usr/local/etc/haproxy/haproxy.cfg:ro", image("HAPROXY", "haproxy:lts"))
	testkit.WriteFile(t, dir, "traefik.yml", fmt.Sprintf(`entryPoints:
  dot: {address: "127.0.0.1:%d"}
providers:
  file: {filename: /etc/traefik/dyn.yml}
`, trf))
	testkit.WriteFile(t, dir, "dyn.yml", fmt.Sprintf(`tcp:
  routers:
    dot: {entryPoints: [dot], rule: "HostSNI(`+"`*`"+`)", service: xffdns}
  services:
    xffdns:
      loadBalancer:
        serversTransport: pp2
        servers: [{address: %q}]
  serversTransports:
    pp2: {proxyProtocol: {version: 2}}
`, at("ppdot")))
	testkit.Run(t, "traefik", "-v", dir+"/traefik.yml:/etc/traefik/traefik.yml:ro", "-v", dir+"/dyn.yml:/etc/traefik/dyn.yml:ro",
		image("TRAEFIK", "traefik:v3"))
	testkit.WriteFile(t, dir, "dnsdist.conf", fmt.Sprintf(`setLocal("127.0.0.1:%d")
setACL({"0.0.0.0/0"})
newServer({address=%q, useProxyProtocol=true, healthCheckMode="up"})
`, dd, at("pp")))
	testkit.Run(t, "dnsdist", "-v", dir+"/dnsdist.conf:/etc/dnsdist/dnsdist.conf:ro", image("DNSDIST", "powerdns/dnsdist-20"))
	ngx := testkit.FreePort(t)
	testkit.WriteFile(t, dir, "nginx.conf", fmt.Sprintf(`events {}
stream {
  server {
    listen 127.0.0.1:%d;
    proxy_pass %s;
    proxy_protocol on;
  }
}
`, ngx, at("ppdot")))
	testkit.Run(t, "nginx", "-v", dir+"/nginx.conf:/etc/nginx/nginx.conf:ro", image("NGINX", "nginx:stable"))
	for _, p := range []int{hap1, hap2, trf, dd, ngx} {
		testkit.WaitTCP(t, fmt.Sprintf("127.0.0.1:%d", p))
	}
	hp := func(p int) string { return fmt.Sprintf("127.0.0.1:%d", p) }

	cases := []struct {
		name     string
		c        client
		clientID string
	}{
		{"udp", client{proto: "udp", addr: at("udp"), from: "127.0.0.77"}, ""},
		{"tcp", client{proto: "tcp", addr: at("tcp"), from: "127.0.0.78"}, ""},
		{"dot-sni", client{proto: "dot", addr: at("dot"), from: "127.0.0.79", sni: "kid." + serverName}, "kid"},
		{"haproxy-v1", client{proto: "tcp", addr: hp(hap1), from: "127.0.0.80"}, ""},
		{"haproxy-v2-dot", client{proto: "dot", addr: hp(hap2), from: "127.0.0.81", sni: "phone." + serverName}, "phone"},
		{"traefik-v2-dot", client{proto: "dot", addr: hp(trf), from: "127.0.0.82", sni: serverName}, ""},
		{"nginx-v1-dot", client{proto: "dot", addr: hp(ngx), from: "127.0.0.90", sni: "tv." + serverName}, "tv"},
		{"dnsdist-udp", client{proto: "udp", addr: hp(dd), from: "127.0.0.83"}, ""},
		{"dnsdist-tcp", client{proto: "tcp", addr: hp(dd), from: "127.0.0.84"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.pool = pool
			name := tc.name + ".example.com"
			if a := tc.c.ask(t, name); !blockedAnswer(a) {
				t.Fatalf("answer %x", a)
			}
			e := ag.FindLog(t, name)
			if e == nil {
				t.Fatal("not in the query log")
			}
			if e.Client != tc.c.from || e.ClientID != tc.clientID || e.ClientProto != "doh" {
				t.Fatalf("logged as client %s, client ID %q, %s; want %s, %q", e.Client, e.ClientID, e.ClientProto, tc.c.from, tc.clientID)
			}
		})
	}

	t.Run("blocked services by address", func(t *testing.T) {
		c := client{proto: "tcp", addr: hp(hap1), from: "127.0.0.85"}
		if a := c.ask(t, "www.ggpht.com"); !blockedAnswer(a) {
			t.Fatalf("youtube was not blocked for laptop: %x", a)
		}
		e := ag.FindLog(t, "www.ggpht.com")
		if e == nil || e.Reason != "FilteredBlockedService" || e.ClientInfo.Name != "laptop" {
			t.Fatalf("%+v", e)
		}
		c.from = "127.0.0.86"
		if a := c.ask(t, "www.ggpht.com"); blockedAnswer(a) {
			t.Fatal("youtube was blocked for another client")
		}
	})
	t.Run("blocked services by client ID", func(t *testing.T) {
		c := client{proto: "dot", addr: hp(trf), from: "127.0.0.89", sni: "kid." + serverName, pool: pool}
		if a := c.ask(t, "amemv.com"); !blockedAnswer(a) {
			t.Fatalf("tiktok was not blocked for kidtab: %x", a)
		}
		if e := ag.FindLog(t, "amemv.com"); e == nil || e.ClientInfo.Name != "kidtab" || e.Client != "127.0.0.89" {
			t.Fatalf("%+v", e)
		}
	})
	t.Run("access list", func(t *testing.T) {
		a := client{proto: "udp", addr: hp(dd), from: "127.0.0.87"}.ask(t, "refused.example.com")
		if a == nil || a[3]&0xF != 5 {
			t.Fatalf("a disallowed client was not refused: %x", a)
		}
	})
	t.Run("spoofed header", func(t *testing.T) {
		// From 127.0.0.88, not a trusted proxy, claiming to be 10.1.2.3.
		hdr := []byte("\r\n\r\n\x00\r\nQUIT\n\x21\x12\x00\x0c\x0a\x01\x02\x03\x7f\x00\x00\x01\x13\x88\x00\x35")
		conn, err := (&net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.88")}}).Dial("udp", at("pp"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write(append(hdr, query("spoofed.example.com")...))
		conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(make([]byte, 512)); err == nil {
			t.Fatal("answered")
		}
		if ag.FindLog(t, "spoofed.example.com") != nil {
			t.Fatal("a spoofed query reached AdGuard Home")
		}
		var b strings.Builder
		m.Write(&b)
		if !strings.Contains(b.String(), `xffdns_proxy_errors_total{listener="pp-udp",reason="untrusted_source"} 1`) {
			t.Fatal(b.String())
		}
	})
	t.Run("check", func(t *testing.T) {
		t.Setenv("XFFDNS_ADGUARD_PASSWORD", ag.Pass)
		var out, errb strings.Builder
		code := cli.Main([]string{"check", "--url", ag.Web + "/dns-query", "--name", "check.example.com", "--ip", "192.0.2.10",
			"--adguard-user", ag.User}, &out, &errb)
		if code != 0 || !strings.Contains(out.String(), "sees the address") {
			t.Fatalf("%d\n%s%s", code, out.String(), errb.String())
		}
	})
}

func image(kind, def string) string {
	if v := os.Getenv("XFFDNS_" + kind + "_IMAGE"); v != "" {
		return v
	}
	return def
}
