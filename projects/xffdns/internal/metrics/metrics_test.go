package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	m := New()
	m.Query("dot", Answered)
	m.Query("dot", Answered)
	m.Query("udp", RateLimited)
	m.UpstreamError("timeout")
	m.Conn(1)
	m.Conn(1)
	m.Conn(-1)
	m.Upstream(3 * time.Millisecond)
	m.Upstream(2 * time.Second)
	m.Upstream(time.Minute)
	var b strings.Builder
	m.Write(&b)
	out := b.String()
	for _, want := range []string{
		`xffdns_queries_total{listener="dot",result="answered"} 2`,
		`xffdns_queries_total{listener="udp",result="rate_limited"} 1`,
		`xffdns_upstream_errors_total{kind="timeout"} 1`,
		"# TYPE xffdns_queries_total counter",
		"xffdns_open_conns 1",
		`xffdns_upstream_seconds_bucket{le="0.005"} 1`,
		`xffdns_upstream_seconds_bucket{le="2.5"} 2`,
		`xffdns_upstream_seconds_bucket{le="+Inf"} 3`,
		"xffdns_upstream_seconds_count 3",
		"xffdns_upstream_seconds_sum 62.003",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	var nilm *Metrics
	nilm.Query("x", Answered) // no panic
	nilm.Conn(1)
	nilm.Upstream(time.Second)
}

func TestHandler(t *testing.T) {
	m := New()
	m.Query("a\"b\\é\n", Answered)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	for path, want := range map[string]string{"/metrics": `listener="a\"b\\é\n"`, "/healthz": "ok"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s: %s", path, b)
		}
	}
	resp, _ := srv.Client().Get(srv.URL + "/other")
	if resp.StatusCode != 404 {
		t.Fatal(resp.Status)
	}
}

func TestHealthy(t *testing.T) {
	m := New()
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	if ok, _ := m.Healthy(); !ok {
		t.Fatal("unhealthy before any query")
	}
	m.UpstreamError("timeout")
	if ok, msg := m.Healthy(); ok || !strings.Contains(msg, "never") {
		t.Fatal(msg)
	}
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	if resp, _ := srv.Client().Get(srv.URL + "/healthz"); resp.StatusCode != 503 {
		t.Fatal(resp.Status)
	}
	now = now.Add(time.Second)
	m.UpstreamOK()
	if ok, _ := m.Healthy(); !ok {
		t.Fatal("unhealthy after an answer")
	}
	now = now.Add(time.Second)
	m.UpstreamError("timeout")
	now = now.Add(2 * time.Minute)
	if ok, _ := m.Healthy(); !ok {
		t.Fatal("an old error still counts")
	}
}
