package check

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

func run(t *testing.T, c config.Check) Result {
	t.Helper()
	ch, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ch.Run(ctx)
}

func expect(t *testing.T, r Result, ok bool, msg string) {
	t.Helper()
	if r.OK != ok || !strings.Contains(r.Message, msg) {
		t.Errorf("got ok=%v %q, want ok=%v containing %q", r.OK, r.Message, ok, msg)
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "Welcome to Synology")
		case "/down":
			w.WriteHeader(503)
		case "/moved":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/auth":
			if r.Header.Get("Authorization") != "Bearer s3cret" || r.Host != "nas.lan" {
				w.WriteHeader(401)
			}
		case "/slow":
			time.Sleep(2 * time.Second)
		}
	}))
	defer srv.Close()
	no := false
	for _, c := range []struct {
		h   config.HTTP
		ok  bool
		msg string
	}{
		{config.HTTP{URL: srv.URL + "/ok", Contains: "Synology"}, true, "status 200"},
		{config.HTTP{URL: srv.URL + "/ok", Contains: "Grafana"}, false, `does not contain "Grafana"`},
		{config.HTTP{URL: srv.URL + "/down"}, false, "status 503, expected 200-399"},
		{config.HTTP{URL: srv.URL + "/down", ExpectStatus: []int{503}}, true, "status 503"},
		{config.HTTP{URL: srv.URL + "/moved"}, true, "status 200"},
		{config.HTTP{URL: srv.URL + "/moved", FollowRedirects: &no}, true, "status 302"},
		{config.HTTP{URL: srv.URL + "/moved", FollowRedirects: &no, ExpectStatus: []int{200, 204}}, false, "expected 200, 204"},
		{config.HTTP{URL: srv.URL + "/auth", Headers: map[string]string{"Authorization": "Bearer s3cret", "Host": "nas.lan"}}, true, "status 200"},
		{config.HTTP{URL: srv.URL + "/auth"}, false, "status 401"},
	} {
		c.h.Method = "GET"
		expect(t, run(t, config.Check{HTTP: &c.h}), c.ok, c.msg)
	}
	// Refused and timed out.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	expect(t, run(t, config.Check{HTTP: &config.HTTP{URL: "http://" + addr, Method: "GET"}}), false, "connection refused")
	ch, _ := New(config.Check{HTTP: &config.HTTP{URL: srv.URL + "/slow", Method: "GET"}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	expect(t, ch.Run(ctx), false, "timed out")
}

func TestHTTPSCertificates(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	expect(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, Method: "GET"}}), false, "certificate")
	expect(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, Method: "GET", Insecure: true}}), true, "status 200")
}

func TestTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	expect(t, run(t, config.Check{TCP: &config.TCP{Address: addr}}), true, "connected")
	l.Close()
	expect(t, run(t, config.Check{TCP: &config.TCP{Address: addr}}), false, "connection refused")
}

// dnsServer answers A, AAAA and TXT questions from a table, over UDP.
func dnsServer(t *testing.T, records map[string][]string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			// Header (12 bytes), then the question: labels, type, class.
			i, name := 12, ""
			for i < len(q) && q[i] != 0 {
				l := int(q[i])
				name += string(q[i+1:i+1+l]) + "."
				i += 1 + l
			}
			qtype := binary.BigEndian.Uint16(q[i+1:])
			question := q[12 : i+5]
			key := fmt.Sprintf("%s %d", strings.ToLower(name), qtype)
			answers := records[key]
			resp := append([]byte{}, q[0:2]...) // ID
			flags := uint16(0x8180)             // response, recursion available
			if _, known := records[key]; !known {
				flags |= 3 // NXDOMAIN
			}
			resp = binary.BigEndian.AppendUint16(resp, flags)
			resp = binary.BigEndian.AppendUint16(resp, 1)
			resp = binary.BigEndian.AppendUint16(resp, uint16(len(answers)))
			resp = append(resp, 0, 0, 0, 0)
			resp = append(resp, question...)
			for _, a := range answers {
				resp = append(resp, 0xc0, 12) // the name, by pointer
				resp = binary.BigEndian.AppendUint16(resp, qtype)
				resp = append(resp, 0, 1, 0, 0, 0, 60)
				var data []byte
				switch qtype {
				case 1, 28:
					ip := net.ParseIP(a)
					if qtype == 1 {
						data = ip.To4()
					} else {
						data = ip.To16()
					}
				case 16:
					data = append([]byte{byte(len(a))}, a...)
				}
				resp = binary.BigEndian.AppendUint16(resp, uint16(len(data)))
				resp = append(resp, data...)
			}
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	server := dnsServer(t, map[string][]string{
		"nas.lan. 1":    {"192.168.1.10"},
		"multi.lan. 1":  {"10.0.0.2", "10.0.0.1"},
		"nas.lan. 28":   {"fd00::10"},
		"site.lan. 16":  {"v=spf1 -all"},
		"empty.lan. 28": {},
	})
	for _, c := range []struct {
		d   config.DNS
		ok  bool
		msg string
	}{
		{config.DNS{Query: "nas.lan", Type: "A"}, true, "nas.lan is 192.168.1.10"},
		{config.DNS{Query: "nas.lan", Type: "A", Expect: []string{"192.168.1.10"}}, true, "192.168.1.10"},
		{config.DNS{Query: "nas.lan", Type: "A", Expect: []string{"192.168.1.11"}}, false, "nas.lan is 192.168.1.10, expected 192.168.1.11"},
		{config.DNS{Query: "multi.lan", Type: "A", Expect: []string{"10.0.0.1", "10.0.0.2"}}, true, "10.0.0.1, 10.0.0.2"},
		{config.DNS{Query: "multi.lan", Type: "A", Expect: []string{"10.0.0.1"}}, false, "expected 10.0.0.1"},
		{config.DNS{Query: "nas.lan", Type: "AAAA"}, true, "fd00::10"},
		{config.DNS{Query: "site.lan", Type: "TXT"}, true, "v=spf1 -all"},
		{config.DNS{Query: "gone.lan", Type: "A"}, false, "no such name"},
	} {
		c.d.Server = server
		expect(t, run(t, config.Check{DNS: &c.d}), c.ok, c.msg)
	}
}

