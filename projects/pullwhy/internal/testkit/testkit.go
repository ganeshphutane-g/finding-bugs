// Package testkit builds scratch Docker setups for side-by-side tests: a
// throwaway daemon (docker:dind) with its own daemon.json, registries on the
// host, and pullwhy run inside the scratch daemon's container, next to a
// real docker pull. Only with PULLWHY_INTEGRATION=1.
package testkit

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Integration skips the test unless asked for.
func Integration(t *testing.T) {
	t.Helper()
	if os.Getenv("PULLWHY_INTEGRATION") == "" {
		t.Skip("set PULLWHY_INTEGRATION=1 to run side by side with real docker pulls")
	}
}

// Docker runs the host's docker CLI.
func Docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := Docker(args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func name(kind string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return "pwtest-" + kind + "-" + hex.EncodeToString(b)
}

// Gateway is the host's address on Docker's default bridge: how a scratch
// daemon's container reaches what runs on the host.
func Gateway(t *testing.T) string {
	t.Helper()
	return mustDocker(t, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
}

// FreePort is a free TCP port.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Dir is a directory containers can read.
func Dir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pwtest")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(d, 0o755)
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func cleanup(t *testing.T, kind, container string) {
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := Docker("logs", "--tail", "30", container)
			t.Logf("%s logs:\n%s", kind, logs)
		}
		Docker("rm", "-f", "-v", container)
	})
}

// Registry is a registry:2 on the host.
type Registry struct {
	Container string
	Port      int
	// Addr is gateway:port, as scratch daemons see it; Local is
	// 127.0.0.1:port, for pushing from the host.
	Addr, Local string
	CA          []byte // PEM, for a TLS registry
	data        string
}

// RegistryOptions choose TLS and auth.
type RegistryOptions struct {
	TLS bool
	// User and Password turn on basic auth (htpasswd).
	User, Password string
	// Data reuses another registry's storage (to push before turning auth on).
	Data string
}

// StartRegistry runs registry:2 on a free port of every host address.
func StartRegistry(t *testing.T, gw string, o RegistryOptions) *Registry {
	t.Helper()
	r := &Registry{Port: FreePort(t), Container: name("registry")}
	r.Addr = fmt.Sprintf("%s:%d", gw, r.Port)
	r.Local = fmt.Sprintf("127.0.0.1:%d", r.Port)
	r.data = o.Data
	if r.data == "" {
		r.data = Dir(t)
	}
	args := []string{"run", "-d", "--name", r.Container, "-p", fmt.Sprintf("%d:5000", r.Port), "-v", r.data + ":/var/lib/registry"}
	conf := Dir(t)
	if o.TLS {
		ca, cert, key := certs(t, gw)
		r.CA = ca
		os.WriteFile(filepath.Join(conf, "reg.crt"), cert, 0o644)
		os.WriteFile(filepath.Join(conf, "reg.key"), key, 0o644)
		args = append(args, "-v", conf+":/conf:ro", "-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/conf/reg.crt", "-e", "REGISTRY_HTTP_TLS_KEY=/conf/reg.key")
	}
	if o.User != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(o.Password), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(conf, "htpasswd"), []byte(o.User+":"+string(hash)+"\n"), 0o644)
		args = append(args, "-v", conf+":/conf:ro", "-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=test",
			"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/conf/htpasswd")
	}
	args = append(args, "registry:2")
	mustDocker(t, args...)
	cleanup(t, "registry", r.Container)
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if c, err := net.DialTimeout("tcp", r.Local, time.Second); err == nil {
			c.Close()
			return r
		}
	}
	t.Fatal("the registry did not start")
	return nil
}

// Data is the registry's storage.
func (r *Registry) Data() string { return r.data }

// Push copies a host image to the registry as repo:tag.
func (r *Registry) Push(t *testing.T, image, repoTag string) {
	t.Helper()
	dst := r.Local + "/" + repoTag
	mustDocker(t, "tag", image, dst)
	mustDocker(t, "push", "-q", dst)
	Docker("rmi", dst)
}

// PutIndex stores an image index at repo:tag that lists one manifest (the
// one at repo:from) for one platform: an image built for that platform only.
func (r *Registry) PutIndex(t *testing.T, repo, from, tag, platform string) {
	t.Helper()
	base := "http://" + r.Local + "/v2/" + repo + "/manifests/"
	req, _ := http.NewRequest("HEAD", base+from, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("HEAD %s: %v %v", base+from, err, resp)
	}
	osArch := strings.SplitN(platform, "/", 2)
	idx, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{map[string]any{"mediaType": resp.Header.Get("Content-Type"), "digest": resp.Header.Get("Docker-Content-Digest"),
			"size": resp.ContentLength, "platform": map[string]string{"os": osArch[0], "architecture": osArch[1]}}},
	})
	put, _ := http.NewRequest("PUT", base+tag, bytes.NewReader(idx))
	put.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	resp, err = http.DefaultClient.Do(put)
	if err != nil || resp.StatusCode != 201 {
		b := []byte{}
		if resp != nil {
			b, _ = io.ReadAll(resp.Body)
		}
		t.Fatalf("PUT %s: %v %s", base+tag, err, b)
	}
}

