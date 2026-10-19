package enrich

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/loki"
	"github.com/ganeshphutane-g/alertlines/internal/redact"
)

var now = time.Date(2026, 10, 19, 19, 0, 0, 0, time.UTC)

// fakeLoki answers queries from a table and records them.
type fakeLoki struct {
	answers map[string][]loki.Line
	asked   []string
	tenants []string
}

func (f *fakeLoki) Lines(_ context.Context, q string, start, end time.Time, limit int, tenant string) ([]loki.Line, error) {
	f.asked = append(f.asked, q+" @"+end.Sub(start).String())
	f.tenants = append(f.tenants, tenant)
	l := f.answers[q]
	if len(l) > limit {
		l = l[:limit]
	}
	return l, nil
}

func explore(expr string) string {
	return "/explore?left=" + url.QueryEscape(`{"queries":[{"expr":`+strings.ReplaceAll(jsonString(expr), "", "")+`,"queryType":"range"}]}`)
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func lines(texts ...string) []loki.Line {
	var out []loki.Line
	for i, t := range texts {
		out = append(out, loki.Line{Time: now.Add(-time.Duration(i) * time.Second), Labels: map[string]string{"pod": "api-1"}, Text: t})
	}
	return out
}

func TestExpression(t *testing.T) {
	expr := `(sum by (app)(count_over_time({app="shop"} |= "ERROR"[5m])) > 0)`
	if got := Expression(explore(expr)); got != expr {
		t.Errorf("explore: %q", got)
	}
	if got := Expression("http://loki:3100/graph?g0.expr=" + url.QueryEscape(expr) + "&g0.tab=1"); got != expr {
		t.Errorf("g0.expr: %q", got)
	}
	panes := "/explore?panes=" + url.QueryEscape(`{"x1":{"queries":[{"expr":"rate({a=\"b\"}[1m])"}]}}`)
	if got := Expression(panes); got != `rate({a="b"}[1m])` {
		t.Errorf("panes: %q", got)
	}
	if Expression("/alerting/grafana/abc/view") != "" || Expression("%zz") != "" {
		t.Error("an expression out of nothing")
	}
}

func TestEnrich(t *testing.T) {
	r, _ := redact.New(true, nil)
	f := &fakeLoki{answers: map[string][]loki.Line{
		`{app="shop"} |= "ERROR" | app="shop"`: lines("ERROR payment 19 failed: gateway timeout", "ERROR payment 18 failed (token=tok_live_abc123)"),
	}}
	e := &Enricher{Loki: f, Redact: r, Opt: Options{MaxLines: 10, ShowLabels: []string{"pod"}}, Now: func() time.Time { return now }}
	a := Alert{Labels: map[string]string{"alertname": "PaymentErrors", "app": "shop", "severity": "page"},
		GeneratorURL: explore(`(sum by (app)(count_over_time({app="shop"} |= "ERROR"[5m])) > 0)`), EndsAt: now.Add(4 * time.Minute)}
	adds, err := e.Enrich(context.Background(), a, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-10-19T19:00:00Z pod=api-1 ERROR payment 19 failed: gateway timeout\n2026-10-19T18:59:59Z pod=api-1 ERROR payment 18 failed (token=[redacted])"
	if adds["log_lines"] != want || adds["log_lines_count"] != "2 lines" {
		t.Errorf("%q\n%q", adds["log_lines"], adds["log_lines_count"])
	}
	if f.asked[0] != `{app="shop"} |= "ERROR" | app="shop" @5m0s` || f.tenants[0] != "team-a" {
		t.Errorf("asked %v as %v", f.asked, f.tenants)
	}
	// Resolved alerts are left alone.
	a.EndsAt = now.Add(-time.Second)
	if _, err := e.Enrich(context.Background(), a, ""); err != ErrResolved {
		t.Errorf("resolved: %v", err)
	}
}

func TestEnrichChoices(t *testing.T) {
	f := &fakeLoki{answers: map[string][]loki.Line{
		`{job="cron"} |= "failed"`:           lines("backup failed"),
		`{app="api"} | json | level="error"`: lines("a", "b", "c", "d"),
		`{app="web"} | pod="web-2"`:          lines("from web-2"),
	}}
	e := &Enricher{Loki: f, Opt: Options{MaxLines: 3, DefaultRange: 15 * time.Minute}, Now: func() time.Time { return now }}

	// No grouping: the series' labels mixed with the rule's (severity),
	// which no stream has; the lines are matched afterwards.
	adds, err := e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "Cron", "job": "cron", "severity": "page"},
		GeneratorURL: explore(`count_over_time({job="cron"} |= "failed"[1h]) > 0`)}, "")
	if err != nil || adds["log_lines_count"] != "1 line" || !strings.HasSuffix(adds["log_lines"], " backup failed") {
		t.Errorf("loose: %v %v (asked %v)", adds, err, f.asked)
	}
	if strings.Join(f.asked, ";") != `{job="cron"} |= "failed" @1h0m0s` {
		t.Errorf("asked %q", f.asked)
	}
	// Grouped without by: no filters. More lines than allowed.
	adds, _ = e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "E"},
		GeneratorURL: explore(`sum(count_over_time({app="api"} | json | level="error" [5m])) > 3`)}, "")
	if adds["log_lines_count"] != "the latest 3 lines (there are more)" || strings.Count(adds["log_lines"], "\n") != 2 {
		t.Errorf("limit: %v", adds)
	}
	// The annotation wins, with the default range when generatorURL says
	// nothing; grouping labels are not known then.
	f.asked = nil
	e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "W", "pod": "web-2"},
		Annotations: map[string]string{QueryAnnotation: `{app="web"} | pod="web-2"`}}, "")
	if f.asked[0] != `{app="web"} | pod="web-2" @15m0s` {
		t.Errorf("annotation: %v", f.asked)
	}
	// Nothing to go on.
	if _, err := e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "X"}, GeneratorURL: "/alerting/grafana/x/view"}, ""); err != ErrGrafana {
		t.Errorf("grafana: %v", err)
	}
	if _, err := e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "X"}, GeneratorURL: explore(`up == 0`)}, ""); err != ErrNoQuery {
		t.Errorf("no query: %v", err)
	}
	if _, err := e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "X"}, GeneratorURL: explore(`rate({a="b"}[5mins])`)}, ""); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("bad range: %v", err)
	}
}

