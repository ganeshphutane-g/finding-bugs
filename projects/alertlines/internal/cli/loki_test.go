package cli

import (
	"context"
	"testing"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/logql"
	"github.com/ganeshphutane-g/alertlines/internal/loki"
	"github.com/ganeshphutane-g/alertlines/internal/testkit"
)

// TestQueriesLokiAccepts runs the log queries cut from real rule
// expressions against a real Loki: each must be a valid log query.
func TestQueriesLokiAccepts(t *testing.T) {
	l := testkit.StartLoki(t, "", "http://127.0.0.1:1")
	l.Push(t, map[string]string{"app": "api", "pod": "api-1"}, `{"level":"error","status":503,"latency":1.5,"route":"/pay"}`, "level=error duration_ms=900")
	time.Sleep(time.Second)
	c := loki.New(l.URL, false)
	for _, expr := range []string{
		`(sum by (app)(count_over_time({app="api"} |= "error"[5m])) > 0)`,
		`sum(rate({app="api"} | json | status >= 500 [2m])) by (pod) > 0`,
		`avg_over_time({app="api"} | logfmt | unwrap duration_ms | __error__="" [5m]) > 500`,
		`quantile_over_time(0.99, {app="api"} | json | unwrap latency [1h30m]) by (route) > 1`,
		`sum(count_over_time({app="api"} |= "[ERROR] (db)" |~ ` + "`a[0-9]+`" + ` [10m])) > 0`,
		`count_over_time(({app="api"} |= "error")[1d]) > 0`,
	} {
		q, err := logql.Extract(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		query := logql.WithLabels(q.Log, map[string]string{"pod": "api-1"})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = c.Lines(ctx, query, time.Now().Add(-q.Range), time.Now(), 5, "")
		cancel()
		if err != nil {
			t.Errorf("Loki refused %s (from %s): %v", query, expr, err)
		}
	}
}
