// Package probe walks one endpoint of the plan the way the daemon will:
// GET /v2/, a token if the registry asks for one, then a HEAD of the
// manifest. Every request is timed by phase (DNS, connect, TLS, waiting),
// and every failure is given a kind the diagnosis can act on.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/creds"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
)

// Accept is what the daemon asks for when it resolves a reference.
const Accept = "application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, */*"

// Kinds of failure.
const (
	KindTimeout          = "timeout"
	KindRefused          = "refused"
	KindReset            = "reset"
	KindDNS              = "dns"
	KindTLSAuthority     = "tls-unknown-authority"
	KindTLSExpired       = "tls-expired"
	KindTLSName          = "tls-wrong-name"
	KindTLSOther         = "tls"
	KindHTTPOnHTTPS      = "http-on-https"
	KindProxy            = "proxy"
	KindNotRegistry      = "not-a-registry"
	KindUnauthorized     = "unauthorized"
	KindNotFound         = "not-found"
	KindRateLimited      = "rate-limited"
	KindServer           = "server-error"
	KindOther            = "other"
	KindTokenUnavailable = "token"
)

// Options say how to probe.
type Options struct {
	// Timeout bounds each request (default 10 s).
	Timeout time.Duration
	Auth    *creds.Auth
	// Containerd: the daemon uses containerd's resolver, which adds
	// ?ns=docker.io when asking a mirror.
	Containerd bool
	// Proxy picks the daemon's proxy for any URL (tokens and blobs may be
	// on other hosts); nil is direct.
	Proxy     func(*url.URL) (*url.URL, error)
	UserAgent string
}

// Phase is one timed part of a request.
type Phase struct {
	Name string        `json:"name"` // dns, connect, tls, wait
	Took time.Duration `json:"took_ns"`
	Info string        `json:"info,omitempty"`
}

// Request is one request made.
type Request struct {
	What    string        `json:"what"` // ping, token, manifest
	Method  string        `json:"method"`
	URL     string        `json:"url"`
	Status  int           `json:"status,omitempty"`
	Took    time.Duration `json:"took_ns"`
	Phases  []Phase       `json:"phases,omitempty"`
	Reused  bool          `json:"reused_connection,omitempty"`
	Kind    string        `json:"kind,omitempty"`
	Error   string        `json:"error,omitempty"`
	Message string        `json:"message,omitempty"` // a registry error body's message
}

// RateLimit is Docker Hub's view of the pulls left.
type RateLimit struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	Window    int    `json:"window_s"`
	Source    string `json:"source,omitempty"` // the address counted
}

// Result is what one endpoint did.
type Result struct {
	Endpoint  plan.Endpoint `json:"-"`
	Base      string        `json:"endpoint"`
	Requests  []Request     `json:"requests"`
	OK        bool          `json:"ok"`
	Kind      string        `json:"kind,omitempty"` // the first failure's kind
	Digest    string        `json:"digest,omitempty"`
	MediaType string        `json:"media_type,omitempty"`
	RateLimit *RateLimit    `json:"rate_limit,omitempty"`
	// Auth says how the registry was asked: anonymous, or which credentials.
	Auth string `json:"auth"`
	// TLSSkipped: the certificate did not verify, and the daemon accepts that
	// here (an insecure registry).
	TLSSkipped string `json:"tls_not_verified,omitempty"`
	// Token is the bearer token got (kept for --deep, never printed).
	Token string `json:"-"`
	// Basic: the registry asked for basic auth.
	Basic bool `json:"-"`
}

// Client builds the HTTP client for ep: the daemon's proxy, the system
// roots plus certs.d's CA files, and certs.d's client certificates.
func Client(ep plan.Endpoint, o Options, skipVerify bool) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	conf := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, InsecureSkipVerify: skipVerify}
	for _, f := range ep.CertFiles {
		path := filepath.Join(ep.CertsDir, f)
		switch {
		case strings.HasSuffix(f, ".crt"):
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if !roots.AppendCertsFromPEM(b) {
				return nil, fmt.Errorf("%s holds no PEM certificate", path)
			}
		case strings.HasSuffix(f, ".cert"):
			key := strings.TrimSuffix(path, ".cert") + ".key"
			c, err := tls.LoadX509KeyPair(path, key)
			if err != nil {
				return nil, fmt.Errorf("client certificate %s: %v", path, err)
			}
			conf.Certificates = append(conf.Certificates, c)
		}
	}
	proxy := o.Proxy
	if proxy == nil {
		proxy = func(*url.URL) (*url.URL, error) { return nil, nil }
	}
	tr := &http.Transport{
		Proxy:                 func(r *http.Request) (*url.URL, error) { return proxy(r.URL) },
		DialContext:           (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       conf,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 0, // the request's context bounds it
		IdleConnTimeout:       30 * time.Second,
	}
	// Redirects are followed as the daemon follows them, without the
	// Authorization header once scheme, host or port change (Go itself only
	// drops it for another host name).
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}, nil
}

