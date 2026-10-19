// Package enrich adds the matching log lines to alerts.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ganeshphutane-g/alertlines/internal/logql"
	"github.com/ganeshphutane-g/alertlines/internal/loki"
	"github.com/ganeshphutane-g/alertlines/internal/redact"
)

// Options say how much to add, and where.
type Options struct {
	MaxLines     int
	MaxLineBytes int
	MaxBytes     int      // for the whole annotation
	ShowLabels   []string // stream labels to show with each line
	Annotation   string   // the annotation's name; its count goes in <name>_count
	DefaultRange time.Duration
	CacheFor     time.Duration
}

// QueryAnnotation, on a rule, gives the log query to use.
const QueryAnnotation = "alertlines_query"

// Alert is what enrich needs of an alert.
type Alert struct {
	Labels       map[string]string
	Annotations  map[string]string
	GeneratorURL string
	EndsAt       time.Time
}

// Lines is what reads log lines (a *loki.Client, or a fake in tests).
type Lines interface {
	Lines(ctx context.Context, query string, start, end time.Time, limit int, tenant string) ([]loki.Line, error)
}

// Enricher adds log lines to alerts.
type Enricher struct {
	Loki   Lines
	Redact *redact.Redactor
	Opt    Options
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at   time.Time
	adds map[string]string
}

// Plan is the query for an alert.
type Plan struct {
	Query      string
	Start, End time.Time
	// Loose is a query without the alert's label filters, tried when the
	// filtered one finds nothing (the labels may not be log labels).
	Loose string
}

// Skip reasons.
var (
	ErrResolved = errors.New("resolved")
	ErrNoQuery  = errors.New("no log query found (no " + QueryAnnotation + " annotation, and generatorURL has no Loki expression)")
)

// PlanFor works out the query for an alert.
func (e *Enricher) PlanFor(a Alert) (*Plan, error) {
	now := e.now()
	if !a.EndsAt.IsZero() && !a.EndsAt.After(now) {
		return nil, ErrResolved
	}
	rng := e.Opt.DefaultRange
	if rng <= 0 {
		rng = 5 * time.Minute
	}
	var parsed *logql.Query
	if expr := Expression(a.GeneratorURL); expr != "" {
		if q, err := logql.Extract(expr); err == nil {
			parsed = q
		}
	}
	var log string
	if q := strings.TrimSpace(a.Annotations[QueryAnnotation]); q != "" {
		log = q
		if parsed != nil {
			rng = parsed.Range
		}
	} else if parsed != nil {
		log, rng = parsed.Log, parsed.Range
	} else {
		return nil, ErrNoQuery
	}
	p := &Plan{Query: log, Start: now.Add(-rng), End: now}
	// The alert's own series: its labels, as filters.
	filters := map[string]string{}
	switch {
	case parsed != nil && len(parsed.By) > 0:
		for _, k := range parsed.By {
			if v, ok := a.Labels[k]; ok {
				filters[k] = v
			}
		}
	case parsed != nil && !parsed.Grouped:
		// Every label of the series, but the rule's own labels are mixed
		// in: try with them, then without.
		for k, v := range a.Labels {
			if k != "alertname" {
				filters[k] = v
			}
		}
		p.Loose = log
	}
	if len(filters) > 0 {
		p.Query = logql.WithLabels(log, filters)
	} else {
		p.Loose = ""
	}
	return p, nil
}

// Expression is the query in a ruler's generatorURL: Loki 3's Explore link
// (left={"queries":[{"expr":...}]}) or the Prometheus-style g0.expr.
func Expression(generatorURL string) string {
	u, err := url.Parse(generatorURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	if e := q.Get("g0.expr"); e != "" {
		return e
	}
	for _, key := range []string{"left", "panes"} {
		v := q.Get(key)
		if v == "" {
			continue
		}
		var pane struct {
			Queries []struct {
				Expr string `json:"expr"`
			} `json:"queries"`
		}
		if json.Unmarshal([]byte(v), &pane) == nil && len(pane.Queries) > 0 && pane.Queries[0].Expr != "" {
			return pane.Queries[0].Expr
		}
		// panes={"a":{"queries":[...]}}
		var panes map[string]json.RawMessage
		if json.Unmarshal([]byte(v), &panes) == nil {
			for _, raw := range panes {
				if json.Unmarshal(raw, &pane) == nil && len(pane.Queries) > 0 && pane.Queries[0].Expr != "" {
					return pane.Queries[0].Expr
				}
			}
		}
	}
	return ""
}

// Enrich returns the annotations to add to an alert.
func (e *Enricher) Enrich(ctx context.Context, a Alert, tenant string) (map[string]string, error) {
	p, err := e.PlanFor(a)
	if err != nil {
		return nil, err
	}
	key := tenant + "\x00" + p.Query
	if adds, ok := e.fromCache(key); ok {
		return adds, nil
	}
	max := max(e.Opt.MaxLines, 1)
	lines, err := e.Loki.Lines(ctx, p.Query, p.Start, p.End, max+1, tenant)
	if err == nil && len(lines) == 0 && p.Loose != "" {
		lines, err = e.Loki.Lines(ctx, p.Loose, p.Start, p.End, max+1, tenant)
	}
	if err != nil {
		return nil, err
	}
	adds := e.format(lines, max)
	e.toCache(key, adds)
	return adds, nil
}

func (e *Enricher) format(lines []loki.Line, max int) map[string]string {
	name := e.Opt.Annotation
	if name == "" {
		name = "log_lines"
	}
	more := len(lines) > max
	if more {
		lines = lines[:max]
	}
	lineBytes := e.Opt.MaxLineBytes
	if lineBytes <= 0 {
		lineBytes = 300
	}
	total := e.Opt.MaxBytes
	if total <= 0 {
		total = 4000
	}
	var b strings.Builder
	shown := 0
	for _, l := range lines {
		text := strings.Join(strings.Fields(l.Text), " ")
		if e.Redact != nil {
			text = e.Redact.Line(text)
		}
		var prefix strings.Builder
		prefix.WriteString(l.Time.UTC().Format("2006-01-02T15:04:05Z"))
		for _, k := range e.Opt.ShowLabels {
			if v, ok := l.Labels[k]; ok {
				fmt.Fprintf(&prefix, " %s=%s", k, v)
			}
		}
		line := prefix.String() + " " + cut(text, lineBytes)
		if b.Len()+len(line)+1 > total {
			more = true
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		shown++
	}
	count := fmt.Sprintf("%d lines", shown)
	switch {
	case shown == 1 && !more:
		count = "1 line"
	case shown == 1:
		count = "the latest line (there are more)"
	case more:
		count = fmt.Sprintf("the latest %d lines (there are more)", shown)
	}
	return map[string]string{name: b.String(), name + "_count": count}
}

// cut shortens s to at most n bytes, on a character boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:max(n, 0)] + "…"
}

func (e *Enricher) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Enricher) fromCache(key string) (map[string]string, bool) {
	if e.Opt.CacheFor <= 0 {
		return nil, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.cache[key]
	if !ok || e.now().Sub(c.at) > e.Opt.CacheFor {
		return nil, false
	}
	return c.adds, true
}

func (e *Enricher) toCache(key string, adds map[string]string) {
	if e.Opt.CacheFor <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cache == nil {
		e.cache = map[string]cached{}
	}
	now := e.now()
	// Drop what has run out, so the cache stays small.
	for k, c := range e.cache {
		if now.Sub(c.at) > e.Opt.CacheFor {
			delete(e.cache, k)
		}
	}
	e.cache[key] = cached{at: now, adds: adds}
}