func TestLimitsAndCache(t *testing.T) {
	long := strings.Repeat("é", 400)
	f := &fakeLoki{answers: map[string][]loki.Line{`{a="b"}`: lines(long, "second", "third")}}
	e := &Enricher{Loki: f, Opt: Options{MaxLines: 10, MaxLineBytes: 50, MaxBytes: 110, CacheFor: time.Minute}, Now: func() time.Time { return now }}
	a := Alert{Labels: map[string]string{"alertname": "A"}, GeneratorURL: explore(`sum(rate({a="b"}[1m])) > 0`)}
	adds, _ := e.Enrich(context.Background(), a, "")
	first := strings.Split(adds["log_lines"], "\n")[0]
	if len(first) > 50+len("2026-10-19T19:00:00Z ") || !strings.HasSuffix(first, "…") {
		t.Errorf("line not cut: %d %q", len(first), first)
	}
	if len(adds["log_lines"]) > 110 || adds["log_lines_count"] != "the latest 2 lines (there are more)" {
		t.Errorf("total: %d %q", len(adds["log_lines"]), adds["log_lines_count"])
	}
	one := &Enricher{Loki: f, Opt: Options{MaxLines: 1}, Now: e.Now}
	if adds, _ := one.Enrich(context.Background(), a, ""); adds["log_lines_count"] != "the latest line (there are more)" {
		t.Errorf("one: %q", adds["log_lines_count"])
	}
	f.asked = f.asked[:1]
	e.Enrich(context.Background(), a, "")
	if len(f.asked) != 1 {
		t.Errorf("the cache did not answer: %v", f.asked)
	}
	e.Now = func() time.Time { return now.Add(2 * time.Minute) }
	e.Enrich(context.Background(), a, "")
	if len(f.asked) != 2 {
		t.Errorf("the cache did not expire: %v", f.asked)
	}
}

