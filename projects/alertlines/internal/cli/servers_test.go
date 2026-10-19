package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/testkit"
)

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

type notification struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

// webhook records what Alertmanager sends.
func webhook(t *testing.T) (*httptest.Server, func() []notification) {
	var mu sync.Mutex
	var got []notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n notification
		json.NewDecoder(r.Body).Decode(&n)
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []notification { mu.Lock(); defer mu.Unlock(); return append([]notification(nil), got...) }
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

const rules = `groups:
  - name: shop
    rules:
      - alert: PaymentErrors
        expr: 'sum by (pod) (count_over_time({app="shop"} |= "ERROR" [1m])) > 0'
        labels: {severity: page}
        annotations: {summary: "Payment errors on {{ $labels.pod }}"}
`

// chain starts the real thing: webhook <- Alertmanager <- alertlines <- Loki's
// ruler. lokiForLines is the Loki alertlines reads (normally the same).
func chain(t *testing.T, lokiForLines func(real string) string) (*testkit.Loki, *testkit.Alertmanager, func() []notification, *syncBuffer) {
	hook, got := webhook(t)
	am := testkit.StartAlertmanager(t, hook.URL)
	port := testkit.FreePort(t)
	l := testkit.StartLoki(t, rules, fmt.Sprintf("http://127.0.0.1:%d", port))
	dir := t.TempDir()
	file := filepath.Join(dir, "alertlines.yaml")
	os.WriteFile(file, []byte(fmt.Sprintf(`listen: "127.0.0.1:%d"
alertmanagers: ["%s"]
loki: {url: "%s"}
timeout: 2s
lines: {max: 5, labels: [pod]}
`, port, am.URL, lokiForLines(l.URL))), 0o600)
	var stderr syncBuffer
	env := Env{Stdout: io.Discard, Stderr: &stderr, ReadFile: os.ReadFile, LookupEnv: func(string) (string, bool) { return "", false }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, env, []string{"run", "-c", file}) }()
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("run exited %d:\n%s", code, stderr.String())
		}
	})
	return l, am, got, &stderr
}

func TestRulerToAlertmanager(t *testing.T) {
	l, am, got, _ := chain(t, func(real string) string { return real })
	l.Push(t, map[string]string{"app": "shop", "pod": "shop-1"},
		"INFO order 17 paid", "ERROR payment 18 failed: card declined (token=tok_live_abc123)", "ERROR payment 19 failed: gateway timeout")
	l.Push(t, map[string]string{"app": "shop", "pod": "shop-2"}, "ERROR payment 20 failed: insufficient funds")

	var shop1 map[string]string
	eventually(t, "the notification with lines", 90*time.Second, func() bool {
		for _, n := range got() {
			for _, a := range n.Alerts {
				if a.Labels["pod"] == "shop-1" && a.Annotations["log_lines"] != "" {
					shop1 = a.Annotations
					return true
				}
			}
		}
		return false
	})
	lines := shop1["log_lines"]
	if !strings.Contains(lines, "pod=shop-1 ERROR payment 19 failed: gateway timeout") || !strings.Contains(lines, "(token=[redacted])") ||
		strings.Contains(lines, "tok_live") || strings.Contains(lines, "payment 20") || strings.Contains(lines, "order 17") ||
		shop1["log_lines_count"] != "2 lines" || shop1["summary"] != "Payment errors on shop-1" {
		t.Errorf("annotations %v", shop1)
	}
	// Alertmanager keeps them too.
	resp, err := http.Get(am.URL + "/api/v2/alerts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var alerts []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	json.NewDecoder(resp.Body).Decode(&alerts)
	found := false
	for _, a := range alerts {
		if a.Labels["pod"] == "shop-2" {
			found = strings.Contains(a.Annotations["log_lines"], "payment 20 failed")
		}
	}
	if !found {
		t.Errorf("Alertmanager's alert for shop-2 lacks its line: %v", alerts)
	}
}

func TestLokiDownStillAlerts(t *testing.T) {
	dead := fmt.Sprintf("http://127.0.0.1:%d", testkit.FreePort(t))
	l, _, got, stderr := chain(t, func(string) string { return dead })
	l.Push(t, map[string]string{"app": "shop", "pod": "shop-1"}, "ERROR payment 18 failed")
	eventually(t, "the notification", 90*time.Second, func() bool {
		for _, n := range got() {
			for _, a := range n.Alerts {
				if a.Labels["alertname"] == "PaymentErrors" {
					if a.Annotations["log_lines"] != "" {
						t.Errorf("lines from a dead Loki: %v", a.Annotations)
					}
					return true
				}
			}
		}
		return false
	})
	if !strings.Contains(stderr.String(), "cannot add log lines to PaymentErrors") {
		t.Errorf("not logged:\n%s", stderr.String())
	}
}
