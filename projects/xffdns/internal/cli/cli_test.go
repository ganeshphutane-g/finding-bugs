package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/server"
)

// fakeAdGuard serves DoH and a query log. It logs each query as from the
// X-Forwarded-For address when trust is set, else as from the connection.
type fakeAdGuard struct {
	mu        sync.Mutex
	trust     bool
	anonymize bool
	log       []map[string]any
}

func (f *fakeAdGuard) handler() http.Handler {
	mux := http.NewServeMux()
	doh := func(w http.ResponseWriter, r *http.Request) {
		q, _ := io.ReadAll(r.Body)
		name := ""
		for i := 12; i < len(q) && q[i] != 0; i += int(q[i]) + 1 {
			name += string(q[i+1:i+1+int(q[i])]) + "."
		}
		client, _, _ := net.SplitHostPort(r.RemoteAddr)
		f.mu.Lock()
		if f.trust {
			client = r.Header.Get("X-Forwarded-For")
		}
		if f.anonymize {
			client = anonymized(netip.MustParseAddr(client)).String()
		}
		f.log = append([]map[string]any{{"client": client, "client_id": r.PathValue("id"), "time": time.Now().Format(time.RFC3339Nano),
			"question": map[string]string{"name": strings.TrimSuffix(name, ".")}}}, f.log...)
		f.mu.Unlock()
		q[2] |= 0x80
		q[3] |= 3 // NXDOMAIN
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(q)
	}
	mux.HandleFunc("POST /dns-query", doh)
	mux.HandleFunc("POST /dns-query/{id}", doh)
	mux.HandleFunc("GET /control/querylog", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"data": f.log})
	})
	return mux
}

