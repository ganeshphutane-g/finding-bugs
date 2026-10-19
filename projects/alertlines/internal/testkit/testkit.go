// Package testkit starts throwaway Loki and Alertmanager servers for tests.
// They run only with ALERTLINES_INTEGRATION=1; ALERTLINES_LOKI_IMAGE picks
// the Loki image (default grafana/loki:latest).
package testkit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// Integration skips the test unless asked for.
func Integration(t *testing.T) {
	t.Helper()
	if os.Getenv("ALERTLINES_INTEGRATION") == "" {
		t.Skip("set ALERTLINES_INTEGRATION=1 to run tests against real servers")
	}
}

// FreePort is a TCP port nothing listens on (now).
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func name(kind string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return "altest-" + kind + "-" + hex.EncodeToString(b)
}

// LokiImage is the Loki image under test.
func LokiImage() string {
	if v := os.Getenv("ALERTLINES_LOKI_IMAGE"); v != "" {
		return v
	}
	return "grafana/loki:latest"
}

// Loki is a running Loki.
type Loki struct {
	URL  string
	Name string
}

// StartLoki runs Loki (host network, its own ports) with a ruler that
// evaluates rules (YAML of rule groups, for tenant "fake") every 2 seconds
// and sends alerts to alertmanagerURL.
func StartLoki(t *testing.T, rules, alertmanagerURL string) *Loki {
	t.Helper()
	Integration(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "rules", "fake"), 0o755)
	if rules == "" {
		rules = "groups: []\n"
	}
	os.WriteFile(filepath.Join(dir, "rules", "fake", "rules.yaml"), []byte(rules), 0o644)
	httpPort, grpcPort := FreePort(t), FreePort(t)
	cfg := fmt.Sprintf(`auth_enabled: false
server: {http_listen_port: %d, grpc_listen_port: %d}
common:
  instance_addr: 127.0.0.1
  path_prefix: /tmp/loki
  storage: {filesystem: {chunks_directory: /tmp/loki/chunks, rules_directory: /tmp/loki/rules}}
  replication_factor: 1
  ring: {kvstore: {store: inmemory}}
schema_config:
  configs:
    - {from: 2020-10-24, store: tsdb, object_store: filesystem, schema: v13, index: {prefix: index_, period: 24h}}
ruler:
  alertmanager_url: %s
  enable_api: true
  evaluation_interval: 2s
  poll_interval: 2s
  storage: {type: local, local: {directory: /etc/loki/rules}}
  rule_path: /tmp/loki/rule-tmp
analytics: {reporting_enabled: false}
`, httpPort, grpcPort, alertmanagerURL)
	os.WriteFile(filepath.Join(dir, "loki.yaml"), []byte(cfg), 0o644)
	os.Chmod(dir, 0o755)
	l := &Loki{URL: fmt.Sprintf("http://127.0.0.1:%d", httpPort), Name: name("loki")}
	if out, err := docker("run", "-d", "--name", l.Name, "--network", "host",
		"-v", filepath.Join(dir, "loki.yaml")+":/etc/loki/loki.yaml:ro", "-v", filepath.Join(dir, "rules")+":/etc/loki/rules:ro",
		LokiImage(), "-config.file=/etc/loki/loki.yaml"); err != nil {
		t.Fatalf("starting %s: %v\n%s", LokiImage(), err, out)
	}
	t.Cleanup(func() { docker("rm", "-f", l.Name) })
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(l.URL + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return l
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", l.Name)
			t.Fatalf("Loki did not start: %v\n%s", err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Push sends log lines (newest last) to one stream, timed now.
func (l *Loki) Push(t *testing.T, labels map[string]string, lines ...string) {
	t.Helper()
	now := time.Now().UnixNano()
	var values [][2]string
	for i, line := range lines {
		values = append(values, [2]string{strconv.FormatInt(now+int64(i)*1000, 10), line})
	}
	body, _ := json.Marshal(map[string]any{"streams": []any{map[string]any{"stream": labels, "values": values}}})
	resp, err := http.Post(l.URL+"/loki/api/v1/push", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("push: %s", resp.Status)
	}
}
