// Package upstream sends queries to AdGuard Home's DoH endpoint (RFC 8484),
// naming the real client in X-Forwarded-For.
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/dnsmsg"
)

const contentType = "application/dns-message"

// maxAnswer is the largest DNS message (it has a 16-bit length on TCP).
const maxAnswer = 65535

// Options say where and how to send.
type Options struct {
	URL        string
	Timeout    time.Duration
	CAFile     string
	ServerName string
	// MaxConns bounds the connections to AdGuard Home.
	MaxConns int
	// UserAgent goes with each request.
	UserAgent string
}

// Client sends queries.
type Client struct {
	url     *url.URL
	http    *http.Client
	timeout time.Duration
	ua      string
}

// Kinds of errors, for metrics.
var (
	ErrTimeout = errors.New("AdGuard Home did not answer in time")
	ErrStatus  = errors.New("AdGuard Home answered with an HTTP error")
	ErrAnswer  = errors.New("AdGuard Home's answer is not a DNS message")
)

// New makes a Client.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.URL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	hc, err := HTTPClient(o)
	if err != nil {
		return nil, err
	}
	return &Client{url: u, http: hc, timeout: o.Timeout, ua: o.UserAgent}, nil
}

// HTTPClient makes the HTTP client New uses: o's TLS settings, a bounded
// pool, and never a proxy from the environment, since AdGuard Home must see
// xffdns itself as the sender, a trusted proxy.
func HTTPClient(o Options) (*http.Client, error) {
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.ServerName}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificates", o.CAFile)
		}
		tlsConf.RootCAs = pool
	}
	if o.MaxConns <= 0 {
		o.MaxConns = 64
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     tlsConf,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        o.MaxConns,
		MaxIdleConnsPerHost: o.MaxConns,
		MaxConnsPerHost:     o.MaxConns,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}, nil
}

// URL is where queries for clientID go.
func (c *Client) URL(clientID string) string {
	u := *c.url
	if clientID != "" {
		u.Path += "/" + clientID
		u.RawPath = ""
	}
	return u.String()
}

// Exchange sends query on behalf of client (and clientID, if set) and
// returns the answer, with the query's ID put back.
func (c *Client) Exchange(ctx context.Context, query []byte, client netip.Addr, clientID string) ([]byte, error) {
	if len(query) < dnsmsg.HeaderLen {
		return nil, dnsmsg.ErrShort
	}
	id := dnsmsg.ID(query)
	body := append([]byte(nil), query...)
	dnsmsg.SetID(body, 0) // RFC 8484 4.1
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL(clientID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", contentType)
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	// One bare address: AdGuard Home takes the first entry, and ignores
	// [v6] and ip:port forms.
	req.Header.Set("X-Forwarded-For", client.Unmap().WithZone("").String())
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, ErrTimeout
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: %s", ErrStatus, resp.Status)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != contentType {
		return nil, fmt.Errorf("%w: content type %q", ErrAnswer, resp.Header.Get("Content-Type"))
	}
	ans, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, ErrTimeout
		}
		return nil, err
	}
	if len(ans) < dnsmsg.HeaderLen || len(ans) > maxAnswer {
		return nil, fmt.Errorf("%w: %d bytes", ErrAnswer, len(ans))
	}
	dnsmsg.SetID(ans, id)
	return ans, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Close drops idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }
