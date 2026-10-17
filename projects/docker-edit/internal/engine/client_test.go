package engine

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDaemon serves handler on a unix socket and returns a client for it.
func fakeDaemon(t *testing.T, apiVersion string, handler http.HandlerFunc) *Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", apiVersion)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/", handler)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c, err := New(Endpoint{Host: "unix://" + sock, Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCallUsesVersionedPath(t *testing.T) {
	var gotPath string
	c := fakeDaemon(t, "1.47", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.47","Os":"linux","Arch":"amd64"}`))
	})
	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1.47/version" {
		t.Fatalf("path = %q", gotPath)
	}
	if v.Version != "27.0.0" || v.Os != "linux" {
		t.Fatalf("got %+v", v)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	c := fakeDaemon(t, "1.47", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"message":"volume is in use"}`))
	})
	err := c.Call(context.Background(), Request{Method: http.MethodDelete, Path: "/volumes/x"}, nil)
	if err == nil || err.Error() != "volume is in use" {
		t.Fatalf("err = %v", err)
	}
	if !IsConflict(err) || IsNotFound(err) {
		t.Fatalf("wrong classification for %v", err)
	}
}

func TestOldDaemonRejected(t *testing.T) {
	c := fakeDaemon(t, "1.40", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	_, err := c.Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectionErrorNamesHost(t *testing.T) {
	c, err := New(Endpoint{Host: "unix:///nonexistent/docker.sock", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Version(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/docker.sock") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnsupportedScheme(t *testing.T) {
	if _, err := New(Endpoint{Host: "ssh://user@host"}); err == nil {
		t.Fatal("expected ssh:// to be rejected")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.41", "1.41", 0},
		{"1.9", "1.41", -1},
		{"1.47", "1.41", 1},
		{"2", "1.99", 1},
		{"1.41.1", "1.41", 1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTCPDefaultPortsAndVerification(t *testing.T) {
	c, err := New(Endpoint{Host: "tcp://example.com"})
	if err != nil || c.baseURL != "http://example.com:2375" {
		t.Fatalf("plain tcp: %v %q", err, c.baseURL)
	}
	c, err = New(Endpoint{Host: "tcp://example.com", TLS: true, TLSVerify: true})
	if err != nil || c.baseURL != "https://example.com:2376" {
		t.Fatalf("tls tcp: %v %q", err, c.baseURL)
	}
	if c.http.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("verified TLS must check certificates")
	}
	c, err = New(Endpoint{Host: "https://example.com:443"})
	if err != nil || c.http.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("https:// must check certificates (err=%v)", err)
	}
	c, err = New(Endpoint{Host: "tcp://example.com:2376", TLS: true})
	if err != nil || !c.http.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("--tls without verify skips checks like the docker CLI (err=%v)", err)
	}
}
