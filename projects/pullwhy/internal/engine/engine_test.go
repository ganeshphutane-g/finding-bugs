package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	e, _ := Find(getenv, dir)
	if e.Host != DefaultHost || e.From != "default" || !e.Local() {
		t.Fatalf("%+v", e)
	}
	env["DOCKER_HOST"] = "tcp://10.0.0.5:2376"
	env["DOCKER_TLS_VERIFY"] = "1"
	if e, _ := Find(getenv, dir); e.Host != "tcp://10.0.0.5:2376" || !e.TLSVerify || e.Local() {
		t.Fatalf("%+v", e)
	}
	delete(env, "DOCKER_HOST")
	// The config's current context, stored under the sha256 of its name.
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"remote"}`), 0o600)
	sum := sha256.Sum256([]byte("remote"))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	os.MkdirAll(meta, 0o700)
	os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Name":"remote","Endpoints":{"docker":{"Host":"tcp://box:2375","SkipTLSVerify":false}}}`), 0o600)
	if e, err := Find(getenv, dir); err != nil || e.Host != "tcp://box:2375" || e.From != "context remote" {
		t.Fatalf("%+v %v", e, err)
	}
	env["DOCKER_CONTEXT"] = "missing"
	if _, err := Find(getenv, dir); err == nil {
		t.Fatal("a missing context was accepted")
	}
	env["DOCKER_CONTEXT"] = "default"
	if e, _ := Find(getenv, dir); e.Host != DefaultHost {
		t.Fatal(e)
	}
}

// fakeDaemon serves the API on a unix socket.
func fakeDaemon(t *testing.T, h http.Handler) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	c, err := New(&Endpoint{Host: "unix://" + sock})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInfo(t *testing.T) {
	c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"ServerVersion":"29.8.2","OperatingSystem":"Docker Desktop","DriverStatus":[["driver-type","io.containerd.snapshotter.v1"]],
"HttpsProxy":"http://proxy:3128","NoProxy":"10.0.0.0/8","RegistryConfig":{"InsecureRegistryCIDRs":["127.0.0.0/8"],
"IndexConfigs":{"docker.io":{"Name":"docker.io","Mirrors":["https://mirror.example/"],"Secure":true,"Official":true}},"Mirrors":["https://mirror.example/"]}}`)
	}))
	i, err := c.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if i.ServerVersion != "29.8.2" || !i.ContainerdStore() || !i.DockerDesktop() || i.HTTPSProxy != "http://proxy:3128" ||
		i.RegistryConfig.IndexConfigs["docker.io"].Mirrors[0] != "https://mirror.example/" {
		t.Fatalf("%+v", i)
	}
}

func TestPull(t *testing.T) {
	var got *http.Request
	c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		fl := w.(http.Flusher)
		for _, line := range []string{
			`{"status":"Pulling from library/nginx","id":"alpine"}`,
			`{"status":"Pulling fs layer","progressDetail":{},"id":"1fdd"}`,
			`{"status":"Downloading","progressDetail":{"current":3145728,"total":20530756},"id":"1fdd"}`,
		} {
			fmt.Fprintln(w, line)
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		if r.URL.Query().Get("tag") == "fail" {
			fmt.Fprintln(w, `{"errorDetail":{"message":"toomanyrequests: You have reached your unauthenticated pull rate limit."},"error":"toomanyrequests: ..."}`)
			return
		}
		fmt.Fprintln(w, `{"status":"Pull complete","progressDetail":{},"id":"1fdd"}`)
	}))
	var evs []Event
	err := c.Pull(context.Background(), "docker.io/library/nginx", "alpine", "linux/arm64", "e30=", func(e Event) { evs = append(evs, e) })
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 || evs[2].ProgressDetail.Total != 20530756 || !evs[2].At.After(evs[0].At) {
		t.Fatalf("%+v", evs)
	}
	q := got.URL.Query()
	if got.Method != "POST" || q.Get("fromImage") != "docker.io/library/nginx" || q.Get("tag") != "alpine" || q.Get("platform") != "linux/arm64" ||
		got.Header.Get("X-Registry-Auth") != "e30=" {
		t.Fatalf("%s %s %v", got.Method, got.URL, got.Header)
	}
	err = c.Pull(context.Background(), "docker.io/library/nginx", "fail", "", "", nil)
	if err == nil || !strings.Contains(err.Error(), "toomanyrequests") {
		t.Fatal(err)
	}
	d := "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
	c.Pull(context.Background(), "docker.io/library/alpine", d, "", "", nil)
	if got.URL.Query().Get("fromImage") != "docker.io/library/alpine@"+d || got.URL.Query().Get("tag") != "" {
		t.Fatal(got.URL)
	}
}

func TestErrors(t *testing.T) {
	c := fakeDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"pull access denied for nope, repository does not exist or may require 'docker login'"}`)
	}))
	err := c.Pull(context.Background(), "docker.io/library/nope", "latest", "", "", nil)
	var de *DaemonError
	if !errorsAs(err, &de) || de.Status != 404 || !strings.Contains(de.Message, "pull access denied") {
		t.Fatal(err)
	}
	dead, _ := New(&Endpoint{Host: "unix://" + filepath.Join(t.TempDir(), "none.sock")})
	if _, err := dead.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot reach the Docker daemon") {
		t.Fatal(err)
	}
	if _, err := New(&Endpoint{Host: "ssh://box"}); err == nil {
		t.Fatal("ssh:// was accepted")
	}
}

func errorsAs(err error, target **DaemonError) bool {
	de, ok := err.(*DaemonError)
	if ok {
		*target = de
	}
	return ok
}
