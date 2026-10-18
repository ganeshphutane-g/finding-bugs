package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pp(t *testing.T, vars map[string]string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	env := Env{Stdout: &out, Stderr: &out, ReadFile: os.ReadFile,
		LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
	return Run(context.Background(), env, args), out.String()
}

func write(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "pushprobe.yaml")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	f := write(t, `
defaults: {retries: 1}
targets:
  kuma: {url: "https://kuma.example.com"}
checks:
  - name: web
    http: {url: "`+srv.URL+`", headers: {Authorization: "Bearer ${PUSHPROBE_WEB}"}}
    kuma: {push_token: "${KUMA_WEB}"}
  - name: printer
    retries: 0
    tcp: {address: "`+closed+`"}
    kuma: {push_token: abc}
`)
	vars := map[string]string{"PUSHPROBE_WEB": "s3cret"}
	code, out := pp(t, vars, "check", "-c", f)
	if code != 3 || !strings.Contains(out, "ok    web      status 200") || !strings.Contains(out, "FAIL  printer  connection refused") ||
		!strings.Contains(out, "2 checks: 1 ok, 1 failed. Nothing was sent.") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if code, out := pp(t, vars, "check", "-c", f, "web"); code != 0 || strings.Contains(out, "printer") {
		t.Fatalf("one check: exit %d:\n%s", code, out)
	}
	if code, out := pp(t, vars, "check", "--config="+f, "nope"); code != 2 || !strings.Contains(out, `no check named "nope"`) {
		t.Fatalf("unknown name: exit %d:\n%s", code, out)
	}
	// validate needs where results go.
	if code, out := pp(t, vars, "validate", "-c", f); code != 1 || !strings.Contains(out, "no value for KUMA_WEB") {
		t.Fatalf("validate: exit %d:\n%s", code, out)
	}
	vars["KUMA_WEB"] = "tok"
	if code, out := pp(t, vars, "validate", "-c", f); code != 0 || !strings.Contains(out, "is fine: 2 checks, 2 reporting to Uptime Kuma, 0 to Gatus") {
		t.Fatalf("validate: exit %d:\n%s", code, out)
	}
	// The file from the environment.
	vars["PUSHPROBE_CONFIG"] = f
	if code, out := pp(t, vars, "check", "web"); code != 0 {
		t.Fatalf("PUSHPROBE_CONFIG: exit %d:\n%s", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "name a command"},
		{[]string{"launch"}, 2, `unknown command "launch"`},
		{[]string{"help"}, 0, "Usage: pushprobe"},
		{[]string{"version"}, 0, "pushprobe dev"},
		{[]string{"check", "--bogus"}, 2, "bogus"},
		{[]string{"check", "-c", "/no/such/file.yaml"}, 1, "there is no /no/such/file.yaml here"},
	} {
		if code, out := pp(t, nil, c.args...); code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%v: exit %d %s", c.args, code, out)
		}
	}
	dir := t.TempDir()
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	if code, out := pp(t, nil, "check"); code != 1 || !strings.Contains(out, "there is no pushprobe.yaml here") {
		t.Errorf("default file: exit %d %s", code, out)
	}
}

func TestBadFile(t *testing.T) {
	f := write(t, "checks:\n  - name: a\n    tcp: {adress: 'x:1'}\n")
	if code, out := pp(t, nil, "check", "-c", f); code != 1 || !strings.Contains(out, `line 3: unknown key "adress" (did you mean address?)`) {
		t.Fatalf("exit %d %s", code, out)
	}
}
