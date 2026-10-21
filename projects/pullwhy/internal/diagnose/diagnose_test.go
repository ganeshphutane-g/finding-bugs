package diagnose

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/creds"
	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
	"github.com/ganeshphutane-g/pullwhy/internal/pull"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
)

func input(name string, containerd bool, results ...*probe.Result) Input {
	r, _ := ref.Parse(name)
	p := &plan.Plan{Ref: r, Containerd: containerd}
	for _, res := range results {
		p.Endpoints = append(p.Endpoints, res.Endpoint)
	}
	return Input{Info: &engine.Info{}, Local: true, Plan: p, Results: results}
}

func ep(base string, mirror bool) plan.Endpoint {
	u, _ := url.Parse(base)
	e := plan.Endpoint{Base: base, Host: u.Host, Mirror: mirror}
	if u.Host != plan.HubHost {
		e.CertsPath = "/etc/docker/certs.d/" + u.Host
	}
	return e
}

func failed(e plan.Endpoint, kind string) *probe.Result {
	return &probe.Result{Endpoint: e, Base: e.Base, Kind: kind, Requests: []probe.Request{{What: "ping", Method: "GET", URL: e.Base + "/v2/", Kind: kind, Took: 10 * time.Second, Error: "boom"}}}
}

func ok(e plan.Endpoint, digest string) *probe.Result {
	return &probe.Result{Endpoint: e, Base: e.Base, OK: true, Digest: digest, MediaType: "application/vnd.oci.image.index.v1+json"}
}