// certServer serves TLS with a certificate for 127.0.0.1 that expires at
// notAfter.
func certServer(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func TestTLS(t *testing.T) {
	days := func(n int) *int { return &n }
	soon := certServer(t, time.Now().Add(5*24*time.Hour+time.Hour))
	later := certServer(t, time.Now().Add(60*24*time.Hour+time.Hour))
	expired := certServer(t, time.Now().Add(-time.Hour))
	expect(t, run(t, config.Check{TLS: &config.TLS{Address: later, MinDaysLeft: days(14)}}), false, "not trusted")
	expect(t, run(t, config.Check{TLS: &config.TLS{Address: later, MinDaysLeft: days(14), Insecure: true}}), true, "expires in 60 days")
	expect(t, run(t, config.Check{TLS: &config.TLS{Address: soon, MinDaysLeft: days(14), Insecure: true}}), false, "expires in 5 days")
	expect(t, run(t, config.Check{TLS: &config.TLS{Address: expired, MinDaysLeft: days(14), Insecure: true}}), false, "expired on")
}

func TestDisk(t *testing.T) {
	expect(t, run(t, config.Check{Disk: &config.Disk{Path: "/", MaxUsedPercent: 100}}), true, "% used")
	expect(t, run(t, config.Check{Disk: &config.Disk{Path: "/", MaxUsedPercent: 0.00001}}), false, "more than")
	expect(t, run(t, config.Check{Disk: &config.Disk{Path: "/no/such/place", MaxUsedPercent: 90}}), false, "no such file")
}

func TestMemory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "meminfo")
	os.WriteFile(f, []byte("MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:    2000000 kB\n"), 0o644)
	meminfo = f
	defer func() { meminfo = "/proc/meminfo" }()
	expect(t, run(t, config.Check{Memory: &config.Memory{MaxUsedPercent: 90}}), true, "memory 75.0% used (1.9 GiB free of 7.6 GiB)")
	expect(t, run(t, config.Check{Memory: &config.Memory{MaxUsedPercent: 70}}), false, "more than 70%")
}

func TestShort(t *testing.T) {
	if got := short("a\nb   c"); got != "a b c" {
		t.Errorf("%q", got)
	}
	if got := short(strings.Repeat("é", 300)); len([]rune(got)) != MaxMessage {
		t.Errorf("%d", len([]rune(got)))
	}
}

func TestPing(t *testing.T) {
	r := run(t, config.Check{Ping: &config.Ping{Host: "127.0.0.1", Count: 2}})
	if strings.Contains(r.Message, "not allowed to send pings") {
		t.Skip("pings are not allowed here:", r.Message)
	}
	expect(t, r, true, "127.0.0.1: 2/2 replies")
	if r.Duration <= 0 || r.Duration > time.Second {
		t.Errorf("round trip %s", r.Duration)
	}
	expect(t, run(t, config.Check{Ping: &config.Ping{Host: "no-such-host.invalid", Count: 1}}), false, "cannot look up")
	// TEST-NET-3 never answers (this sandbox answers 192.0.2.1 itself).
	ch, _ := New(config.Check{Ping: &config.Ping{Host: "203.0.113.77", Count: 1}})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if r := ch.Run(ctx); r.OK {
		t.Errorf("203.0.113.77 answered: %s", r.Message)
	} else if !strings.Contains(r.Message, "no reply") {
		t.Errorf("%s", r.Message)
	}
}

func TestExec(t *testing.T) {
	t.Setenv("PUSHPROBE_KUMA_PASSWORD", "secret")
	expect(t, run(t, config.Check{Exec: &config.Exec{Command: []string{"sh", "-c", "echo backup is 2 hours old; echo more"}}}), true, "backup is 2 hours old")
	expect(t, run(t, config.Check{Exec: &config.Exec{Command: []string{"sh", "-c", "echo stale >&2; exit 2"}}}), false, "exit code 2: stale")
	expect(t, run(t, config.Check{Exec: &config.Exec{Command: []string{"sh", "-c", "echo ${PUSHPROBE_KUMA_PASSWORD:-none}"}}}), true, "none")
	expect(t, run(t, config.Check{Exec: &config.Exec{Command: []string{"/no/such/program"}}}), false, "no such file")
	ch, _ := New(config.Check{Exec: &config.Exec{Command: []string{"sleep", "10"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if r := ch.Run(ctx); r.OK || r.Message != "timed out" || time.Since(start) > 3*time.Second {
		t.Errorf("%+v after %s", r, time.Since(start))
	}
}
