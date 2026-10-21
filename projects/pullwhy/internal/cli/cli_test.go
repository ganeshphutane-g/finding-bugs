package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/pullwhy/internal/plan"
)

// fakeRegistry: anonymous tokens; "team/app" exists, the rest do not.
func fakeRegistry(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, srv.URL))
			w.WriteHeader(401)
		case r.URL.Path == "/token":
			fmt.Fprint(w, `{"token":"t"}`)
		case r.URL.Path == "/v2/team/app/manifests/1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395")
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeDaemon serves /info and /images/create on a unix socket.
func fakeDaemon(t *testing.T, pullLines []string) string {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info"):
			fmt.Fprint(w, `{"ServerVersion":"29.8.2","OSType":"linux","Architecture":"x86_64",
"DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],
"RegistryConfig":{"InsecureRegistryCIDRs":["127.0.0.0/8"],"IndexConfigs":{"docker.io":{"Name":"docker.io","Secure":true,"Official":true}}}}`)
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			for _, line := range pullLines {
				fmt.Fprintln(w, line)
			}
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "unix://" + sock
}

func run(t *testing.T, host string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	env := Env{
		Getenv:    func(k string) string { return map[string]string{"DOCKER_HOST": host}[k] },
		ConfigDir: t.TempDir(),
		Resolver:  plan.DefaultResolver,
	}
	code := Run(context.Background(), args, &out, &errb, env)
	return code, out.String(), errb.String()
}

func TestHealthy(t *testing.T) {
	reg := fakeRegistry(t)
	host := fakeDaemon(t, []string{
		`{"status":"Pulling from team/app","id":"1"}`,
		`{"status":"Pulling fs layer","progressDetail":{},"id":"aaa"}`,
		`{"status":"Download complete","progressDetail":{},"id":"aaa"}`,
		`{"status":"Pull complete","progressDetail":{},"id":"aaa"}`,
		`{"status":"Status: Downloaded newer image for app:1"}`,
	})
	image := strings.TrimPrefix(reg.URL, "http://") + "/team/app:1"
	code, out, errs := run(t, host, "--pull", image)
	if code != exitOK {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"Docker 29.8.2, containerd image store", "1. registry https://", "2. registry http://", "found     sha256:d56c",
		"pull by the daemon", "Downloaded newer image", "Nothing wrong found"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	// On loopback, HTTPS fails and plain HTTP works (insecure by default).
	if !strings.Contains(out, "insecure: 127.0.0.1 is in the daemon's insecure range 127.0.0.0/8") {
		t.Error(out)
	}
}

func TestProblems(t *testing.T) {
	reg := fakeRegistry(t)
	host := fakeDaemon(t, []string{`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`})
	image := strings.TrimPrefix(reg.URL, "http://") + "/team/gone:1"
	code, out, _ := run(t, host, "--json", "--pull", image)
	if code != exitProblem {
		t.Fatalf("%d\n%s", code, out)
	}
	var rep struct {
		Image    string
		Findings []struct{ Severity, Kind, Title string }
		Plan     []struct {
			Endpoint string
			Kind     string
		}
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err, out)
	}
	if rep.Findings[0].Kind != "not-found" || len(rep.Plan) != 2 {
		t.Fatalf("%+v", rep)
	}
	// The pull failed the same way: said once.
	if len(rep.Findings) != 1 {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"Bad_Name"}, {"--platform", "linux", "x"}, {"--timeout", "0s", "x"}, {"a", "b"}, {"--bogus"}} {
		if code, _, _ := run(t, "unix:///nonexistent", args...); code != exitUsage {
			t.Errorf("%v: %d", args, code)
		}
	}
	if code, out, _ := run(t, "", "--version"); code != 0 || !strings.HasPrefix(out, "pullwhy ") {
		t.Fatal(code, out)
	}
	if code, _, _ := run(t, "", "-h"); code != 0 {
		t.Fatal(code)
	}
	code, _, errs := run(t, "unix://"+filepath.Join(t.TempDir(), "none.sock"), "alpine")
	if code != exitError || !strings.Contains(errs, "cannot reach the Docker daemon") {
		t.Fatal(code, errs)
	}
}