// certs makes a CA and a certificate for ip, signed by it.
func certs(t *testing.T, ip string) (ca, cert, key []byte) {
	t.Helper()
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pullwhy test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: ip}, IPAddresses: []net.IP{net.ParseIP(ip), net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, caCert, &k.PublicKey, caKey)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// Binary builds a static pullwhy once.
func Binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Join(filepath.Dir(file), "..", "..")
		dir, err := os.MkdirTemp("", "pwbin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "pullwhy")
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// DindImage is the scratch daemon's image.
func DindImage() string {
	if v := os.Getenv("PULLWHY_DIND_IMAGE"); v != "" {
		return v
	}
	return "docker:dind"
}

// Dind is a scratch daemon in a container.
type Dind struct {
	Container string
	t         *testing.T
}

// StartDind runs a scratch daemon with daemonJSON (may be empty), on the
// classic image store when classic is set, with pullwhy copied in.
func StartDind(t *testing.T, daemonJSON string, classic bool) *Dind {
	t.Helper()
	d := &Dind{Container: name("dind"), t: t}
	args := []string{"run", "-d", "--privileged", "--name", d.Container, "-e", "DOCKER_TLS_CERTDIR="}
	if daemonJSON != "" {
		dir := Dir(t)
		os.WriteFile(filepath.Join(dir, "daemon.json"), []byte(daemonJSON), 0o644)
		args = append(args, "-v", filepath.Join(dir, "daemon.json")+":/etc/docker/daemon.json:ro")
	}
	// Where outgoing TLS is intercepted, the scratch daemon needs that CA
	// too (PULLWHY_DIND_CA: a PEM bundle to use as its system roots).
	if ca := os.Getenv("PULLWHY_DIND_CA"); ca != "" {
		args = append(args, "-v", ca+":/etc/ssl/certs/ca-certificates.crt:ro")
	}
	args = append(args, DindImage())
	if classic {
		args = append(args, "--feature", "containerd-snapshotter=false")
	}
	mustDocker(t, args...)
	cleanup(t, "dind", d.Container)
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if _, err := Docker("exec", d.Container, "docker", "info"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scratch daemon did not start")
		}
	}
	// Make sure the store is the one asked for.
	status, _ := d.Exec("docker", "info", "--format", "{{.DriverStatus}}")
	if containerd := strings.Contains(status, "snapshotter"); containerd == classic {
		t.Fatalf("the scratch daemon's image store is not the one asked for (classic=%v): %s", classic, status)
	}
	mustDocker(t, "cp", Binary(t), d.Container+":/usr/local/bin/pullwhy")
	return d
}

// Write puts content at path inside the container.
func (d *Dind) Write(path string, content []byte) {
	d.t.Helper()
	dir := Dir(d.t)
	f := filepath.Join(dir, "f")
	os.WriteFile(f, content, 0o644)
	mustDocker(d.t, "exec", d.Container, "mkdir", "-p", filepath.Dir(path))
	mustDocker(d.t, "cp", f, d.Container+":"+path)
}

// Exec runs a command in the container and returns its output and exit
// code.
func (d *Dind) Exec(args ...string) (string, int) {
	cmd := exec.Command("docker", append([]string{"exec", d.Container}, args...)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return out.String(), code
}

// Pull is a real docker pull in the scratch daemon.
type Pull struct {
	Output string
	Code   int
	Took   time.Duration
}

// Pull runs docker pull (args before the image are passed on).
func (d *Dind) Pull(args ...string) Pull {
	start := time.Now()
	out, code := d.Exec(append([]string{"docker", "pull"}, args...)...)
	return Pull{Output: out, Code: code, Took: time.Since(start)}
}

// Report is the part of pullwhy's JSON the tests read.
type Report struct {
	Findings []struct {
		Severity string   `json:"severity"`
		Kind     string   `json:"kind"`
		Title    string   `json:"title"`
		Evidence []string `json:"evidence"`
		Fix      string   `json:"fix"`
	} `json:"findings"`
	Plan []struct {
		Endpoint string `json:"endpoint"`
		Mirror   bool   `json:"mirror"`
		Proxy    string `json:"proxy"`
		OK       bool   `json:"ok"`
		Kind     string `json:"kind"`
		Digest   string `json:"digest"`
	} `json:"plan"`
	Raw  string `json:"-"`
	Code int    `json:"-"`
}

// Kinds lists the findings' kinds.
func (r *Report) Kinds() string {
	var ks []string
	for _, f := range r.Findings {
		ks = append(ks, f.Kind)
	}
	return strings.Join(ks, " ")
}

// Pullwhy runs pullwhy --json in the container.
func (d *Dind) Pullwhy(args ...string) *Report {
	d.t.Helper()
	cmd := exec.Command("docker", append([]string{"exec", d.Container, "pullwhy", "--json"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := &Report{Raw: out.String() + errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		d.t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), r); err != nil && r.Code != 1 && r.Code != 2 {
		d.t.Fatalf("pullwhy's JSON: %v\n%s", err, r.Raw)
	}
	return r
}

// ConnectProxy is a CONNECT (and plain HTTP) proxy on the host, which
// records the hosts asked for.
type ConnectProxy struct {
	Port  int
	mu    sync.Mutex
	hosts []string
	srv   *http.Server
}

// StartProxy listens on every address.
func StartProxy(t *testing.T) *ConnectProxy {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	p := &ConnectProxy{Port: l.Addr().(*net.TCPAddr).Port}
	p.srv = &http.Server{Handler: p}
	go p.srv.Serve(l)
	t.Cleanup(func() { p.srv.Close() })
	return p
}

// Hosts are the hosts asked for.
func (p *ConnectProxy) Hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

func (p *ConnectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.mu.Unlock()
	if r.Method != "CONNECT" {
		r.RequestURI = ""
		out, err := (&http.Transport{Proxy: nil}).RoundTrip(r)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer out.Body.Close()
		for k, v := range out.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(out.StatusCode)
		io.Copy(w, out.Body)
		return
	}
	up, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	w.WriteHeader(200)
	c, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	go func() { io.Copy(up, c); up.Close() }()
	io.Copy(c, up)
	c.Close()
}
