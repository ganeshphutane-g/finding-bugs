package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/enrich"
	"github.com/ganeshphutane-g/alertlines/internal/loki"
)

type fakeLoki struct {
	delay time.Duration
	err   error
}

func (f fakeLoki) Lines(ctx context.Context, q string, start, end time.Time, limit int, tenant string) ([]loki.Line, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.delay):
	}
	if f.err != nil {
		return nil, f.err
	}
	return []loki.Line{{Time: time.Date(2026, 10, 19, 19, 0, 0, 0, time.UTC), Text: "ERROR " + tenant + " " + q}}, nil
}

// fakeAM records batches.
type fakeAM struct {
	mu      sync.Mutex
	batches [][]map[string]any
	tenants []string
	auth    []string
	status  int
	*httptest.Server
}

func newAM(t *testing.T, status int) *fakeAM {
	am := &fakeAM{status: status}
	am.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			io.WriteString(w, "passed "+r.URL.Path)
			return
		}
		am.mu.Lock()
		am.tenants = append(am.tenants, r.Header.Get("X-Scope-OrgID"))
		am.auth = append(am.auth, r.Header.Get("Authorization"))
		am.mu.Unlock()
		var b []map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		am.mu.Lock()
		am.batches = append(am.batches, b)
		am.mu.Unlock()
		w.WriteHeader(am.status)
	}))
	t.Cleanup(am.Close)
	return am
}

func (am *fakeAM) last() []map[string]any {
	am.mu.Lock()
	defer am.mu.Unlock()
	if len(am.batches) == 0 {
		return nil
	}
	return am.batches[len(am.batches)-1]
}

func batch(endsAt time.Time) string {
	gen := "/explore?left=" + url.QueryEscape(`{"queries":[{"expr":"sum by (app) (count_over_time({app=\"shop\"} |= \"ERROR\" [5m])) > 0"}]}`)
	alerts := []map[string]any{
		{"labels": map[string]string{"alertname": "Errors", "app": "shop"}, "annotations": map[string]string{"summary": "s"},
			"generatorURL": gen, "startsAt": "2026-10-19T18:55:00Z", "endsAt": endsAt.Format(time.RFC3339), "extra": map[string]int{"kept": 1}},
		{"labels": map[string]string{"alertname": "Other"}, "generatorURL": "/alerting/grafana/x/view", "endsAt": endsAt.Format(time.RFC3339)},
	}
	b, _ := json.Marshal(alerts)
	return string(b)
}

func server(l enrich.Lines, timeout time.Duration, ams ...*fakeAM) *Server {
	s := &Server{Enricher: &enrich.Enricher{Loki: l, Opt: enrich.Options{MaxLines: 5}}, Client: http.DefaultClient, Timeout: timeout}
	for _, am := range ams {
		s.Alertmanagers = append(s.Alertmanagers, am.URL)
	}
	return s
}

func post(t *testing.T, s *Server, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/alerts", strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAddsLinesAndForwards(t *testing.T) {
	am1, am2 := newAM(t, 200), newAM(t, 200)
	s := server(fakeLoki{}, time.Second, am1, am2)
	rec := post(t, s, batch(time.Now().Add(time.Hour)), "X-Scope-OrgID", "team-a")
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	for _, am := range []*fakeAM{am1, am2} {
		b := am.last()
		if len(b) != 2 {
			t.Fatalf("batch %v", b)
		}
		ann := b[0]["annotations"].(map[string]any)
		if ann["summary"] != "s" || !strings.Contains(ann["log_lines"].(string), `ERROR team-a {app="shop"} |= "ERROR" | app="shop"`) || ann["log_lines_count"] != "1 line" {
			t.Errorf("annotations %v", ann)
		}
		if b[0]["extra"].(map[string]any)["kept"] != 1.0 {
			t.Errorf("an unknown field was lost: %v", b[0])
		}
		if _, has := b[1]["annotations"]; has {
			t.Errorf("an alert without a log query was changed: %v", b[1])
		}
	}
	var m bytes.Buffer
	s.writeMetrics(&m)
	for _, want := range []string{"alertlines_alerts_received_total 2", "alertlines_alerts_enriched_total 1", `alertlines_alerts_skipped_total{reason="grafana_managed"} 1`, `alertlines_forwards_total{result="ok"} 2`} {
		if !strings.Contains(m.String(), want) {
			t.Errorf("metrics lack %s:\n%s", want, m.String())
		}
	}
}

