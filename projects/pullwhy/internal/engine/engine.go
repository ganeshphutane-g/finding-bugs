// Package engine talks to the Docker daemon's API: what it is configured
// with (/info), and a real pull (/images/create) with its progress.
package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Endpoint is where the daemon is, and how it was found.
type Endpoint struct {
	Host string // unix:///var/run/docker.sock, tcp://host:2376
	// From says where Host came from: DOCKER_HOST, the context's name, or
	// "default".
	From string
	// TLS settings for tcp://, from DOCKER_TLS_VERIFY and DOCKER_CERT_PATH
	// (or the context's TLS files).
	TLSVerify bool
	CertPath  string
}

// DefaultHost is the daemon's socket on Linux.
const DefaultHost = "unix:///var/run/docker.sock"

// Find works out the daemon the docker CLI would use: DOCKER_HOST, then
// DOCKER_CONTEXT or the config's currentContext, then the default socket.
func Find(getenv func(string) string, configDir string) (*Endpoint, error) {
	if h := getenv("DOCKER_HOST"); h != "" {
		e := &Endpoint{Host: h, From: "DOCKER_HOST", TLSVerify: getenv("DOCKER_TLS_VERIFY") != "", CertPath: getenv("DOCKER_CERT_PATH")}
		if e.TLSVerify && e.CertPath == "" {
			e.CertPath = configDir // as the CLI: ~/.docker/{ca,cert,key}.pem
		}
		return e, nil
	}
	name := getenv("DOCKER_CONTEXT")
	if name == "" {
		var conf struct {
			CurrentContext string `json:"currentContext"`
		}
		if b, err := os.ReadFile(filepath.Join(configDir, "config.json")); err == nil {
			json.Unmarshal(b, &conf)
		}
		name = conf.CurrentContext
	}
	if name == "" || name == "default" {
		return &Endpoint{Host: DefaultHost, From: "default"}, nil
	}
	// Contexts live in contexts/meta/<sha256 of the name>/meta.json.
	sum := sha256.Sum256([]byte(name))
	dir := hex.EncodeToString(sum[:])
	b, err := os.ReadFile(filepath.Join(configDir, "contexts", "meta", dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("docker context %q: %v", name, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, fmt.Errorf("docker context %q: %v", name, err)
	}
	ep, ok := meta.Endpoints["docker"]
	if !ok || ep.Host == "" {
		return nil, fmt.Errorf("docker context %q has no docker endpoint", name)
	}
	e := &Endpoint{Host: ep.Host, From: "context " + name}
	tlsDir := filepath.Join(configDir, "contexts", "tls", dir, "docker")
	if _, err := os.Stat(tlsDir); err == nil {
		e.CertPath, e.TLSVerify = tlsDir, !ep.SkipTLSVerify
	}
	return e, nil
}

// Local reports whether the daemon is on this machine: a unix socket.
func (e *Endpoint) Local() bool { return strings.HasPrefix(e.Host, "unix://") }

// Client calls the API.
type Client struct {
	http *http.Client
	base string // http://docker or https://host:port
	ep   *Endpoint
}

// New connects to e (lazily: nothing is dialled until the first call).
func New(e *Endpoint) (*Client, error) {
	u, err := url.Parse(e.Host)
	if err != nil {
		return nil, fmt.Errorf("daemon address %q: %v", e.Host, err)
	}
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	c := &Client{ep: e}
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", path)
		}
		c.base = "http://docker"
	case "tcp", "http", "https":
		scheme := "http"
		if e.TLSVerify || e.CertPath != "" || u.Scheme == "https" {
			scheme = "https"
			conf, err := tlsConfig(e)
			if err != nil {
				return nil, err
			}
			tr.TLSClientConfig = conf
		}
		c.base = scheme + "://" + u.Host
	default:
		return nil, fmt.Errorf("daemon address %q: only unix:// and tcp:// are supported (for ssh://, forward the socket with ssh -L and use unix://)", e.Host)
	}
	c.http = &http.Client{Transport: tr}
	return c, nil
}

func tlsConfig(e *Endpoint) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !e.TLSVerify}
	if e.CertPath == "" {
		return conf, nil
	}
	if ca, err := os.ReadFile(filepath.Join(e.CertPath, "ca.pem")); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		conf.RootCAs = pool
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(e.CertPath, "cert.pem"), filepath.Join(e.CertPath, "key.pem"))
	if err == nil {
		conf.Certificates = []tls.Certificate{cert}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client certificate in %s: %v", e.CertPath, err)
	}
	return conf, nil
}