// Endpoint probes one endpoint for r.
func Endpoint(ctx context.Context, ep plan.Endpoint, r *ref.Ref, o Options) *Result {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	res := &Result{Endpoint: ep, Base: ep.Base, Auth: "anonymous"}
	c, err := Client(ep, o, false)
	if err != nil {
		res.fail(Request{What: "setup", Kind: KindOther, Error: err.Error()})
		return res
	}
	ping, resp := do(ctx, c, o, "ping", "GET", ep.Base+"/v2/", nil)
	if ping.Kind != "" && strings.HasPrefix(ping.Kind, "tls") && ep.Insecure && strings.HasPrefix(ep.Base, "https://") {
		// The daemon does not verify certificates of insecure registries.
		res.TLSSkipped = ping.Error
		res.Requests = append(res.Requests, ping)
		if c, err = Client(ep, o, true); err == nil {
			ping, resp = do(ctx, c, o, "ping", "GET", ep.Base+"/v2/", nil)
		}
	}
	if ping.Kind == "" && resp != nil && viaProxy(o, ep.Base) && proxyAnswered(resp) {
		// Plain HTTP through a proxy: the proxy's own refusal comes back as
		// an answer.
		ping.Kind = KindProxy
		ping.Error = "the proxy answered " + resp.Status
	}
	if ping.Kind != "" {
		res.fail(ping)
		return res
	}
	challenge := ""
	if resp != nil {
		challenge = resp.Header.Get("Www-Authenticate")
	}
	switch {
	case ping.Status == http.StatusOK:
	case ping.Status == http.StatusUnauthorized && challenge != "":
	case ping.Status == http.StatusNotFound:
		ping.Kind = KindNotRegistry
		res.fail(ping)
		return res
	case ping.Status >= 500:
		ping.Kind = KindServer
		res.fail(ping)
		return res
	case ping.Status == http.StatusUnauthorized:
	default:
		ping.Kind = KindNotRegistry
		res.fail(ping)
		return res
	}
	res.Requests = append(res.Requests, ping)

	header := http.Header{"Accept": {Accept}}
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "bearer":
		tok, req := token(ctx, c, o, params, r.Repo)
		res.Requests = append(res.Requests, req)
		if req.Kind != "" {
			res.Kind = req.Kind
			return res
		}
		res.Token = tok
		header.Set("Authorization", "Bearer "+tok)
	case "basic":
		res.Basic = true
		if !o.Auth.Anonymous() && o.Auth.Username != "" {
			res.Auth = "basic, from " + o.Auth.Source
			req, _ := http.NewRequest("GET", "/", nil)
			req.SetBasicAuth(o.Auth.Username, o.Auth.Password)
			header.Set("Authorization", req.Header.Get("Authorization"))
		}
	}
	if res.Auth == "anonymous" && strings.EqualFold(scheme, "bearer") && !o.Auth.Anonymous() {
		res.Auth = "token, with credentials from " + o.Auth.Source
	}
	u := ep.Base + "/v2/" + r.Repo + "/manifests/" + r.Reference()
	if ep.Mirror && o.Containerd {
		u += "?ns=docker.io"
	}
	m, mresp := do(ctx, c, o, "manifest", "HEAD", u, header)
	if mresp != nil {
		res.RateLimit = rateLimit(mresp.Header)
	}
	switch {
	case m.Kind != "":
	case m.Status == http.StatusOK:
		res.OK = true
		res.Digest = mresp.Header.Get("Docker-Content-Digest")
		res.MediaType = strings.TrimSpace(strings.Split(mresp.Header.Get("Content-Type"), ";")[0])
	case m.Status == http.StatusUnauthorized || m.Status == http.StatusForbidden:
		m.Kind = KindUnauthorized
	case m.Status == http.StatusNotFound:
		m.Kind = KindNotFound
	case m.Status == http.StatusTooManyRequests:
		m.Kind = KindRateLimited
	case m.Status >= 500:
		m.Kind = KindServer
	default:
		m.Kind = KindOther
	}
	if m.Kind != "" {
		res.fail(m)
		return res
	}
	res.Requests = append(res.Requests, m)
	return res
}

func (res *Result) fail(r Request) {
	res.Requests = append(res.Requests, r)
	if res.Kind == "" {
		res.Kind = r.Kind
	}
}

// do makes one request, timing it by phase.
func do(ctx context.Context, c *http.Client, o Options, what, method, u string, header http.Header) (Request, *http.Response) {
	return doFull(ctx, c, o, what, method, u, header, "", nil)
}

