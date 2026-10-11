// Package engine is a small client for the Docker Engine API.
//
// It only covers the endpoints docker-mop needs and uses the standard
// library alone, so the plugin ships as one dependency-free binary.
package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinAPIVersion is the oldest Engine API docker-mop supports (Docker 20.10).
const MinAPIVersion = "1.41"

// Client talks to one Docker daemon.
type Client struct {
	Endpoint Endpoint

	http    *http.Client
	baseURL string

	once       sync.Once
	apiVersion string
	versionErr error
}

// APIError is a non-2xx response from the daemon.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("docker daemon returned HTTP %d", e.StatusCode)
	}
	return e.Message
}

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsConflict reports whether err is a 409 from the daemon, which it returns
// when an object is in use.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// New creates a client for the given endpoint.
func New(ep Endpoint) (*Client, error) {
	u, err := url.Parse(ep.Host)
	if err != nil {
		return nil, fmt.Errorf("invalid docker host %q: %w", ep.Host, err)
	}

	transport := &http.Transport{
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	c := &Client{Endpoint: ep}

	switch u.Scheme {
	case "unix":
		socket := u.Path
		if socket == "" {
			socket = u.Host
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		c.baseURL = "http://docker"
	case "tcp", "http", "https":
		scheme := "http"
		useTLS := ep.TLS || u.Scheme == "https"
		if useTLS {
			cfg, err := tlsConfig(ep)
			if err != nil {
				return nil, err
			}
			transport.TLSClientConfig = cfg
			scheme = "https"
		}
		host := u.Host
		if u.Port() == "" {
			// Docker's default ports, as the docker CLI uses them.
			port := "2375"
			if useTLS {
				port = "2376"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
		c.baseURL = scheme + "://" + host
	default:
		return nil, fmt.Errorf("docker host %q: the %q scheme is not supported yet (use unix:// or tcp://)", ep.Host, u.Scheme)
	}

	c.http = &http.Client{Transport: transport}
	return c, nil
}

func tlsConfig(ep Endpoint) (*tls.Config, error) {
	// Certificates are only left unchecked when TLS was asked for without
	// verification (--tls / DOCKER_TLS, or a context with SkipTLSVerify),
	// as with the docker CLI. A plain https:// host is always verified.
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: ep.TLS && !ep.TLSVerify}
	if ep.CertPath == "" {
		return cfg, nil
	}
	caPEM, err := os.ReadFile(filepath.Join(ep.CertPath, "ca.pem"))
	if err == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no certificates found in %s", filepath.Join(ep.CertPath, "ca.pem"))
		}
		cfg.RootCAs = pool
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(ep.CertPath, "cert.pem"), filepath.Join(ep.CertPath, "key.pem"))
	if err == nil {
		cfg.Certificates = []tls.Certificate{cert}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	return cfg, nil
}

// APIVersion returns the API version used for requests: the daemon's own
// version, checked against MinAPIVersion.
func (c *Client) APIVersion(ctx context.Context) (string, error) {
	c.once.Do(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/_ping", nil)
		if err != nil {
			c.versionErr = err
			return
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.versionErr = c.connError(err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)

		v := resp.Header.Get("API-Version")
		if v == "" {
			c.versionErr = fmt.Errorf("the daemon at %s did not report an API version", c.Endpoint.Host)
			return
		}
		if CompareVersions(v, MinAPIVersion) < 0 {
			c.versionErr = fmt.Errorf("docker daemon API %s is too old; docker-mop needs %s or newer (Docker 20.10+)", v, MinAPIVersion)
			return
		}
		c.apiVersion = v
	})
	return c.apiVersion, c.versionErr
}

func (c *Client) connError(err error) error {
	return fmt.Errorf("cannot connect to the Docker daemon at %s (from %s): %w", c.Endpoint.Host, c.Endpoint.Source, err)
}

// Request is one API call.
type Request struct {
	Method      string
	Path        string
	Query       url.Values
	Body        io.Reader
	JSON        any // marshalled as the body when set
	ContentType string
}

// Do sends a request and returns the raw response. The caller closes the body.
// Non-2xx responses are turned into *APIError.
func (c *Client) Do(ctx context.Context, r Request) (*http.Response, error) {
	version, err := c.APIVersion(ctx)
	if err != nil {
		return nil, err
	}

	body := r.Body
	contentType := r.ContentType
	if r.JSON != nil {
		data, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
		contentType = "application/json"
	}

	u := c.baseURL + "/v" + version + r.Path
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.connError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}
	return resp, nil
}

// Call sends a request and decodes a JSON response into out (if non-nil).
func (c *Client) Call(ctx context.Context, r Request, out any) error {
	resp, err := c.Do(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", r.Method, r.Path, err)
	}
	return nil
}

func readAPIError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &body) == nil && body.Message != "" {
		msg = body.Message
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg}
}

// CompareVersions compares dotted numeric versions such as "1.41" and "1.9".
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