// Endpoint is the daemon this client talks to.
func (c *Client) Endpoint() *Endpoint { return c.ep }

// apiError is the daemon's {"message": ...} body.
type apiError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Docker daemon at %s: %v", c.ep.Host, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e apiError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return nil, &DaemonError{Status: resp.StatusCode, Message: e.Message}
	}
	return resp, nil
}

// DaemonError is an error answer from the daemon.
type DaemonError struct {
	Status  int
	Message string
}

func (e *DaemonError) Error() string {
	return fmt.Sprintf("the daemon answered %d: %s", e.Status, e.Message)
}

// IndexConfig is one registry's settings in /info.
type IndexConfig struct {
	Name     string   `json:"Name"`
	Mirrors  []string `json:"Mirrors"`
	Secure   bool     `json:"Secure"`
	Official bool     `json:"Official"`
}

// Info is the part of /info pullwhy uses.
type Info struct {
	ServerVersion   string      `json:"ServerVersion"`
	OperatingSystem string      `json:"OperatingSystem"`
	OSType          string      `json:"OSType"`
	Architecture    string      `json:"Architecture"`
	Name            string      `json:"Name"`
	Driver          string      `json:"Driver"`
	DriverStatus    [][2]string `json:"DriverStatus"`
	SecurityOptions []string    `json:"SecurityOptions"`
	HTTPProxy       string      `json:"HttpProxy"`
	HTTPSProxy      string      `json:"HttpsProxy"`
	NoProxy         string      `json:"NoProxy"`
	RegistryConfig  struct {
		InsecureRegistryCIDRs []string               `json:"InsecureRegistryCIDRs"`
		IndexConfigs          map[string]IndexConfig `json:"IndexConfigs"`
		Mirrors               []string               `json:"Mirrors"`
	} `json:"RegistryConfig"`
}

// ContainerdStore reports whether images are kept in containerd's store
// (the default since Docker 29), which resolves and retries differently from
// the classic one.
func (i *Info) ContainerdStore() bool {
	for _, kv := range i.DriverStatus {
		if kv[0] == "driver-type" && strings.Contains(kv[1], "snapshotter") {
			return true
		}
	}
	return false
}

// Rootless reports whether the daemon runs rootless: its own network
// namespace, and its settings under ~/.config/docker.
func (i *Info) Rootless() bool {
	for _, o := range i.SecurityOptions {
		if strings.Contains(o, "name=rootless") {
			return true
		}
	}
	return false
}

// DockerDesktop reports whether the daemon runs in Docker Desktop's VM.
func (i *Info) DockerDesktop() bool {
	return strings.Contains(i.OperatingSystem, "Docker Desktop")
}

// Info reads /info.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	resp, err := c.do(ctx, "GET", "/info", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var i Info
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&i); err != nil {
		return nil, fmt.Errorf("reading /info: %v", err)
	}
	return &i, nil
}

// Event is one line of a pull's progress.
type Event struct {
	Status         string `json:"status"`
	ID             string `json:"id"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	// At is when the line arrived (the stream carries no times).
	At time.Time `json:"-"`
}

// Pull asks the daemon to pull image (name and tag or digest), with
// registryAuth as the X-Registry-Auth header (base64url JSON, or empty), and
// calls fn for each progress line as it arrives. It returns the daemon's
// error, if the pull failed.
func (c *Client) Pull(ctx context.Context, name, reference, platform, registryAuth string, fn func(Event)) error {
	q := url.Values{"fromImage": {name}}
	if strings.Contains(reference, ":") { // a digest (tags have no colon)
		q.Set("fromImage", name+"@"+reference)
	} else {
		q.Set("tag", reference)
	}
	if platform != "" {
		q.Set("platform", platform)
	}
	h := http.Header{}
	if registryAuth != "" {
		h.Set("X-Registry-Auth", registryAuth)
	}
	resp, err := c.do(ctx, "POST", "/images/create?"+q.Encode(), h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		e.At = time.Now()
		if fn != nil {
			fn(e)
		}
		if e.Error != "" || e.ErrorDetail.Message != "" {
			msg := e.ErrorDetail.Message
			if msg == "" {
				msg = e.Error
			}
			return &DaemonError{Status: 200, Message: msg}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("the daemon's pull stream broke off: %v", err)
	}
	return nil
}
