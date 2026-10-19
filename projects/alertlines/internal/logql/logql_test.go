package logql

import (
	"strings"
	"testing"
	"time"
)

func TestExtract(t *testing.T) {
	for _, c := range []struct {
		expr, log string
		rng       time.Duration
		by        string
		grouped   bool
	}{
		// As Loki 3.7's ruler puts it in generatorURL.
		{`(sum by (app)(count_over_time({app="shop"} |= "ERROR"[5m])) > 0)`, `{app="shop"} |= "ERROR"`, 5 * time.Minute, "app", true},
		{`count_over_time({job="api"}[1h]) > 10`, `{job="api"}`, time.Hour, "", false},
		{`sum(rate({app="web"} | json | status >= 500 [2m])) by (pod, namespace) > 1`, `{app="web"} | json | status >= 500`, 2 * time.Minute, "namespace,pod", true},
		// A "[" and "(" inside strings are text.
		{`sum(count_over_time({app="x"} |= "[ERROR] (db)" |~ ` + "`a[0-9]+`" + ` [10m])) > 0`, `{app="x"} |= "[ERROR] (db)" |~ ` + "`a[0-9]+`", 10 * time.Minute, "", true},
		// unwrap is for metrics only.
		{`avg_over_time({app="api"} | logfmt | unwrap duration_ms | __error__="" [5m]) > 500`, `{app="api"} | logfmt | __error__=""`, 5 * time.Minute, "", false},
		{`quantile_over_time(0.99, {app="api"} | json | unwrap latency [1h30m]) by (route) > 2`, `{app="api"} | json`, 90 * time.Minute, "route", false},
		// The first of two (the numerator of an error ratio).
		{`sum(rate({app="a"} |= "error" [5m])) / sum(rate({app="a"}[5m])) > 0.05`, `{app="a"} |= "error"`, 5 * time.Minute, "", true},
		{`count_over_time(({app="x"} |= "y")[1d]) > 0`, `{app="x"} |= "y"`, 24 * time.Hour, "", false},
		{`absent_over_time({app="cron"}[1w])`, `{app="cron"}`, 7 * 24 * time.Hour, "", false},
	} {
		q, err := Extract(c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if q.Log != c.log || q.Range != c.rng || strings.Join(q.By, ",") != c.by || q.Grouped != c.grouped {
			t.Errorf("%s:\n got %q %s %v %v\nwant %q %s %v %v", c.expr, q.Log, q.Range, q.By, q.Grouped, c.log, c.rng, c.by, c.grouped)
		}
	}
	for _, bad := range []string{`vector(1)`, `sum(up)`, `count_over_time({app="x"} |= "unclosed [5m])`, `rate({app="x"}[5x])`, ``} {
		if q, err := Extract(bad); err == nil {
			t.Errorf("%s: got %+v", bad, q)
		}
	}
}

func TestWithLabels(t *testing.T) {
	got := WithLabels(`{app="web"} |= "x"`, map[string]string{"pod": `api-"7"`, "namespace": "prod", "bad-name": "x", "__name__": "y"})
	if got != `{app="web"} |= "x" | namespace="prod" | pod="api-\"7\""` {
		t.Fatalf("%s", got)
	}
}
