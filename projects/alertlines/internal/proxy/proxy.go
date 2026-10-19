// Package proxy sits where the ruler sends alerts, adds log lines to them
// and passes them on to Alertmanager.
package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/enrich"
)

// Server is the proxy.
type Server struct {
	Enricher *enrich.Enricher
	// Alertmanagers get every batch; other requests go to the first.
	Alertmanagers []string
	Client        *http.Client
	// Timeout bounds the wait for Loki per batch.
	Timeout time.Duration
	// Tenant, if set, replaces the ruler's X-Scope-OrgID for Loki.
	Tenant string
	// Tenants, if set, are the only ones whose alerts get lines.
	Tenants []string
	// Username and Password, or BearerToken, are asked of every request.
	Username, Password, BearerToken string
	Log                             func(format string, a ...any)

	stats   stats
	logOnce sync.Map // messages logged recently
}

type stats struct {
	mu        sync.Mutex
	received  int64
	enriched  int64
	skipped   map[string]int64
	forwarded map[string]int64 // by result: ok, error
	lokiTime  time.Duration
	lokiCalls int64
}

// Limits: the largest batch read, and the most alerts given lines in one
// (Loki's ruler sends at most 64).
const (
	maxBody   = 4 << 20
	maxAlerts = 1000
	workers   = 8
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		fmt.Fprintln(w, "ok")
		return
	case !s.authorized(r):
		w.Header().Set("WWW-Authenticate", `Basic realm="alertlines"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.writeMetrics(w)
	// (A ruler may be set up with a path prefix, as for Mimir.)
	case strings.HasSuffix(r.URL.Path, "/api/v2/alerts") && r.Method == http.MethodPost:
		s.alerts(w, r)
	default:
		// Nothing else is passed on: alertlines is not a way into
		// Alertmanager's API.
		http.Error(w, "alertlines takes only POST /api/v2/alerts (from Loki's ruler)", http.StatusNotFound)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	eq := func(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
	switch {
	case s.BearerToken != "":
		return eq(r.Header.Get("Authorization"), "Bearer "+s.BearerToken)
	case s.Username != "":
		u, p, ok := r.BasicAuth()
		return ok && eq(u, s.Username) && eq(p, s.Password)
	}
	return true
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "reading the alerts: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "too many alerts in one request", http.StatusRequestEntityTooLarge)
		return
	}
	tenant := s.Tenant
	if tenant == "" {
		tenant = r.Header.Get("X-Scope-OrgID")
	}
	out := body
	if s.allowed(tenant) {
		out = s.enrichBatch(r.Context(), body, tenant)
	} else {
		s.skip("tenant", 1)
		s.logLimited("alerts from tenant %q pass on without lines: it is not in tenants", tenant)
	}
	s.forward(w, r, out)
}

// enrichBatch adds lines to the batch's firing alerts, within the timeout.
// Anything it cannot read goes on unchanged.
func (s *Server) enrichBatch(ctx context.Context, body []byte, tenant string) []byte {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		s.skip("unreadable", 1)
		return body
	}
	s.stats.mu.Lock()
	s.stats.received += int64(len(raw))
	s.stats.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	results := make([]result, len(raw))
	n := len(raw)
	if n > maxAlerts {
		s.skip("batch_too_large", int64(n-maxAlerts))
		for i := maxAlerts; i < n; i++ {
			results[i].err = errTooMany
		}
		n = maxAlerts
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = s.enrichOne(ctx, raw[i], tenant)
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	changed := false
	for i, res := range results {
		switch {
		case errors.Is(res.err, errTooMany):
			continue
		case res.err != nil:
			r := reason(res.err)
			s.skip(r, 1)
			if r != "resolved" {
				s.logLimited("cannot add log lines to %s: %v", alertName(raw[i]), res.err)
			}
			continue
		case res.adds == nil:
			continue
		}
		var ann map[string]string
		json.Unmarshal(raw[i]["annotations"], &ann)
		if ann == nil {
			ann = map[string]string{}
		}
		for k, v := range res.adds {
			ann[k] = v
		}
		b, _ := json.Marshal(ann)
		raw[i]["annotations"] = b
		changed = true
		s.stats.mu.Lock()
		s.stats.enriched++
		s.stats.mu.Unlock()
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

var errTooMany = errors.New("too many alerts in one batch")

type result struct {
	adds map[string]string
	err  error
}

func (s *Server) enrichOne(ctx context.Context, a map[string]json.RawMessage, tenant string) result {
	if ctx.Err() != nil {
		return result{err: ctx.Err()}
	}
	var alert struct {
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generatorURL"`
		EndsAt       time.Time         `json:"endsAt"`
	}
	b, _ := json.Marshal(a)
	if err := json.Unmarshal(b, &alert); err != nil {
		return result{err: errors.New("unreadable")}
	}
	start := time.Now()
	adds, err := s.Enricher.Enrich(ctx, enrich.Alert{Labels: alert.Labels, Annotations: alert.Annotations,
		GeneratorURL: alert.GeneratorURL, EndsAt: alert.EndsAt}, tenant)
	if err == nil || reason(err) == "loki_error" || reason(err) == "timeout" {
		s.stats.mu.Lock()
		s.stats.lokiTime += time.Since(start)
		s.stats.lokiCalls++
		s.stats.mu.Unlock()
	}
	return result{adds, err}
}

