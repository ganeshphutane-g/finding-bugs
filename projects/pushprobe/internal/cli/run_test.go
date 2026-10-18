package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/runner"
)

// syncBuffer is a bytes.Buffer safe for the agent's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestRunAgent(t *testing.T) {
	runner.MaxStartDelay = time.Millisecond
	var mu sync.Mutex
	pushes := map[string][]string{}
	kuma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		token := strings.TrimPrefix(r.URL.Path, "/api/push/")
		pushes[token] = append(pushes[token], r.URL.Query().Get("status")+" "+r.URL.Query().Get("msg"))
		io.WriteString(w, `{"ok":true}`)
	}))
	defer kuma.Close()
	got := func(token string) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pushes[token]...)
	}

	target, _ := net.Listen("tcp", "127.0.0.1:0")
	defer target.Close()
	listen := freePort(t)
	file := write(t, `
listen: "`+listen+`"
defaults: {interval: 5s, timeout: 1s, retries: 0}
targets: {kuma: {url: "`+kuma.URL+`"}}
checks:
  - {name: printer, tcp: {address: "`+target.Addr().String()+`"}, kuma: {push_token: "${KUMA_PRINTER}"}}
`)
	var stderr syncBuffer
	reload := make(chan struct{}, 1)
	env := Env{Stdout: io.Discard, Stderr: &stderr, ReadFile: os.ReadFile, Reload: reload,
		LookupEnv: func(k string) (string, bool) {
			return map[string]string{"KUMA_PRINTER": "tok1"}[k], k == "KUMA_PRINTER"
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, env, []string{"run", "-c", file}) }()

	eventually(t, "the first push", 5*time.Second, func() bool { return len(got("tok1")) == 1 })
	if p := got("tok1")[0]; p != "up connected" {
		t.Errorf("push %q", p)
	}
	resp, err := http.Get("http://" + listen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`pushprobe_check_up{check="printer"} 1`, `pushprobe_check_runs_total{check="printer"} 1`,
		`pushprobe_deliveries_total{check="printer",target="Uptime Kuma",outcome="delivered"} 1`} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics lack %s:\n%s", want, metrics)
		}
	}

	// A broken file keeps the old configuration.
	good, _ := os.ReadFile(file)
	os.WriteFile(file, []byte("checks: [{name: x}]\n"), 0o600)
	reload <- struct{}{}
	eventually(t, "the failed reload", 5*time.Second, func() bool { return strings.Contains(stderr.String(), "reload failed") })

	// A new check after a good reload.
	os.WriteFile(file, append(good, []byte(`  - {name: web, tcp: {address: "`+freePort(t)+`"}, kuma: {push_token: tok2}}`+"\n")...), 0o600)
	reload <- struct{}{}
	eventually(t, "the new check", 5*time.Second, func() bool { return len(got("tok2")) == 1 })
	if p := got("tok2")[0]; !strings.HasPrefix(p, "down ") || !strings.Contains(p, "connection refused") {
		t.Errorf("push %q", p)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	logs := stderr.String()
	for _, want := range []string{"started: 1 check, 1 reporting to Uptime Kuma, 0 to Gatus", "printer: up (connected)",
		"reloaded: 2 checks", "web: DOWN (connection refused)", "stopped"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "tok1") || strings.Contains(logs, "tok2") {
		t.Errorf("a token is in the log:\n%s", logs)
	}
}
