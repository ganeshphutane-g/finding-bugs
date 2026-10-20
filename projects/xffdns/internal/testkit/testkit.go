// Package testkit starts throwaway AdGuard Home servers and proxies in
// Docker for tests. They run only with XFFDNS_INTEGRATION=1, on the host
// network; XFFDNS_ADGUARD_IMAGE picks the AdGuard Home image (default
// adguard/adguardhome:latest).
package testkit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Integration skips the test unless asked for.
func Integration(t *testing.T) {
	t.Helper()
	if os.Getenv("XFFDNS_INTEGRATION") == "" {
		t.Skip("set XFFDNS_INTEGRATION=1 to run tests against real servers")
	}
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func name(kind string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return "xffdnstest-" + kind + "-" + hex.EncodeToString(b)
}

// FreePort is a port free for both TCP and UDP on 127.0.0.1 (now).
func FreePort(t *testing.T) int {
	t.Helper()
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		l.Close()
		if err == nil {
			u.Close()
			return port
		}
	}
	t.Fatal("no free port")
	return 0
}

// Dir is a directory containers can read (t.TempDir is 0700).
func Dir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "xffdnstest")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o755)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// WriteFile writes a file containers can read.
func WriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Run starts a container on the host network and removes it at the end of
// the test. args are what docker run takes after its options: more
// options, the image, then the image's arguments.
func Run(t *testing.T, kind string, args ...string) string {
	t.Helper()
	n := name(kind)
	out, err := docker(append([]string{"run", "-d", "--name", n, "--network", "host"}, args...)...)
	if err != nil {
		t.Fatalf("docker run %s: %v\n%s", kind, err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := docker("logs", "--tail", "40", n)
			t.Logf("%s logs:\n%s", kind, logs)
		}
		docker("rm", "-f", n)
	})
	return n
}

// WaitTCP waits for something to listen on addr.
func WaitTCP(t *testing.T, addr string) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return
		}
	}
	t.Fatalf("nothing listens on %s", addr)
}

// AdGuardImage is the AdGuard Home image under test.
func AdGuardImage() string {
	if v := os.Getenv("XFFDNS_ADGUARD_IMAGE"); v != "" {
		return v
	}
	return "adguard/adguardhome:latest"
}

// AdGuard is a running AdGuard Home with DoH over plain HTTP on.
type AdGuard struct {
	Web     string // http://127.0.0.1:port, where DoH and the API are
	DNSPort int
	User    string
	Pass    string
	name    string
	http    *http.Client
}

// StartAdGuard runs AdGuard Home (host network) and sets it up: user
// admin, web and DNS on free ports of 127.0.0.1, and DoH over plain HTTP.
// Its setup wizard needs port 3000 free.
func StartAdGuard(t *testing.T) *AdGuard {
	t.Helper()
	Integration(t)
	if c, err := net.DialTimeout("tcp", "127.0.0.1:3000", time.Second); err == nil {
		c.Close()
		t.Fatal("port 3000 is taken; AdGuard Home's setup wizard needs it")
	}
	a := &AdGuard{DNSPort: FreePort(t), User: "admin", Pass: "Test-pass-123",
		http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	webPort := FreePort(t)
	a.Web = fmt.Sprintf("http://127.0.0.1:%d", webPort)
	a.name = Run(t, "adguard", AdGuardImage())
	WaitTCP(t, "127.0.0.1:3000")
	setup := fmt.Sprintf(`{"web":{"ip":"127.0.0.1","port":%d},"dns":{"ip":"127.0.0.1","port":%d},"username":%q,"password":%q}`,
		webPort, a.DNSPort, a.User, a.Pass)
	var err error
	for try := 0; try < 20; try++ {
		var resp *http.Response
		resp, err = a.http.Post("http://127.0.0.1:3000/control/install/configure", "application/json", strings.NewReader(setup))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				err = nil
				break
			}
			err = fmt.Errorf("%s: %s", resp.Status, b)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("AdGuard Home setup: %v", err)
	}
	WaitTCP(t, fmt.Sprintf("127.0.0.1:%d", webPort))
	// Plain-HTTP DoH: http.doh.insecure_enabled since v0.107.74,
	// tls.allow_unencrypted_doh before.
	conf := "/opt/adguardhome/conf/AdGuardHome.yaml"
	if out, err := docker("exec", a.name, "sed", "-i", "-e", "s/insecure_enabled: false/insecure_enabled: true/",
		"-e", "s/allow_unencrypted_doh: false/allow_unencrypted_doh: true/", conf); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := docker("restart", a.name); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	WaitTCP(t, fmt.Sprintf("127.0.0.1:%d", webPort))
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		if _, err := a.call("GET", "/control/status", nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("AdGuard Home's API did not come up")
		}
	}
	return a
}

func (a *AdGuard) call(method, path string, body any) ([]byte, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.Web+path, r)
	req.SetBasicAuth(a.User, a.Pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s %s: %s %s", method, path, resp.Status, b)
	}
	return b, nil
}

// API calls AdGuard Home's API and fails the test on an error.
func (a *AdGuard) API(t *testing.T, method, path string, body any) []byte {
	t.Helper()
	b, err := a.call(method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// AddClient adds a persistent client with its own blocked services.
func (a *AdGuard) AddClient(t *testing.T, name string, ids []string, blocked []string) {
	t.Helper()
	a.API(t, "POST", "/control/clients/add", map[string]any{"name": name, "ids": ids, "use_global_settings": true,
		"use_global_blocked_services": false, "blocked_services": blocked, "filtering_enabled": true})
}

// SetRules replaces the custom filtering rules.
func (a *AdGuard) SetRules(t *testing.T, rules ...string) {
	t.Helper()
	a.API(t, "POST", "/control/filtering/set_rules", map[string]any{"rules": rules})
}

// Disallow puts addresses on the access list's disallowed clients.
func (a *AdGuard) Disallow(t *testing.T, ids ...string) {
	t.Helper()
	a.API(t, "POST", "/control/access/set", map[string]any{"allowed_clients": []string{}, "disallowed_clients": ids, "blocked_hosts": []string{}})
}

// LogEntry is one query in AdGuard Home's query log.
type LogEntry struct {
	Client      string `json:"client"`
	ClientID    string `json:"client_id"`
	ClientProto string `json:"client_proto"`
	Reason      string `json:"reason"`
	ServiceName string `json:"service_name"`
	ClientInfo  struct {
		Name string `json:"name"`
	} `json:"client_info"`
	Question struct {
		Name string `json:"name"`
	} `json:"question"`
}

// FindLog waits for a query for name to show in the query log.
func (a *AdGuard) FindLog(t *testing.T, name string) *LogEntry {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		b := a.API(t, "GET", "/control/querylog?limit=50&search="+url.QueryEscape(name), nil)
		var body struct {
			Data []LogEntry `json:"data"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		for _, e := range body.Data {
			if e.Question.Name == name {
				return &e
			}
		}
	}
	return nil
}
