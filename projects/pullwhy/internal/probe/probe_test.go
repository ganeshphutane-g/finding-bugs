package probe

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/creds"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
)

const digest = "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"

// registry is a fake registry with token auth. Repos named "missing" are
// not found; "limited" is rate limited; "private" needs credentials.
type registry struct {
	mu      sync.Mutex
	srv     *httptest.Server
	basic   bool // ask for basic auth instead of a token
	gotNS   string
	tokens  []string // token requests: method user scope
	limited bool
}

func (g *registry) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			if g.basic {
				w.Header().Set("Www-Authenticate", `Basic realm="Registry Realm"`)
			} else {
				w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake.registry"`, g.srv.URL))
			}
			w.WriteHeader(401)
			return
		}
		g.mu.Lock()
		g.gotNS = r.URL.Query().Get("ns")
		g.mu.Unlock()
		auth := r.Header.Get("Authorization")
		ok := auth == "Bearer t-anon" || auth == "Bearer t-user"
		if g.basic {
			u, p, _ := r.BasicAuth()
			ok = u == "bob" && p == "pw"
		}
		w.Header().Set("Ratelimit-Limit", "100;w=3600")
		w.Header().Set("Ratelimit-Remaining", "7;w=3600")
		w.Header().Set("Docker-Ratelimit-Source", "203.0.113.4")
		switch {
		case !ok || strings.Contains(r.URL.Path, "/private/") && auth != "Bearer t-user":
			w.WriteHeader(401)
		case strings.Contains(r.URL.Path, "/missing/"):
			w.WriteHeader(404)
		case strings.Contains(r.URL.Path, "/limited/"):
			w.Header().Set("Ratelimit-Remaining", "0;w=3600")
			w.WriteHeader(429)
		default:
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
		}
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		user, pass, _ := r.BasicAuth()
		g.mu.Lock()
		g.tokens = append(g.tokens, r.Method+" "+user+" "+r.Form.Get("scope"))
		g.mu.Unlock()
		switch {
		case r.Method == "POST" && r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "refresh-ok":
			fmt.Fprint(w, `{"access_token":"t-user"}`)
		case r.Method == "POST":
			w.WriteHeader(401)
		case user == "bob" && pass == "pw":
			fmt.Fprint(w, `{"token":"t-user"}`)
		case user != "":
			w.WriteHeader(401)
			fmt.Fprint(w, `{"details":"incorrect username or password"}`)
		default:
			fmt.Fprint(w, `{"token":"t-anon"}`)
		}
	})
	return mux
}

func newRegistry(t *testing.T, tlsOn bool) *registry {
	g := &registry{}
	if tlsOn {
		g.srv = httptest.NewTLSServer(g.handler())
	} else {
		g.srv = httptest.NewServer(g.handler())
	}
	t.Cleanup(g.srv.Close)
	return g
}

func endpoint(base string) plan.Endpoint {
	u, _ := url.Parse(base)
	return plan.Endpoint{Base: base, Host: u.Host}
}

