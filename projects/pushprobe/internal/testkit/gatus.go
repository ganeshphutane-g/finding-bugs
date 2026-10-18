package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GatusImage is the Gatus image under test.
func GatusImage() string {
	if v := os.Getenv("PUSHPROBE_GATUS_IMAGE"); v != "" {
		return v
	}
	return "twinproduction/gatus:latest"
}

// StartGatus runs a Gatus server with the given config (YAML) and returns
// its URL. Gatus refuses a config without an ordinary endpoint, so one that
// checks Gatus itself is added.
func StartGatus(t *testing.T, config string) string {
	t.Helper()
	if os.Getenv("PUSHPROBE_INTEGRATION") == "" {
		t.Skip("set PUSHPROBE_INTEGRATION=1 to run tests against a real server")
	}
	dir := t.TempDir()
	config += "\nendpoints:\n  - {name: self, url: 'http://127.0.0.1:8080/health', interval: 1m, conditions: ['[STATUS] == 200']}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	b := make([]byte, 4)
	rand.Read(b)
	name := "pptest-gatus-" + hex.EncodeToString(b)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if out, err := docker("run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		"-v", dir+":/config:ro", GatusImage()); err != nil {
		t.Fatalf("starting %s: %v\n%s", GatusImage(), err, out)
	}
	t.Cleanup(func() { docker("rm", "-f", name) })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := http.Get(url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return url
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", name)
			t.Fatalf("Gatus did not start: %v\n%s", err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
