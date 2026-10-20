// Package metrics keeps xffdns' counters and writes them in Prometheus'
// text format.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Results of a query, for the queries counter.
const (
	Answered     = "answered"
	ServFail     = "servfail"
	Truncated    = "truncated"
	RateLimited  = "rate_limited"
	Busy         = "busy"
	Malformed    = "malformed"
	RefusedProxy = "refused_proxy"
)

// escape makes a label value safe in the text format.
var escape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// latency buckets, in seconds.
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Metrics are all of them. The zero value is not usable; use New.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]map[string]*atomic.Uint64 // name -> labels -> value
	help     map[string]string
	conns    atomic.Int64
	hist     []atomic.Uint64 // per bucket, then +Inf
	sumMicro atomic.Uint64
}

// New makes the metrics.
func New() *Metrics {
	return &Metrics{
		counters: map[string]map[string]*atomic.Uint64{},
		help: map[string]string{
			"xffdns_queries_total":         "Queries taken, by listener and result.",
			"xffdns_upstream_errors_total": "Queries AdGuard Home did not answer, by kind of error.",
			"xffdns_proxy_errors_total":    "Connections or datagrams dropped for their PROXY header, by listener and reason.",
			"xffdns_conns_refused_total":   "TCP and DoT connections closed at once, by listener and reason.",
			"xffdns_cert_reloads_total":    "Certificate reloads, by listener and result.",
		},
		hist: make([]atomic.Uint64, len(buckets)+1),
	}
}

func (m *Metrics) inc(name string, labels ...string) {
	if m == nil {
		return
	}
	var b strings.Builder
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", labels[i], escape.Replace(labels[i+1]))
	}
	key := b.String()
	m.mu.Lock()
	vec := m.counters[name]
	if vec == nil {
		vec = map[string]*atomic.Uint64{}
		m.counters[name] = vec
	}
	c := vec[key]
	if c == nil {
		c = new(atomic.Uint64)
		vec[key] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

// Query counts a query on listener with result.
func (m *Metrics) Query(listener, result string) {
	m.inc("xffdns_queries_total", "listener", listener, "result", result)
}

// UpstreamError counts a failed exchange.
func (m *Metrics) UpstreamError(kind string) { m.inc("xffdns_upstream_errors_total", "kind", kind) }

// ProxyError counts a bad or unwanted PROXY header.
func (m *Metrics) ProxyError(listener, reason string) {
	m.inc("xffdns_proxy_errors_total", "listener", listener, "reason", reason)
}

// ConnRefused counts a connection closed at once.
func (m *Metrics) ConnRefused(listener, reason string) {
	m.inc("xffdns_conns_refused_total", "listener", listener, "reason", reason)
}

// CertReload counts a certificate reload.
func (m *Metrics) CertReload(listener string, ok bool) {
	r := "ok"
	if !ok {
		r = "error"
	}
	m.inc("xffdns_cert_reloads_total", "listener", listener, "result", r)
}

// Conn tracks open connections: +1 on open, -1 on close.
func (m *Metrics) Conn(delta int64) {
	if m != nil {
		m.conns.Add(delta)
	}
}

// Upstream records how long an exchange took.
func (m *Metrics) Upstream(d time.Duration) {
	if m == nil {
		return
	}
	s := d.Seconds()
	i := sort.SearchFloat64s(buckets, s)
	m.hist[i].Add(1)
	m.sumMicro.Add(uint64(d.Microseconds()))
}

// Write writes every metric.
func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", n, m.help[n], n)
		keys := make([]string, 0, len(m.counters[n]))
		for k := range m.counters[n] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s} %d\n", n, k, m.counters[n][k].Load())
		}
	}
	m.mu.Unlock()
	fmt.Fprintf(w, "# HELP xffdns_open_conns TCP and DoT connections open now.\n# TYPE xffdns_open_conns gauge\nxffdns_open_conns %d\n", m.conns.Load())
	fmt.Fprintf(w, "# HELP xffdns_upstream_seconds Time AdGuard Home took to answer.\n# TYPE xffdns_upstream_seconds histogram\n")
	var total uint64
	for i, b := range buckets {
		total += m.hist[i].Load()
		fmt.Fprintf(w, "xffdns_upstream_seconds_bucket{le=\"%g\"} %d\n", b, total)
	}
	total += m.hist[len(buckets)].Load()
	fmt.Fprintf(w, "xffdns_upstream_seconds_bucket{le=\"+Inf\"} %d\n", total)
	fmt.Fprintf(w, "xffdns_upstream_seconds_sum %g\nxffdns_upstream_seconds_count %d\n", float64(m.sumMicro.Load())/1e6, total)
}

// Handler serves /metrics and /healthz.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.Write(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return mux
}
