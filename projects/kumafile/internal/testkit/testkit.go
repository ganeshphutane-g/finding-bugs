// Package testkit starts throwaway Uptime Kuma servers for tests. The tests
// run only with KUMAFILE_INTEGRATION=1; KUMAFILE_IMAGE picks the image
// (default louislam/uptime-kuma:2).
package testkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

// Admin is the account every test server is set up with.
const (
	AdminUser     = "admin"
	AdminPassword = "Test-pass-123"
)

// Server is a running instance.
type Server struct {
	T    *testing.T
	Name string
	URL  string
}

// Image is the server image under test.
func Image() string {
	if v := os.Getenv("KUMAFILE_IMAGE"); v != "" {
		return v
	}
	return "louislam/uptime-kuma:2"
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// Start runs a fresh server, set up with the admin account, and removes it
// when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	if os.Getenv("KUMAFILE_INTEGRATION") == "" {
		t.Skip("set KUMAFILE_INTEGRATION=1 to run tests against a real server")
	}
	b := make([]byte, 4)
	rand.Read(b)
	name := "kftest-" + hex.EncodeToString(b)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	if out, err := docker("run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:3001", port), Image()); err != nil {
		t.Fatalf("starting %s: %v\n%s", Image(), err, out)
	}
	t.Cleanup(func() { docker("rm", "-f", name) })
	s := &Server{T: t, Name: name, URL: url}

	// Choose the database (2.x asks on first start), then wait for the
	// real server.
	s.waitHTTP("/setup-database-info", 2*time.Minute)
	resp, err := http.Post(url+"/setup-database", "application/json", bytes.NewReader([]byte(`{"dbConfig":{"type":"sqlite"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, err := s.connect()
		if err == nil {
			var r struct {
				OK  bool   `json:"ok"`
				Msg string `json:"msg"`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = c.Call(ctx, &r, "setup", AdminUser, AdminPassword)
			cancel()
			c.Close()
			if err == nil && r.OK {
				break
			}
			if err == nil {
				err = fmt.Errorf("setup: %s", r.Msg)
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", name)
			t.Fatalf("server did not start: %v\n%s", err, logs)
		}
		time.Sleep(time.Second)
	}
	return s
}

func (s *Server) waitHTTP(path string, d time.Duration) {
	deadline := time.Now().Add(d)
	for {
		resp, err := http.Get(s.URL + path)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", s.Name)
			s.T.Fatalf("server did not answer %s: %v\n%s", path, err, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *Server) connect() (*kuma.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return kuma.Connect(ctx, s.URL)
}

// Client is a logged-in connection, closed when the test ends.
func (s *Server) Client() *kuma.Client {
	s.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := kuma.Connect(ctx, s.URL)
	if err != nil {
		s.T.Fatal(err)
	}
	s.T.Cleanup(func() { c.Close() })
	if _, err := c.Login(ctx, kuma.Credentials{Username: AdminUser, Password: AdminPassword}); err != nil {
		s.T.Fatal(err)
	}
	return c
}

// Must calls an event and fails the test unless the answer is ok. It
// returns the answer.
func (s *Server) Must(c *kuma.Client, event string, args ...any) map[string]any {
	s.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var r map[string]any
	if err := c.Call(ctx, &r, event, args...); err != nil {
		s.T.Fatalf("%s: %v", event, err)
	}
	if ok, _ := r["ok"].(bool); !ok {
		s.T.Fatalf("%s: %v", event, r["msg"])
	}
	return r
}

// Logs is the end of the server's log, for failure messages.
func (s *Server) Logs() string {
	out, _ := docker("logs", "--tail", "40", s.Name)
	return strings.TrimSpace(out)
}