// doFull is do with a request body (a form) and a reader for a good
// answer's body.
func doFull(ctx context.Context, c *http.Client, o Options, what, method, u string, header http.Header, form string, read func(io.Reader)) (Request, *http.Response) {
	rec := Request{What: what, Method: method, URL: u}
	if what == "token" {
		rec.URL = redact(u)
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var (
		start                       = time.Now()
		dnsStart, connStart, tlsStt time.Time
		wrote                       time.Time
	)
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(i httptrace.DNSDoneInfo) {
			var addrs []string
			for _, a := range i.Addrs {
				addrs = append(addrs, a.IP.String())
			}
			rec.Phases = append(rec.Phases, Phase{Name: "dns", Took: time.Since(dnsStart), Info: strings.Join(addrs, " ")})
		},
		ConnectStart: func(_, addr string) { connStart = time.Now() },
		ConnectDone: func(_, addr string, err error) {
			if err == nil {
				rec.Phases = append(rec.Phases, Phase{Name: "connect", Took: time.Since(connStart), Info: addr})
			}
		},
		TLSHandshakeStart: func() { tlsStt = time.Now() },
		TLSHandshakeDone: func(s tls.ConnectionState, err error) {
			if err == nil {
				rec.Phases = append(rec.Phases, Phase{Name: "tls", Took: time.Since(tlsStt), Info: tls.VersionName(s.Version)})
			}
		},
		GotConn:      func(i httptrace.GotConnInfo) { rec.Reused = i.Reused },
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = time.Now() },
		GotFirstResponseByte: func() {
			if !wrote.IsZero() {
				rec.Phases = append(rec.Phases, Phase{Name: "wait", Took: time.Since(wrote)})
			}
		},
	}
	var body io.Reader
	if form != "" {
		body = strings.NewReader(form)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, u, body)
	if err != nil {
		rec.Kind, rec.Error = KindOther, err.Error()
		return rec, nil
	}
	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	resp, err := c.Do(req)
	rec.Took = time.Since(start)
	if err != nil {
		rec.Kind, rec.Error = Classify(err), RedactQueries(err.Error())
		var ua x509.UnknownAuthorityError
		if errors.As(err, &ua) && ua.Cert != nil {
			rec.Message = "the certificate presented was issued by " + ua.Cert.Issuer.String()
		}
		if o.Proxy != nil && rec.Kind == KindOther {
			// A proxy refusing CONNECT surfaces as its bare status text.
			if pu, _ := o.Proxy(req.URL); pu != nil && proxyStatus(err) != "" {
				rec.Kind = KindProxy
				rec.Error = fmt.Sprintf("the proxy %s answered %q to CONNECT %s", pu.Host, proxyStatus(err), req.URL.Host)
			}
		}
		return rec, nil
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	if resp.StatusCode >= 400 && method != "HEAD" {
		rec.Message = errorMessage(io.LimitReader(resp.Body, 64<<10))
	} else if read != nil {
		read(resp.Body)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return rec, resp
}

// errorMessage reads a registry's {"errors":[{"code","message"}]} body.
func errorMessage(r io.Reader) string {
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Details string `json:"details"`
	}
	b, _ := io.ReadAll(r)
	if json.Unmarshal(b, &body) == nil && len(body.Errors) > 0 {
		return body.Errors[0].Code + ": " + body.Errors[0].Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Classify gives an error its kind.
func Classify(err error) string {
	var (
		dnsErr   *net.DNSError
		ua       x509.UnknownAuthorityError
		ci       x509.CertificateInvalidError
		hn       x509.HostnameError
		recErr   tls.RecordHeaderError
		tlsAlert tls.AlertError
		opErr    *net.OpError
	)
	switch {
	case errors.As(err, &opErr) && opErr.Op == "proxyconnect":
		return KindProxy
	case errors.As(err, &dnsErr):
		return KindDNS // a lookup that timed out too
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return KindTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return KindRefused
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		return KindReset
	case errors.As(err, &ua):
		return KindTLSAuthority
	case errors.As(err, &ci):
		if ci.Reason == x509.Expired {
			return KindTLSExpired
		}
		return KindTLSOther
	case errors.As(err, &hn):
		return KindTLSName
	case errors.Is(err, http.ErrSchemeMismatch) || errors.As(err, &recErr):
		return KindHTTPOnHTTPS
	case errors.As(err, &tlsAlert):
		return KindTLSOther
	case strings.Contains(err.Error(), "tls: failed to verify certificate"):
		return KindTLSOther
	}
	return KindOther
}

// viaProxy reports whether a request to base goes through a proxy.
func viaProxy(o Options, base string) bool {
	if o.Proxy == nil {
		return false
	}
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	pu, _ := o.Proxy(u)
	return pu != nil
}

// proxyAnswered reports whether an answer came from a proxy rather than the
// registry: 407, or a refusal carrying a proxy's headers.
func proxyAnswered(resp *http.Response) bool {
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return true
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return resp.Header.Get("Proxy-Authenticate") != "" || resp.Header.Get("X-Squid-Error") != "" ||
			resp.Header.Get("Via") != "" && resp.Header.Get("Docker-Distribution-Api-Version") == ""
	}
	return false
}

// proxyStatus finds an HTTP status text at the end of err, as Go reports a
// proxy's refusal of CONNECT.
func proxyStatus(err error) string {
	msg := err.Error()
	for code := 400; code < 600; code++ {
		if t := http.StatusText(code); t != "" && strings.HasSuffix(msg, ": "+t) {
			return fmt.Sprintf("%d %s", code, t)
		}
	}
	return ""
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parseChallenge reads `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(v) && v[i] != '"'; i++ {
				if v[i] == '\\' && i+1 < len(v) {
					i++
				}
				b.WriteByte(v[i])
			}
			params[k] = b.String()
			if i >= len(v) {
				break
			}
			rest = v[i+1:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			params[k] = strings.TrimSpace(val)
			rest = after
		}
	}
	return scheme, params
}

// token gets a pull token from the challenge's realm.
func token(ctx context.Context, c *http.Client, o Options, params map[string]string, repo string) (string, Request) {
	realm := params["realm"]
	ru, err := url.Parse(realm)
	if err != nil || ru.Host == "" {
		return "", Request{What: "token", Kind: KindTokenUnavailable, Error: fmt.Sprintf("the registry's token realm %q is not a URL", realm)}
	}
	q := ru.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+repo+":pull")
	ru.RawQuery = q.Encode()
	header := http.Header{}
	if a := o.Auth; !a.Anonymous() && a.Username != "" {
		req, _ := http.NewRequest("GET", "/", nil)
		req.SetBasicAuth(a.Username, a.Password)
		header.Set("Authorization", req.Header.Get("Authorization"))
	}
	var (
		rec  Request
		body []byte
		read = func(r io.Reader) { body, _ = io.ReadAll(io.LimitReader(r, 1<<20)) }
	)
	if a := o.Auth; !a.Anonymous() && a.IdentityToken != "" {
		// An identity token is an OAuth2 refresh token: traded in a POST
		// (the distribution token spec), as the daemon does.
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.IdentityToken}, "client_id": {"pullwhy"},
			"scope": {"repository:" + repo + ":pull"}}
		if s := params["service"]; s != "" {
			form.Set("service", s)
		}
		ru.RawQuery = ""
		rec, _ = doFull(ctx, c, o, "token", "POST", ru.String(), nil, form.Encode(), read)
	} else {
		rec, _ = doFull(ctx, c, o, "token", "GET", ru.String(), header, "", read)
	}
	if rec.Kind != "" {
		return "", rec
	}
	if rec.Status == http.StatusUnauthorized || rec.Status == http.StatusForbidden {
		rec.Kind = KindUnauthorized
		return "", rec
	}
	if rec.Status != http.StatusOK {
		rec.Kind = KindTokenUnavailable
		return "", rec
	}
	var tb struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &tb) != nil || tb.Token == "" && tb.AccessToken == "" {
		rec.Kind, rec.Error = KindTokenUnavailable, "the token answer has no token"
		return "", rec
	}
	if tb.Token == "" {
		tb.Token = tb.AccessToken
	}
	return tb.Token, rec
}

var queryRe = regexp.MustCompile(`(https?://[^\s"?]+)\?[^\s",)]*`)

// RedactQueries drops the query of every URL in s: a CDN's signed URL or a
// token request's parameters must not end up in a report.
func RedactQueries(s string) string { return queryRe.ReplaceAllString(s, "$1?…") }

// redact drops a query (a token request's scope is fine, but keep URLs
// short and free of anything secret).
func redact(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// rateLimit reads Docker Hub's headers: ratelimit-limit: 100;w=3600.
func rateLimit(h http.Header) *RateLimit {
	lim, w := splitLimit(h.Get("Ratelimit-Limit"))
	rem, _ := splitLimit(h.Get("Ratelimit-Remaining"))
	if lim < 0 || rem < 0 {
		return nil
	}
	return &RateLimit{Limit: lim, Remaining: rem, Window: w, Source: h.Get("Docker-Ratelimit-Source")}
}

func splitLimit(v string) (int, int) {
	if v == "" {
		return -1, 0
	}
	n, rest, _ := strings.Cut(v, ";")
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil {
		return -1, 0
	}
	w := 0
	if _, ws, ok := strings.Cut(rest, "w="); ok {
		w, _ = strconv.Atoi(strings.TrimSpace(ws))
	}
	return count, w
}
