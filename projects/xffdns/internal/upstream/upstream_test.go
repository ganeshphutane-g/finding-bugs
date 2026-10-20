package upstream

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var query = []byte{0xAB, 0xCD, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}

type seen struct {
	mu                         sync.Mutex
	method, path, xff, ct, acc string
	id                         uint16
}

// fake is AdGuard Home's DoH endpoint: it answers with the query, QR set.
func fake(s *seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.method, s.path, s.xff, s.ct, s.acc = r.Method, r.URL.Path, r.Header.Get("X-Forwarded-For"), r.Header.Get("Content-Type"), r.Header.Get("Accept")
		s.id = binary.BigEndian.Uint16(b)
		s.mu.Unlock()
		b[2] |= 0x80
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(b)
	}
}

func TestExchange(t *testing.T) {
	var s seen
	srv := httptest.NewServer(fake(&s))
	defer srv.Close()
	c, err := New(Options{URL: srv.URL + "/dns-query/", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ans, err := c.Exchange(context.Background(), query, netip.MustParseAddr("::ffff:192.0.2.10"), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.method != "POST" || s.path != "/dns-query" || s.xff != "192.0.2.10" || s.ct != contentType || s.acc != contentType || s.id != 0 {
		t.Fatalf("%+v", &s)
	}
	if binary.BigEndian.Uint16(ans) != 0xABCD || ans[2]&0x80 == 0 {
		t.Fatalf("%x", ans)
	}
	if query[0] != 0xAB {
		t.Fatal("the query was changed in place")
	}
	if _, err := c.Exchange(context.Background(), query, netip.MustParseAddr("fe80::1%eth0"), "kid"); err != nil {
		t.Fatal(err)
	}
	if s.path != "/dns-query/kid" || s.xff != "fe80::1" {
		t.Fatalf("%+v", &s)
	}
}

func TestErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusBadRequest) })
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(query)
	})
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write([]byte{1, 2})
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body) // so the server notices the client leaving
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) { t.Error("a redirect was followed") })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for path, want := range map[string]error{"/moved": ErrStatus, "/status": ErrStatus, "/html": ErrAnswer, "/short": ErrAnswer, "/slow": ErrTimeout} {
		c, _ := New(Options{URL: srv.URL + path, Timeout: 200 * time.Millisecond})
		if _, err := c.Exchange(context.Background(), query, netip.MustParseAddr("192.0.2.1"), ""); !errors.Is(err, want) {
			t.Errorf("%s: %v", path, err)
		}
	}
	c, _ := New(Options{URL: srv.URL})
	if _, err := c.Exchange(context.Background(), query[:5], netip.MustParseAddr("192.0.2.1"), ""); err == nil {
		t.Fatal("a short query was sent")
	}
}

func TestHTTPS(t *testing.T) {
	var s seen
	srv := httptest.NewTLSServer(fake(&s))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	// httptest's certificate is for example.com and 127.0.0.1.
	c, err := New(Options{URL: srv.URL + "/dns-query", CAFile: ca, ServerName: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exchange(context.Background(), query, netip.MustParseAddr("2001:db8::5"), ""); err != nil {
		t.Fatal(err)
	}
	if s.xff != "2001:db8::5" {
		t.Fatal(s.xff)
	}
	// Without the CA, the certificate is not trusted.
	c, _ = New(Options{URL: srv.URL + "/dns-query"})
	if _, err := c.Exchange(context.Background(), query, netip.MustParseAddr("192.0.2.1"), ""); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if _, err := New(Options{URL: srv.URL, CAFile: "/nonexistent"}); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, []byte("nothing"), 0o600)
	if _, err := New(Options{URL: srv.URL, CAFile: empty}); err == nil {
		t.Fatal("a CA file with no certificates was accepted")
	}
}

func TestNoProxyFromEnvironment(t *testing.T) {
	var s seen
	srv := httptest.NewServer(fake(&s))
	defer srv.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	c, _ := New(Options{URL: srv.URL})
	if _, err := c.Exchange(context.Background(), query, netip.MustParseAddr("192.0.2.1"), ""); err != nil {
		t.Fatal("went through the environment's proxy:", err)
	}
}