func TestNeverInTheWay(t *testing.T) {
	am := newAM(t, 200)
	// Loki too slow: the batch goes on in time, without lines.
	s := server(fakeLoki{delay: 5 * time.Second}, 200*time.Millisecond, am)
	start := time.Now()
	if rec := post(t, s, batch(time.Now().Add(time.Hour))); rec.Code != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("slow Loki: %d after %s", rec.Code, time.Since(start))
	}
	if _, has := am.last()[0]["annotations"].(map[string]any)["log_lines"]; has {
		t.Error("lines from a timed-out query")
	}
	// Loki failing.
	s = server(fakeLoki{err: io.ErrUnexpectedEOF}, time.Second, am)
	var logged []string
	s.Log = func(f string, a ...any) { logged = append(logged, f) }
	post(t, s, batch(time.Now().Add(time.Hour)))
	post(t, s, batch(time.Now().Add(time.Hour)))
	// (Each problem once: Loki's error, and the Grafana-managed alert.)
	if len(am.batches) != 3 || len(logged) != 2 {
		t.Errorf("batches %d, logged %v", len(am.batches), logged)
	}
	// Resolved alerts and unreadable bodies pass untouched.
	body := batch(time.Now().Add(-time.Minute))
	post(t, s, body)
	if _, has := am.last()[0]["annotations"].(map[string]any)["log_lines"]; has {
		t.Error("a resolved alert got lines")
	}
	post(t, s, "not json")
	if len(am.batches) != 5 {
		t.Errorf("unreadable body not passed on: %d", len(am.batches))
	}
}

func TestAlertmanagerAnswers(t *testing.T) {
	down, up := newAM(t, 500), newAM(t, 200)
	s := server(fakeLoki{}, time.Second, down, up)
	if rec := post(t, s, batch(time.Now().Add(time.Hour))); rec.Code != 200 {
		t.Errorf("one of two took it: %d", rec.Code)
	}
	s = server(fakeLoki{}, time.Second, down)
	if rec := post(t, s, batch(time.Now().Add(time.Hour))); rec.Code != 500 {
		t.Errorf("none took it: %d", rec.Code)
	}
	gone := newAM(t, 200)
	gone.Close()
	s = server(fakeLoki{}, time.Second, gone)
	if rec := post(t, s, batch(time.Now().Add(time.Hour))); rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable: %d", rec.Code)
	}
	// Nothing else is passed on.
	s = server(fakeLoki{}, time.Second, up)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/alerts", nil))
	if rec.Code != 404 {
		t.Errorf("GET alerts: %d %q", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Body.String() != "ok\n" {
		t.Errorf("healthz: %q", rec.Body)
	}
}

func TestAuthTenantsAndPaths(t *testing.T) {
	am := newAM(t, 200)
	s := server(fakeLoki{}, time.Second, am)
	s.Username, s.Password = "ruler", "pw"
	if rec := post(t, s, batch(time.Now().Add(time.Hour))); rec.Code != 401 {
		t.Fatalf("no auth: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/prefix/api/v2/alerts", strings.NewReader(batch(time.Now().Add(time.Hour))))
	req.SetBasicAuth("ruler", "pw")
	req.Header.Set("X-Scope-OrgID", "team-a")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || am.tenants[0] != "team-a" || am.auth[0] != "" {
		t.Fatalf("auth, prefix: %d tenants %v auth %v", rec.Code, am.tenants, am.auth)
	}
	if _, has := am.last()[0]["annotations"].(map[string]any)["log_lines"]; !has {
		t.Error("a prefixed path got no lines")
	}
	// Only listed tenants get lines; the alerts still pass.
	s = server(fakeLoki{}, time.Second, am)
	s.Tenants = []string{"team-a"}
	post(t, s, batch(time.Now().Add(time.Hour)), "X-Scope-OrgID", "team-b")
	if _, has := am.last()[0]["annotations"].(map[string]any)["log_lines"]; has {
		t.Error("an unlisted tenant got lines")
	}
	s.BearerToken = "t0ken"
	if rec := post(t, s, batch(time.Now().Add(time.Hour)), "Authorization", "Bearer wrong"); rec.Code != 401 {
		t.Errorf("bearer: %d", rec.Code)
	}
}

func TestBatchCap(t *testing.T) {
	am := newAM(t, 200)
	s := server(fakeLoki{}, 5*time.Second, am)
	var alerts []string
	for range maxAlerts + 50 {
		alerts = append(alerts, `{"labels":{"alertname":"A"},"annotations":{"alertlines_query":"{a=\"b\"}"},"endsAt":"2099-01-01T00:00:00Z"}`)
	}
	if rec := post(t, s, "["+strings.Join(alerts, ",")+"]"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	b := am.last()
	with := 0
	for _, a := range b {
		if _, has := a["annotations"].(map[string]any)["log_lines"]; has {
			with++
		}
	}
	if len(b) != maxAlerts+50 || with != maxAlerts {
		t.Errorf("%d alerts, %d with lines", len(b), with)
	}
}