func TestDeadMirror(t *testing.T) {
	hub := ep("https://registry-1.docker.io", false)
	for _, containerd := range []bool{true, false} {
		in := input("alpine:3", containerd, failed(ep("http://10.255.255.1:5000", true), probe.KindTimeout), ok(hub, "sha256:a"))
		fs := Diagnose(in)
		f := fs[0]
		if f.Severity != Slow || f.Kind != "mirror-timeout" || !strings.Contains(f.Title, "does not answer") || !strings.Contains(f.Fix, "registry-mirrors") {
			t.Fatalf("%+v", fs)
		}
		want := "15 s"
		if containerd {
			want = "30 s"
		}
		if !strings.Contains(strings.Join(f.Evidence, " "), want) {
			t.Fatalf("containerd=%v: %v", containerd, f.Evidence)
		}
		if Worst(fs) != Slow {
			t.Fatal(Worst(fs))
		}
	}
	// With --pull, the slow start is put down to the mirror.
	in := input("alpine:3", true, failed(ep("http://10.255.255.1:5000", true), probe.KindTimeout), ok(hub, "sha256:a"))
	// Two silences before any layer (no layer events at all): one finding,
	// with their sum.
	in.Pull = &pull.Timeline{Gaps: []pull.Gap{{Took: 61 * time.Second, Phase: "before the first layer"}, {Took: 60 * time.Second, Phase: "before the first layer"}}}
	fs := Diagnose(in)
	starts := 0
	for _, f := range fs {
		if f.Kind == "slow-start" {
			starts++
			if !strings.Contains(f.Title, "2m1s") || !strings.Contains(strings.Join(f.Evidence, " "), "waiting for the mirror http://10.255.255.1:5000") {
				t.Fatalf("%+v", f)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("%+v", fs)
	}
	// Every endpoint down: a problem.
	in = input("alpine:3", true, failed(ep("http://10.255.255.1:5000", true), probe.KindTimeout), failed(hub, probe.KindDNS))
	if fs := Diagnose(in); fs[0].Severity != Problem || Worst(fs) != Problem {
		t.Fatalf("%+v", fs)
	}
}

func TestRegistryKinds(t *testing.T) {
	reg := ep("https://reg.example:5443", false)
	for kind, want := range map[string]string{
		probe.KindTLSAuthority: "/etc/docker/certs.d/reg.example:5443/ca.crt",
		probe.KindHTTPOnHTTPS:  "insecure-registries",
		probe.KindProxy:        "no-proxy",
		probe.KindNotFound:     "Check the name",
		probe.KindRateLimited:  "docker login",
		probe.KindRefused:      "firewall",
	} {
		fs := Diagnose(input("reg.example:5443/app:1", true, failed(reg, kind)))
		if fs[0].Severity != Problem || !strings.Contains(fs[0].Fix, want) {
			t.Errorf("%s: %+v", kind, fs[0])
		}
	}
	// A public registry with an unknown CA: interception.
	fs := Diagnose(input("nginx", true, failed(ep("https://registry-1.docker.io", false), probe.KindTLSAuthority)))
	if !strings.Contains(fs[0].Title, "intercepts TLS") || !strings.Contains(fs[0].Fix, "trust store") {
		t.Fatal(fs[0])
	}
	// A ca.crt that is there but wrong.
	bad := reg
	bad.CertsDir, bad.CertFiles = "/etc/docker/certs.d/reg.example:5443", []string{"ca.crt"}
	fs = Diagnose(input("reg.example:5443/app:1", true, failed(bad, probe.KindTLSAuthority)))
	if !strings.Contains(fs[0].Fix, "Replace") {
		t.Fatal(fs[0])
	}
	// Unauthorized: anonymous versus refused credentials.
	in := input("reg.example:5443/app:1", true, failed(reg, probe.KindUnauthorized))
	if fs := Diagnose(in); !strings.Contains(fs[0].Title, "needs a login") {
		t.Fatal(fs[0])
	}
	in.Auth = &creds.Auth{Username: "u", Password: "p", Source: "credential helper desktop"}
	if fs := Diagnose(in); !strings.Contains(fs[0].Title, "refused the credentials from credential helper desktop") {
		t.Fatal(fs[0])
	}
	in.CredErr = errors.New("docker-credential-gcloud: not found")
	if fs := Diagnose(in); fs[0].Kind != "credentials" {
		t.Fatal(fs[0])
	}
}

func TestRateAndStale(t *testing.T) {
	hub := ok(ep("https://registry-1.docker.io", false), "sha256:index")
	hub.RateLimit = &probe.RateLimit{Limit: 100, Remaining: 3, Window: 3600, Source: "203.0.113.4"}
	mirror := ok(ep("https://mirror.example", true), "sha256:single")
	fs := Diagnose(input("alpine:3", true, mirror, hub))
	kinds := ""
	for _, f := range fs {
		kinds += f.Kind + " "
	}
	// The mirror is used; its digest differs from Hub's. (The mirror carries
	// no rate limit, so none is reported for it.)
	if !strings.Contains(kinds, "stale-mirror") {
		t.Fatal(kinds)
	}
	fs = Diagnose(input("alpine:3", true, hub))
	if fs[0].Kind != "rate-limit-low" || fs[0].Severity != Slow || !strings.Contains(fs[0].Title, "Only 3") {
		t.Fatalf("%+v", fs)
	}
	hub.RateLimit.Remaining = 50
	if fs := Diagnose(input("alpine:3", true, hub)); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
}

func TestDeepAndPull(t *testing.T) {
	hub := ok(ep("https://registry-1.docker.io", false), "sha256:a")
	in := input("nginx", true, hub)
	in.Deep = &probe.Deep{Missing: true, Platform: "linux/s390x", Platforms: []string{"linux/amd64", "linux/arm64"}}
	if fs := Diagnose(in); fs[0].Kind != pull.KindPlatform || !strings.Contains(fs[0].Evidence[0], "linux/amd64, linux/arm64") {
		t.Fatalf("%+v", fs)
	}
	in.Deep = &probe.Deep{Kind: probe.KindProxy, Error: "proxy refused", Hops: []probe.Hop{{Host: "registry-1.docker.io", Status: 307}, {Host: "production.cloudfront.docker.com", Kind: probe.KindProxy}}}
	if fs := Diagnose(in); !strings.Contains(fs[0].Fix, "Allow production.cloudfront.docker.com") {
		t.Fatalf("%+v", fs)
	}
	in.Deep = nil
	in.Pull = &pull.Timeline{Error: "toomanyrequests: ...", Kind: probe.KindRateLimited}
	if fs := Diagnose(in); fs[0].Title != "The daemon's pull failed" || fs[0].Kind != probe.KindRateLimited {
		t.Fatalf("%+v", fs)
	}
	// A pull failure of a kind a probe already found is not repeated.
	in = input("reg.example/app", true, failed(ep("https://reg.example", false), probe.KindTLSAuthority))
	in.Pull = &pull.Timeline{Error: "x509: certificate signed by unknown authority", Kind: probe.KindTLSAuthority}
	if fs := Diagnose(in); len(fs) != 1 {
		t.Fatalf("%+v", fs)
	}
	// A remote daemon gets a note.
	in = input("nginx", true, hub)
	in.Local, in.DaemonHost = false, "tcp://box:2376"
	if fs := Diagnose(in); fs[0].Kind != "remote-daemon" || !strings.Contains(fs[0].Title, "tcp://box:2376") {
		t.Fatalf("%+v", fs)
	}
}

func TestHubUnauthorizedIsMissingOrPrivate(t *testing.T) {
	in := input("nginxx", true, failed(ep("https://registry-1.docker.io", false), probe.KindUnauthorized))
	in.Pull = &pull.Timeline{Error: "pull access denied for nginxx, repository does not exist or may require 'docker login'", Kind: probe.KindNotFound}
	fs := Diagnose(in)
	if len(fs) != 1 || fs[0].Kind != probe.KindNotFound || !strings.Contains(fs[0].Title, "library/nginxx is not on Docker Hub, or it is private") {
		t.Fatalf("%+v", fs)
	}
	if strings.Contains(fs[0].Fix, "login  ") {
		t.Fatal("double space")
	}
}

func TestMirrorLogin(t *testing.T) {
	hub := ok(ep("https://registry-1.docker.io", false), "sha256:a")
	m := failed(ep("http://172.17.0.1:5099", true), probe.KindUnauthorized)
	fs := Diagnose(input("alpine:3", true, m, hub))
	if fs[0].Severity != Problem || !strings.Contains(fs[0].Title, "stops every pull") {
		t.Fatalf("containerd: %+v", fs)
	}
	fs = Diagnose(input("alpine:3", false, m, hub))
	if Worst(fs) != Note || !strings.Contains(fs[0].Title, "refused the Docker Hub credentials") {
		t.Fatalf("classic: %+v", fs)
	}
	// A quick failure of another kind is a note, not "slow".
	quick := failed(ep("http://172.17.0.1:5099", true), probe.KindServer)
	quick.Requests[0].Took = 5 * time.Millisecond
	fs = Diagnose(input("alpine:3", true, quick, hub))
	if Worst(fs) != Note {
		t.Fatalf("%+v", fs)
	}
}

func TestPickFailure(t *testing.T) {
	// A TLS registry on loopback: HTTPS says not-found, the plain-HTTP retry
	// can only say "not a registry". The first is the answer.
	https := failed(ep("https://127.0.0.1:5443", false), probe.KindNotFound)
	http := failed(ep("http://127.0.0.1:5443", false), probe.KindNotRegistry)
	if fs := Diagnose(input("127.0.0.1:5443/team/app:nope", true, https, http)); fs[0].Kind != probe.KindNotFound {
		t.Fatalf("%+v", fs)
	}
	// A plain-HTTP registry: HTTPS gets http-on-https, the HTTP retry's own
	// failure is the answer.
	https = failed(ep("https://127.0.0.1:5000", false), probe.KindHTTPOnHTTPS)
	http = failed(ep("http://127.0.0.1:5000", false), probe.KindRefused)
	if fs := Diagnose(input("127.0.0.1:5000/x", true, https, http)); fs[0].Kind != probe.KindRefused {
		t.Fatalf("%+v", fs)
	}
}

func TestSlowPhases(t *testing.T) {
	res := ok(ep("https://reg.example", false), "sha256:a")
	res.Requests = []probe.Request{{What: "ping", Method: "GET", URL: "https://reg.example/v2/", Status: 401,
		Phases: []probe.Phase{{Name: "dns", Took: 3 * time.Second}, {Name: "wait", Took: 100 * time.Millisecond}}}}
	fs := Diagnose(input("reg.example/x", true, res))
	if len(fs) != 1 || fs[0].Kind != "slow-dns" || Worst(fs) != Slow || !strings.Contains(fs[0].Title, "the DNS lookup took 3s") {
		t.Fatalf("%+v", fs)
	}
}

func TestMaskedProxy(t *testing.T) {
	e := ep("https://ghcr.io", false)
	e.Proxy, _ = url.Parse("http://user:xxxxx@proxy:3128")
	e.ProxyMasked = true
	fs := Diagnose(input("ghcr.io/o/r", true, failed(e, probe.KindProxy)))
	if Worst(fs) != Note || fs[0].Kind != "proxy-unchecked" {
		t.Fatalf("%+v", fs)
	}
}