func runCheck(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	var out, errb strings.Builder
	code := check(args, &out, &errb, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	return code, out.String() + errb.String()
}

func TestCheck(t *testing.T) {
	f := &fakeAdGuard{trust: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	pw := map[string]string{"XFFDNS_ADGUARD_PASSWORD": "pw"}
	doh := srv.URL + "/dns-query"

	code, out := runCheck(t, nil, "--url", doh)
	if code != 0 || !strings.Contains(out, "answer  NXDOMAIN, 0 records") || !strings.Contains(out, "--adguard-user") {
		t.Fatalf("%d\n%s", code, out)
	}
	code, out = runCheck(t, pw, "--url", doh, "--adguard-user", "admin", "--ip", "2001:db8::7", "--client-id", "kid")
	if code != 0 || !strings.Contains(out, `client 2001:db8::7, client ID "kid"`) || !strings.Contains(out, "sees the address") {
		t.Fatalf("%d\n%s", code, out)
	}
	f.trust = false
	code, out = runCheck(t, pw, "--url", doh, "--adguard-user", "admin")
	if code != exitIgnored || !strings.Contains(out, "logged the query as from 127.0.0.1, not 192.0.2.10") || !strings.Contains(out, "trusted_proxies") {
		t.Fatalf("%d\n%s", code, out)
	}
	code, out = runCheck(t, nil, "--url", doh, "--adguard-user", "admin")
	if code != exitError || !strings.Contains(out, "XFFDNS_ADGUARD_PASSWORD") {
		t.Fatalf("%d\n%s", code, out)
	}
	// From a file.
	dir := t.TempDir()
	conf := filepath.Join(dir, "x.yaml")
	os.WriteFile(conf, []byte(fmt.Sprintf("upstream: {url: %q}\nlisteners: [{protocol: udp, address: ':53'}]\n", doh)), 0o600)
	if code, out := runCheck(t, nil, "-c", conf); code != 0 {
		t.Fatalf("%d\n%s", code, out)
	}
	// AdGuard Home with plain DoH off answers 404.
	code, out = runCheck(t, nil, "--url", srv.URL+"/nothing")
	if code != exitError || !strings.Contains(out, "insecure_enabled") {
		t.Fatalf("%d\n%s", code, out)
	}
	// The same name again: the new entry is found, not the old one.
	f.trust = true
	code, out = runCheck(t, pw, "--url", doh, "--adguard-user", "admin", "--name", "again.example.com")
	f.trust = false
	code2, out2 := runCheck(t, pw, "--url", doh, "--adguard-user", "admin", "--name", "again.example.com")
	if code != 0 || code2 != exitIgnored {
		t.Fatalf("%d %d\n%s\n%s", code, code2, out, out2)
	}
	// AdGuard Home with anonymize_client_ip logs 192.168.0.0 for 192.168.5.77.
	f.trust, f.anonymize = true, true
	code, out = runCheck(t, pw, "--url", doh, "--adguard-user", "admin", "--ip", "192.168.5.77")
	f.anonymize = false
	if code != 0 || !strings.Contains(out, "anonymizes") {
		t.Fatalf("%d\n%s", code, out)
	}
	if code, _ := runCheck(t, nil, "-h"); code != 0 {
		t.Fatal("-h:", code)
	}
	for _, args := range [][]string{{"--ip", "nope"}, {"--ip", "fe80::1%eth0"}, {"--ip", "127.0.0.1"}, {"--ip", "::1"},
		{"--name", "a..b"}, {"--name", strings.Repeat("a", 64) + ".com"}, {"-c", conf, "--ca-file", "x"},
		{"--client-id", "Kid"}, {"--url", doh, "-c", conf}, {"--url", "ftp://x"}, {"--url", srv.URL}, {"extra"}} {
		if code, out := runCheck(t, nil, args...); code != exitUsage {
			t.Errorf("%v: %d\n%s", args, code, out)
		}
	}
}

func TestMain(t *testing.T) {
	var out, errb strings.Builder
	if Main(nil, &out, &errb) != exitUsage || Main([]string{"bogus"}, &out, &errb) != exitUsage {
		t.Fatal("usage")
	}
	out.Reset()
	if Main([]string{"version"}, &out, &errb) != 0 || !strings.HasPrefix(out.String(), "xffdns ") {
		t.Fatal(out.String())
	}
	if Main([]string{"help"}, &out, &errb) != 0 {
		t.Fatal("help")
	}
	if code := Main([]string{"run", "-c", "/nonexistent.yaml"}, &out, &errb); code != exitError {
		t.Fatal(code)
	}
}

func TestServe(t *testing.T) {
	f := &fakeAdGuard{trust: true}
	ag := httptest.NewServer(f.handler())
	defer ag.Close()
	dir := t.TempDir()
	conf := filepath.Join(dir, "x.yaml")
	os.WriteFile(conf, []byte(fmt.Sprintf(`upstream: {url: %q}
listeners: [{name: u, protocol: udp, address: "127.0.0.1:0", client_id: lan}]
metrics_listen: "127.0.0.1:0"
`, ag.URL+"/dns-query")), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan *server.Server, 1)
	done := make(chan int)
	var logs strings.Builder
	logw := &syncWriter{w: &logs}
	go func() {
		done <- serve(ctx, []string{"-c", conf}, logw, func(s *server.Server) { ready <- s })
	}()
	var s *server.Server
	select {
	case s = <-ready:
	case code := <-done:
		t.Fatalf("exited %d: %s", code, logs.String())
	}
	c, _ := net.Dial("udp", s.Addr("u").String())
	defer c.Close()
	q := makeQuery("served.example")
	binary.BigEndian.PutUint16(q, 77)
	c.Write(q)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || binary.BigEndian.Uint16(buf) != 77 || buf[3]&0xF != 3 || n < 12 {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.log[0]
	f.mu.Unlock()
	if got["client"] != "127.0.0.1" || got["client_id"] != "lan" {
		t.Fatal(got)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(snapshot(&logs, logw), "AdGuard Home answers") {
			break
		}
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"listening", "metrics", "xffdns started", "AdGuard Home answers", "stopping"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no %q in the log:\n%s", want, logs.String())
		}
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestServeProbeFails(t *testing.T) {
	// Plain-HTTP DoH off: AdGuard Home answers 404.
	ag := httptest.NewServer(http.NotFoundHandler())
	defer ag.Close()
	conf := filepath.Join(t.TempDir(), "x.yaml")
	os.WriteFile(conf, []byte(fmt.Sprintf("upstream: {url: %q}\nlisteners: [{protocol: udp, address: \"127.0.0.1:0\"}]\n", ag.URL+"/dns-query")), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	var logs strings.Builder
	w := &syncWriter{w: &logs}
	done := make(chan int)
	go func() { done <- serve(ctx, []string{"-c", conf}, w, nil) }()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		w.mu.Lock()
		found := strings.Contains(logs.String(), "insecure_enabled")
		w.mu.Unlock()
		if found {
			break
		}
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "did not answer a test query") || !strings.Contains(logs.String(), "insecure_enabled") {
		t.Fatal(logs.String())
	}
}

func snapshot(b *strings.Builder, w *syncWriter) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return b.String()
}
