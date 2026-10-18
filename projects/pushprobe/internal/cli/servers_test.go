package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/kuma"
	"github.com/ganeshphutane-g/pushprobe/internal/runner"
	"github.com/ganeshphutane-g/pushprobe/internal/testkit"
)

type beat struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
}

// kumaBeats reads a monitor's heartbeats, newest last.
func kumaBeats(t *testing.T, s *testkit.Server, c *kuma.Client, id int64) []beat {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r struct {
		OK   bool   `json:"ok"`
		Data []beat `json:"data"`
	}
	if err := c.Call(ctx, &r, "getMonitorBeats", id, 1); err != nil || !r.OK {
		t.Fatalf("getMonitorBeats: %v %+v", err, r)
	}
	return r.Data
}

type gatusResult struct {
	Success bool     `json:"success"`
	Errors  []string `json:"errors"`
}

func gatusResults(t *testing.T, url, key string) []gatusResult {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/endpoints/statuses")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var all []struct {
		Key     string        `json:"key"`
		Results []gatusResult `json:"results"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	for _, e := range all {
		if e.Key == key {
			return e.Results
		}
	}
	return nil
}

// TestRealServers pushes to a real Uptime Kuma and a real Gatus: up, a
// missed push turning Kuma's monitor down, and down with the reason.
func TestRealServers(t *testing.T) {
	gatus := testkit.StartGatus(t, "external-endpoints:\n  - {name: printer, group: office, token: gatus-secret-token, heartbeat: {interval: 10m}}\n")
	s := testkit.Start(t)
	c := s.Client()
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	r := s.Must(c, "add", map[string]any{"type": "push", "name": "printer", "pushToken": token, "interval": 20, "retryInterval": 20,
		"maxretries": 0, "accepted_statuscodes": []string{"200-299"}, "notificationIDList": map[string]any{}, "conditions": []any{}, "active": true})
	id := kuma.Int(r["monitorID"])

	runner.MaxStartDelay = time.Millisecond
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	file := write(t, `
defaults: {interval: 5s, timeout: 2s, retries: 0}
targets:
  kuma: {url: "`+s.URL+`"}
  gatus: {url: "`+gatus+`", token: "${GATUS_TOKEN}"}
checks:
  - name: printer
    tcp: {address: "`+target.Addr().String()+`"}
    kuma: {push_token: "${KUMA_PRINTER}"}
    gatus: {endpoint: office_printer}
`)
	vars := map[string]string{"KUMA_PRINTER": token, "GATUS_TOKEN": "gatus-secret-token"}
	var stderr syncBuffer
	env := Env{Stdout: io.Discard, Stderr: &stderr, ReadFile: os.ReadFile, Reload: make(chan struct{}),
		LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
	agent := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int)
		go func() { done <- Run(ctx, env, []string{"run", "-c", file}) }()
		return func() { cancel(); <-done }
	}

	stop := agent()
	eventually(t, "Kuma to show the check up", time.Minute, func() bool {
		bs := kumaBeats(t, s, c, id)
		return len(bs) > 0 && bs[len(bs)-1].Status == 1 && bs[len(bs)-1].Msg == "connected"
	})
	eventually(t, "Gatus to show the check up", time.Minute, func() bool {
		rs := gatusResults(t, gatus, "office_printer")
		return len(rs) > 0 && rs[len(rs)-1].Success
	})
	stop()

	// No pushes for longer than the monitor's interval: Kuma marks it down
	// by itself.
	eventually(t, "Kuma to mark the silent monitor down", 90*time.Second, func() bool {
		bs := kumaBeats(t, s, c, id)
		return len(bs) > 0 && bs[len(bs)-1].Status == 0
	})
	bs := kumaBeats(t, s, c, id)
	t.Logf("Kuma's own down beat: %q", bs[len(bs)-1].Msg)

	// The printer goes away: down, with the reason, in both.
	target.Close()
	stop = agent()
	defer stop()
	eventually(t, "Kuma to show the reason", time.Minute, func() bool {
		bs := kumaBeats(t, s, c, id)
		return len(bs) > 0 && bs[len(bs)-1].Status == 0 && bs[len(bs)-1].Msg == "connection refused"
	})
	eventually(t, "Gatus to show the failure", time.Minute, func() bool {
		rs := gatusResults(t, gatus, "office_printer")
		last := rs[len(rs)-1]
		return !last.Success && len(last.Errors) == 1 && last.Errors[0] == "connection refused"
	})
}