func (s *Server) allowed(tenant string) bool {
	if len(s.Tenants) == 0 {
		return true
	}
	for _, t := range s.Tenants {
		if t == tenant {
			return true
		}
	}
	return false
}

func reason(err error) string {
	switch {
	case errors.Is(err, enrich.ErrResolved):
		return "resolved"
	case errors.Is(err, enrich.ErrNoQuery):
		return "no_query"
	case errors.Is(err, enrich.ErrGrafana):
		return "grafana_managed"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "timeout"
	case err.Error() == "unreadable":
		return "unreadable"
	}
	return "loki_error"
}

func alertName(a map[string]json.RawMessage) string {
	var labels map[string]string
	json.Unmarshal(a["labels"], &labels)
	if n := labels["alertname"]; n != "" {
		return n
	}
	return "an alert"
}

// forward sends the batch to every Alertmanager, and answers with the first
// success (or the last failure).
func (s *Server) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	type answer struct {
		status int
		body   []byte
		err    error
	}
	answers := make([]answer, len(s.Alertmanagers))
	var wg sync.WaitGroup
	for i, am := range s.Alertmanagers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(am, "/")+"/api/v2/alerts", bytes.NewReader(body))
			if err != nil {
				answers[i].err = err
				return
			}
			req.Header.Set("Content-Type", "application/json")
			// The tenant, for a multi-tenant Alertmanager (Mimir, Cortex). The
			// ruler's Authorization is for alertlines; an Alertmanager's own
			// credentials go in its URL.
			if t := r.Header.Get("X-Scope-OrgID"); t != "" {
				req.Header.Set("X-Scope-OrgID", t)
			}
			resp, err := s.Client.Do(req)
			if err != nil {
				answers[i].err = err
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			answers[i] = answer{status: resp.StatusCode, body: b}
		}()
	}
	wg.Wait()
	var best *answer
	for i := range answers {
		a := &answers[i]
		ok := a.err == nil && a.status >= 200 && a.status < 300
		s.stats.mu.Lock()
		if s.stats.forwarded == nil {
			s.stats.forwarded = map[string]int64{}
		}
		if ok {
			s.stats.forwarded["ok"]++
		} else {
			s.stats.forwarded["error"]++
		}
		s.stats.mu.Unlock()
		if !ok {
			msg := fmt.Sprintf("status %d", a.status)
			if a.err != nil {
				msg = a.err.Error()
			}
			s.logLimited("cannot pass alerts on to %s: %s", redacted(s.Alertmanagers[i]), msg)
		}
		// The first that took them, else the first answer.
		if best == nil || ok && !(best.err == nil && best.status < 300) {
			best = a
		}
	}
	if best.err != nil {
		http.Error(w, "no Alertmanager took the alerts: "+best.err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(best.status)
	w.Write(best.body)
}

func redacted(u string) string {
	if p, err := url.Parse(u); err == nil && p.User != nil {
		return p.Redacted()
	}
	return u
}

func (s *Server) skip(why string, n int64) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	if s.stats.skipped == nil {
		s.stats.skipped = map[string]int64{}
	}
	s.stats.skipped[why] += n
}

// logLimited logs a message at most once a minute.
func (s *Server) logLimited(f string, a ...any) {
	if s.Log == nil {
		return
	}
	msg := fmt.Sprintf(f, a...)
	if last, ok := s.logOnce.Load(msg); ok && time.Since(last.(time.Time)) < time.Minute {
		return
	}
	s.logOnce.Store(msg, time.Now())
	s.Log("%s", msg)
}

func (s *Server) writeMetrics(w io.Writer) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	fmt.Fprintf(w, "# HELP alertlines_alerts_received_total Alerts received from the ruler.\n# TYPE alertlines_alerts_received_total counter\nalertlines_alerts_received_total %d\n", s.stats.received)
	fmt.Fprintf(w, "# HELP alertlines_alerts_enriched_total Alerts given log lines.\n# TYPE alertlines_alerts_enriched_total counter\nalertlines_alerts_enriched_total %d\n", s.stats.enriched)
	fmt.Fprintf(w, "# HELP alertlines_alerts_skipped_total Alerts passed on without lines, by reason.\n# TYPE alertlines_alerts_skipped_total counter\n")
	var reasons []string
	for r := range s.stats.skipped {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(w, "alertlines_alerts_skipped_total{reason=%q} %d\n", r, s.stats.skipped[r])
	}
	fmt.Fprintf(w, "# HELP alertlines_forwards_total Batches sent on to an Alertmanager, by result.\n# TYPE alertlines_forwards_total counter\n")
	for _, r := range []string{"ok", "error"} {
		fmt.Fprintf(w, "alertlines_forwards_total{result=%q} %d\n", r, s.stats.forwarded[r])
	}
	fmt.Fprintf(w, "# HELP alertlines_loki_seconds_total Time spent asking Loki.\n# TYPE alertlines_loki_seconds_total counter\nalertlines_loki_seconds_total %g\n", s.stats.lokiTime.Seconds())
	fmt.Fprintf(w, "# HELP alertlines_loki_queries_total Questions asked of Loki (or answered from the cache).\n# TYPE alertlines_loki_queries_total counter\nalertlines_loki_queries_total %d\n", s.stats.lokiCalls)
}