// streams answers with lines of two pods, a and b, for any query.
type streams struct{ asked []string }

func (s *streams) Lines(_ context.Context, q string, start, end time.Time, limit int, tenant string) ([]loki.Line, error) {
	s.asked = append(s.asked, fmt.Sprintf("%s %s..%s", q, start.Format("15:04"), end.Format("15:04")))
	return []loki.Line{
		{Time: now, Labels: map[string]string{"app": "shop", "pod": "b", "env": "prod"}, Text: "from b"},
		{Time: now.Add(-time.Second), Labels: map[string]string{"app": "shop", "pod": "a", "env": "dev"}, Text: "from a"},
	}, nil
}

func TestReviewFixes(t *testing.T) {
	s := &streams{}
	e := &Enricher{Loki: s, Opt: Options{MaxLines: 5, MaxRange: time.Hour}, Now: func() time.Time { return now }}
	run := func(labels map[string]string, expr string) string {
		t.Helper()
		adds, err := e.Enrich(context.Background(), Alert{Labels: labels, GeneratorURL: explore(expr)}, "")
		if err != nil {
			t.Fatal(err)
		}
		return adds["log_lines"]
	}
	// One pod's alert shows that pod's lines, though severity is no log label.
	if got := run(map[string]string{"alertname": "E", "pod": "a", "severity": "page"}, `count_over_time({app="shop"} |= "ERROR"[5m]) > 0`); !strings.HasSuffix(got, "from a") || strings.Contains(got, "from b") {
		t.Errorf("per stream: %q", got)
	}
	// without (pod): env still names the series.
	if got := run(map[string]string{"alertname": "E", "env": "prod"}, `sum without (pod) (count_over_time({app="shop"}[5m])) > 0`); !strings.HasSuffix(got, "from b") || strings.Contains(got, "from a") {
		t.Errorf("without: %q", got)
	}
	// offset moves the window; the range is capped.
	s.asked = nil
	run(map[string]string{"alertname": "O"}, `sum(count_over_time({app="shop"}[5m] offset 1h0m0s)) > 0`)
	run(map[string]string{"alertname": "O"}, `sum(count_over_time({app="shop"}[30d])) > 0`)
	if s.asked[0] != `{app="shop"} 17:55..18:00` || s.asked[1] != `{app="shop"} 18:00..19:00` {
		t.Errorf("windows: %v", s.asked)
	}
	// label_replace's label is no log label: no filter, matched after.
	s.asked = nil
	got := run(map[string]string{"alertname": "R", "svc": "a"}, `sum by (svc) (label_replace(count_over_time({app="shop"}[5m]),"svc","$1","pod","(.*)")) > 0`)
	if s.asked[0] != `{app="shop"} 18:55..19:00` || !strings.Contains(got, "from a") || !strings.Contains(got, "from b") {
		t.Errorf("label_replace: %v %q", s.asked, got)
	}
}

func TestCacheKnowsTheWindow(t *testing.T) {
	f := &fakeLoki{answers: map[string][]loki.Line{`{a="b"}`: lines("x")}}
	e := &Enricher{Loki: f, Opt: Options{MaxLines: 3, CacheFor: time.Minute}, Now: func() time.Time { return now }}
	for _, expr := range []string{`sum(rate({a="b"}[1h])) > 0`, `sum(rate({a="b"}[5m])) > 0`, `sum(rate({a="b"}[5m])) > 0`} {
		e.Enrich(context.Background(), Alert{Labels: map[string]string{"alertname": "A"}, GeneratorURL: explore(expr)}, "")
	}
	if len(f.asked) != 2 {
		t.Errorf("asked %v", f.asked)
	}
}