func mustRef(s string) *ref.Ref {
	r, err := ref.Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

func TestTokenFlow(t *testing.T) {
	g := newRegistry(t, false)
	ctx := context.Background()
	res := Endpoint(ctx, endpoint(g.srv.URL), mustRef("example.com/team/app:1"), Options{})
	if !res.OK || res.Digest != digest || res.MediaType != "application/vnd.oci.image.index.v1+json" || res.Auth != "anonymous" {
		t.Fatalf("%+v", res)
	}
	if len(res.Requests) != 3 || res.Requests[0].What != "ping" || res.Requests[1].What != "token" || res.Requests[2].Method != "HEAD" {
		t.Fatalf("%+v", res.Requests)
	}
	if rl := res.RateLimit; rl == nil || rl.Limit != 100 || rl.Remaining != 7 || rl.Window != 3600 || rl.Source != "203.0.113.4" {
		t.Fatalf("%+v", res.RateLimit)
	}
	if g.tokens[0] != "GET  repository:team/app:pull" {
		t.Fatal(g.tokens)
	}
	// Phases: a fresh connection has connect and wait.
	names := ""
	for _, p := range res.Requests[0].Phases {
		names += p.Name + " "
	}
	if !strings.Contains(names, "connect") || !strings.Contains(names, "wait") {
		t.Fatal(names)
	}
	// With credentials, and with an identity token (a refresh token POST).
	res = Endpoint(ctx, endpoint(g.srv.URL), mustRef("example.com/private/app"), Options{Auth: &creds.Auth{Username: "bob", Password: "pw", Source: "config.json auths"}})
	if !res.OK || !strings.Contains(res.Auth, "config.json auths") {
		t.Fatalf("%+v", res)
	}
	res = Endpoint(ctx, endpoint(g.srv.URL), mustRef("example.com/private/app"), Options{Auth: &creds.Auth{IdentityToken: "refresh-ok", Source: "credential helper desktop"}})
	if !res.OK {
		t.Fatalf("%+v", res.Requests)
	}
	// Wrong password: the token request fails as unauthorized.
	res = Endpoint(ctx, endpoint(g.srv.URL), mustRef("example.com/private/app"), Options{Auth: &creds.Auth{Username: "bob", Password: "nope", Source: "x"}})
	if res.OK || res.Kind != KindUnauthorized {
		t.Fatalf("%+v", res)
	}
	// Anonymous on a private repo: the manifest is unauthorized.
	res = Endpoint(ctx, endpoint(g.srv.URL), mustRef("example.com/private/app"), Options{})
	if res.Kind != KindUnauthorized || res.Requests[len(res.Requests)-1].What != "manifest" {
		t.Fatalf("%+v", res)
	}
	for name, kind := range map[string]string{"example.com/missing/x": KindNotFound, "example.com/limited/x": KindRateLimited} {
		res := Endpoint(ctx, endpoint(g.srv.URL), mustRef(name), Options{})
		if res.Kind != kind {
			t.Errorf("%s: %+v", name, res)
		}
		if kind == KindRateLimited && (res.RateLimit == nil || res.RateLimit.Remaining != 0) {
			t.Errorf("no rate limit read from a 429: %+v", res.RateLimit)
		}
	}
}

func TestMirrorNS(t *testing.T) {
	g := newRegistry(t, false)
	ep := endpoint(g.srv.URL)
	ep.Mirror = true
	Endpoint(context.Background(), ep, mustRef("alpine:3"), Options{Containerd: true})
	if g.gotNS != "docker.io" {
		t.Fatal("no ?ns=docker.io on the containerd store", g.gotNS)
	}
	Endpoint(context.Background(), ep, mustRef("alpine:3"), Options{Containerd: false})
	if g.gotNS != "" {
		t.Fatal("?ns= on the classic store")
	}
}

func TestBasic(t *testing.T) {
	g := newRegistry(t, false)
	g.basic = true
	res := Endpoint(context.Background(), endpoint(g.srv.URL), mustRef("example.com/x"), Options{Auth: &creds.Auth{Username: "bob", Password: "pw", Source: "s"}})
	if !res.OK || !res.Basic || !strings.HasPrefix(res.Auth, "basic") {
		t.Fatalf("%+v", res)
	}
	res = Endpoint(context.Background(), endpoint(g.srv.URL), mustRef("example.com/x"), Options{})
	if res.Kind != KindUnauthorized {
		t.Fatalf("%+v", res)
	}
}

func TestTLS(t *testing.T) {
	g := newRegistry(t, true)
	ctx := context.Background()
	ep := endpoint(g.srv.URL)
	res := Endpoint(ctx, ep, mustRef("example.com/x"), Options{})
	if res.Kind != KindTLSAuthority {
		t.Fatalf("%+v", res)
	}
	// certs.d's ca.crt makes it trusted.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o644)
	ep.CertsDir, ep.CertFiles = dir, []string{"ca.crt"}
	if res := Endpoint(ctx, ep, mustRef("example.com/x"), Options{}); !res.OK {
		t.Fatalf("%+v", res)
	}
	// An insecure registry: the daemon does not verify, so neither does the
	// probe, but it says so.
	ep = endpoint(g.srv.URL)
	ep.Insecure = true
	res = Endpoint(ctx, ep, mustRef("example.com/x"), Options{})
	if !res.OK || res.TLSSkipped == "" {
		t.Fatalf("%+v", res)
	}
	// HTTPS to a plain HTTP registry.
	plain := newRegistry(t, false)
	res = Endpoint(ctx, endpoint(strings.Replace(plain.srv.URL, "http://", "https://", 1)), mustRef("example.com/x"), Options{})
	if res.Kind != KindHTTPOnHTTPS {
		t.Fatalf("%+v", res)
	}
}

func TestNetworkFailures(t *testing.T) {
	ctx := context.Background()
	// Refused: a port nothing listens on.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	if res := Endpoint(ctx, endpoint("http://"+addr), mustRef("example.com/x"), Options{}); res.Kind != KindRefused {
		t.Fatalf("%+v", res)
	}
	// A server that accepts and never answers.
	hang, _ := net.Listen("tcp", "127.0.0.1:0")
	defer hang.Close()
	go func() {
		for {
			c, err := hang.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	res := Endpoint(ctx, endpoint("http://"+hang.Addr().String()), mustRef("example.com/x"), Options{Timeout: 300 * time.Millisecond})
	if res.Kind != KindTimeout || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v", res)
	}
	if res := Endpoint(ctx, endpoint("http://no-such-host.invalid:5000"), mustRef("example.com/x"), Options{}); res.Kind != KindDNS {
		t.Fatalf("%+v", res)
	}
	// Not a registry.
	web := httptest.NewServer(http.NotFoundHandler())
	defer web.Close()
	if res := Endpoint(ctx, endpoint(web.URL), mustRef("example.com/x"), Options{}); res.Kind != KindNotRegistry {
		t.Fatalf("%+v", res)
	}
}

// connectProxy is a CONNECT proxy that logs hosts and can refuse some.
type connectProxy struct {
	mu     sync.Mutex
	hosts  []string
	refuse string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Method+" "+r.Host)
	p.mu.Unlock()
	if r.Method != "CONNECT" {
		// Plain HTTP through the proxy: forward it.
		out, err := http.DefaultTransport.RoundTrip(r)
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
	if p.refuse != "" && strings.HasPrefix(r.Host, p.refuse) {
		w.WriteHeader(403)
		return
	}
	up, err := net.Dial("tcp", r.Host)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	w.WriteHeader(200)
	c, _, _ := w.(http.Hijacker).Hijack()
	go func() { io.Copy(up, c); up.Close() }()
	io.Copy(c, up)
	c.Close()
}

func TestProxy(t *testing.T) {
	g := newRegistry(t, true)
	cp := &connectProxy{}
	px := httptest.NewServer(cp)
	defer px.Close()
	pu, _ := url.Parse(px.URL)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o644)
	ep := endpoint(g.srv.URL)
	ep.CertsDir, ep.CertFiles = dir, []string{"ca.crt"}
	via := func(*url.URL) (*url.URL, error) { return pu, nil }
	res := Endpoint(context.Background(), ep, mustRef("example.com/x"), Options{Proxy: via})
	if !res.OK || len(cp.hosts) == 0 || !strings.HasPrefix(cp.hosts[0], "CONNECT ") {
		t.Fatalf("%+v %v", res, cp.hosts)
	}
	// The proxy refuses the registry.
	cp.refuse = "127.0.0.1"
	res = Endpoint(context.Background(), ep, mustRef("example.com/x"), Options{Proxy: via})
	if res.Kind != KindProxy {
		t.Fatalf("%+v", res)
	}
	// The proxy is down.
	px.Close()
	res = Endpoint(context.Background(), ep, mustRef("example.com/x"), Options{Proxy: via})
	if res.Kind != KindProxy {
		t.Fatalf("%+v", res)
	}
}

func TestChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:a:pull"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:a:pull" {
		t.Fatal(s, p)
	}
	s, p = parseChallenge(`Basic realm=Registry`)
	if s != "Basic" || p["realm"] != "Registry" {
		t.Fatal(s, p)
	}
	if lim, w := splitLimit("100;w=21600"); lim != 100 || w != 21600 {
		t.Fatal(lim, w)
	}
	if lim, _ := splitLimit("x"); lim != -1 {
		t.Fatal(lim)
	}
}
