// Package testkit starts throwaway Gitea (or Forgejo) servers for tests. The
// tests run only with ISSUEMOVE_INTEGRATION=1; ISSUEMOVE_IMAGE picks the
// image (default gitea/gitea:1.24).
package testkit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
)

// Server is a running instance with an admin user "root".
type Server struct {
	T     *testing.T
	Name  string
	URL   string
	Admin *gitea.Client
}

// Image is the server image under test.
func Image() string {
	if v := os.Getenv("ISSUEMOVE_IMAGE"); v != "" {
		return v
	}
	return "gitea/gitea:1.24"
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// Start runs a fresh server and removes it when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	if os.Getenv("ISSUEMOVE_INTEGRATION") == "" {
		t.Skip("set ISSUEMOVE_INTEGRATION=1 to run tests against a real server")
	}
	b := make([]byte, 4)
	rand.Read(b)
	name := "imtest-" + hex.EncodeToString(b)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	out, err := docker("run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:3000", port),
		"-e", "GITEA__security__INSTALL_LOCK=true", "-e", "GITEA__database__DB_TYPE=sqlite3",
		"-e", "GITEA__server__ROOT_URL="+url+"/", "-e", "FORGEJO__security__INSTALL_LOCK=true",
		"-e", "FORGEJO__database__DB_TYPE=sqlite3", "-e", "FORGEJO__server__ROOT_URL="+url+"/", Image())
	if err != nil {
		t.Fatalf("starting %s: %v\n%s", Image(), err, out)
	}
	t.Cleanup(func() { docker("rm", "-f", name) })
	s := &Server{T: t, Name: name, URL: url}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(url + "/api/v1/version")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", "--tail", "30", name)
			t.Fatalf("server did not start:\n%s", logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.User("root", true)
	s.Admin = gitea.New(url, s.Token("root"))
	return s
}

const password = "Test-pass-123"

// User creates an account.
func (s *Server) User(name string, admin bool) {
	s.T.Helper()
	bin := "gitea"
	if strings.Contains(Image(), "forgejo") {
		bin = "forgejo"
	}
	args := []string{"exec", "-u", "git", s.Name, bin, "admin", "user", "create", "--username", name,
		"--password", password, "--email", name + "@example.com", "--must-change-password=false"}
	if admin {
		args = append(args, "--admin")
	}
	if out, err := docker(args...); err != nil {
		s.T.Fatalf("creating user %s: %v\n%s", name, err, out)
	}
}

// Token makes an API token for a user.
func (s *Server) Token(user string) string {
	s.T.Helper()
	b := make([]byte, 4)
	rand.Read(b)
	body, _ := json.Marshal(map[string]any{"name": "t" + hex.EncodeToString(b), "scopes": []string{"all"}})
	req, _ := http.NewRequest("POST", s.URL+"/api/v1/users/"+user+"/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.T.Fatal(err)
	}
	defer resp.Body.Close()
	var t struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.SHA1 == "" {
		s.T.Fatalf("token for %s: status %d %v", user, resp.StatusCode, err)
	}
	return t.SHA1
}

// Client is an API client for a user.
func (s *Server) Client(user string) *gitea.Client { return gitea.New(s.URL, s.Token(user)) }

// Call makes a raw API request as the admin (for setting up tests).
func (s *Server) Call(method, path string, body any, out any) {
	s.T.Helper()
	if err := s.Admin.Raw(context.Background(), method, path, body, out); err != nil {
		s.T.Fatalf("%s %s: %v", method, path, err)
	}
}

// Repo creates a repository owned by the admin.
func (s *Server) Repo(name string) {
	s.T.Helper()
	s.Call("POST", "/user/repos", map[string]any{"name": name}, nil)
}
