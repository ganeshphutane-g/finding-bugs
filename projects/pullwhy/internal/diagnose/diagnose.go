// Package diagnose turns what pullwhy saw into likely causes, each with its
// evidence and the fix, most important first.
package diagnose

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/creds"
	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
	"github.com/ganeshphutane-g/pullwhy/internal/pull"
)

// Severities, most serious first.
const (
	Problem = "problem" // the pull fails, or would
	Slow    = "slow"    // it works, slowly
	Note    = "note"    // worth knowing
)

// Finding is one likely cause.
type Finding struct {
	Severity string   `json:"severity"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Evidence []string `json:"evidence,omitempty"`
	Fix      string   `json:"fix,omitempty"`
}

// Input is everything seen.
type Input struct {
	Info    *engine.Info
	Local   bool // probes ran where the daemon runs
	Plan    *plan.Plan
	Results []*probe.Result // one per plan endpoint, in order
	Auth    *creds.Auth
	CredErr error
	Deep    *probe.Deep
	Pull    *pull.Timeline
	// Timeout is the probes' per-request timeout.
	Timeout time.Duration
	// DaemonHost is the daemon's address, for the remote-daemon note.
	DaemonHost string
}

// DaemonJSON is where the daemon's settings usually live.
const DaemonJSON = "/etc/docker/daemon.json"

func daemonJSON(in Input) string {
	if in.Info != nil && in.Info.Rootless() {
		return "~/.config/docker/daemon.json"
	}
	return DaemonJSON
}

// registryAnswered: kinds where the registry itself answered, which say
// more than a failure to connect.
var registryAnswered = map[string]bool{probe.KindUnauthorized: true, probe.KindNotFound: true, probe.KindRateLimited: true,
	probe.KindTokenUnavailable: true, probe.KindServer: true}

// pickFailure chooses the registry endpoint whose failure explains most:
// one where the registry answered; else not the plain-HTTP retry of a TLS
// registry (which can only say "not a registry"); else the last.
func pickFailure(results []*probe.Result) *probe.Result {
	var regs []*probe.Result
	for _, r := range results {
		if !r.Endpoint.Mirror {
			regs = append(regs, r)
		}
	}
	if len(regs) == 0 {
		return results[len(results)-1]
	}
	for _, r := range regs {
		if registryAnswered[r.Kind] {
			return r
		}
	}
	if len(regs) == 2 && regs[0].Kind != probe.KindHTTPOnHTTPS {
		return regs[0]
	}
	return regs[len(regs)-1]
}

// Diagnose finds the likely causes.
func Diagnose(in Input) []Finding {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }
	r := in.Plan.Ref

	if in.CredErr != nil {
		add(Finding{Severity: Problem, Kind: "credentials", Title: "The credentials for " + r.Registry + " cannot be read",
			Evidence: []string{in.CredErr.Error()},
			Fix:      "Install the credential helper named in ~/.docker/config.json, or run docker login" + loginHost(r.Registry) + " again."})
	}

	// The endpoint the daemon will end up using: the first that works. On
	// the containerd store, when the pull carries Docker Hub credentials, a
	// mirror that asks for a login stops it there (tested: the mirror is sent
	// none, and there is no fallback); anonymous pulls fall back.
	var used, blocked *probe.Result
	for _, res := range in.Results {
		if res.OK {
			used = res
			break
		}
		if res.Endpoint.Mirror && in.Plan.Containerd && res.Kind == probe.KindUnauthorized && !in.Auth.Anonymous() {
			blocked = res
			break
		}
	}
	last := in.Results[len(in.Results)-1]

	for _, res := range in.Results {
		if res.Endpoint.Mirror && !res.OK {
			add(mirrorFinding(in, res, used != nil && blocked == nil))
		}
		if res == blocked {
			break
		}
	}
	if used == nil && blocked == nil {
		add(registryFinding(in, pickFailure(in.Results)))
	} else if used != nil {
		out = append(out, slowFindings(used)...)
		if used.RateLimit != nil {
			if f, ok := rateFinding(used.RateLimit); ok {
				add(f)
			}
		}
		if used.TLSSkipped != "" {
			add(Finding{Severity: Note, Kind: "tls-not-verified", Title: used.Base + "'s certificate does not verify; the daemon accepts it because the registry is insecure",
				Evidence: []string{used.TLSSkipped, used.Endpoint.InsecureWhy}})
		}
	}
	// A mirror serving another manifest than the registry.
	if hub := last; used != nil && used != hub && hub.OK && used.Digest != "" && hub.Digest != "" && used.Digest != hub.Digest && r.Digest == "" {
		add(Finding{Severity: Note, Kind: "stale-mirror", Title: "The mirror " + used.Base + " gives another image for " + r.Reference() + " than " + hub.Base,
			Evidence: []string{"mirror: " + used.Digest + " (" + used.MediaType + ")", "registry: " + hub.Digest + " (" + hub.MediaType + ")"},
			Fix:      "The mirror may be out of date, or hold a single-platform copy. Pull by digest to be sure, or refresh the mirror."})
	}

	if d := in.Deep; d != nil {
		switch {
		case d.Missing:
			add(Finding{Severity: Problem, Kind: pull.KindPlatform, Title: fmt.Sprintf("%s has no image for %s", r, d.Platform),
				Evidence: []string{"the image is built for: " + strings.Join(d.Platforms, ", ")},
				Fix:      "Use an image built for " + d.Platform + ", or pull another platform with --platform (it may run under emulation)."})
		case d.Kind != "" && len(d.Hops) > 0:
			h := d.Hops[len(d.Hops)-1]
			f := Finding{Severity: Problem, Kind: "blob-" + d.Kind, Title: "Layers come from " + h.Host + ", which cannot be read",
				Evidence: []string{d.Error}}
			if len(d.Hops) > 1 {
				f.Evidence = append(f.Evidence, "the registry redirects layer downloads to "+h.Host)
				f.Fix = "Allow " + h.Host + " through your proxy or firewall, as well as the registry."
			}
			add(f)
		case d.Kind != "":
			add(Finding{Severity: Problem, Kind: "manifest-" + d.Kind, Title: "Reading the manifest failed", Evidence: []string{d.Error}})
		case d.Rate() > 0 && d.Rate() < 1e6 && d.Read >= 256<<10:
			host := ""
			if len(d.Hops) > 0 {
				host = d.Hops[len(d.Hops)-1].Host
			}
			add(Finding{Severity: Slow, Kind: "slow-download", Title: fmt.Sprintf("Layers download slowly from %s: %s", host, rate(d.Rate())),
				Evidence: []string{fmt.Sprintf("read %s in %s", size(d.Read), d.ReadTook.Round(time.Millisecond))}})
		}
	}

	if t := in.Pull; t != nil {
		out = append(out, pullFindings(in, t, used)...)
	}

	if !in.Local {
		why := "the daemon is at " + in.DaemonHost
		switch {
		case in.Info.DockerDesktop():
			why = "the daemon runs in Docker Desktop's virtual machine"
		case in.Info.Rootless():
			why = "the daemon runs rootless, in its own network namespace"
		}
		add(Finding{Severity: Note, Kind: "remote-daemon", Title: "The probes ran from this machine, but " + why,
			Evidence: []string{"its network, proxy and certificates can differ from this machine's; --pull shows what the daemon itself does"}})
	}
	return order(dedupe(out))
}

func loginHost(registry string) string {
	if registry == "docker.io" {
		return ""
	}
	return " " + registry
}

func loopback(hostport string) bool {
	h := strings.Trim(strings.Split(strings.TrimPrefix(hostport, "["), "]")[0], "[]")
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[:i], ":") {
		h = h[:i]
	}
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

// internal reports whether a registry host looks like it is on a private
// network (a private address, or a name without a public suffix).
func internal(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if a, err := netip.ParseAddr(h); err == nil {
		return a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLoopback()
	}
	if !strings.Contains(h, ".") {
		return true
	}
	for _, suf := range []string{".local", ".lan", ".internal", ".home.arpa", ".corp", ".intranet"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

func mirrorFinding(in Input, res *probe.Result, fallbackWorks bool) Finding {
	f := Finding{Kind: "mirror-" + res.Kind, Evidence: evidence(res), Severity: Note}
	fix := "Remove it from \"registry-mirrors\" in " + daemonJSON(in) + " (or fix it), then restart Docker."
	slowest := time.Duration(0)
	for _, q := range res.Requests {
		if q.Took > slowest {
			slowest = q.Took
		}
	}
	switch res.Kind {
	case probe.KindTimeout:
		f.Severity = Slow
		f.Title = "The mirror " + res.Base + " does not answer, and the daemon waits for it on every pull"
		if in.Plan.Containerd {
			f.Evidence = append(f.Evidence, "with the containerd image store, the daemon waits about 30 s for it on each request of a pull "+
				"before it tries the next endpoint (manifest, index, each layer...); in tests a pull of alpine:3 took 2 to 3.5 min instead of 1 s")
		} else {
			f.Evidence = append(f.Evidence, "with the classic image store, the daemon waits about 15 s for it once per pull before it tries the next endpoint")
		}
	case probe.KindUnauthorized:
		switch {
		case in.Plan.Containerd && !in.Auth.Anonymous():
			f.Severity = Problem
			f.Title = "The mirror " + res.Base + " asks for a login, which stops pulls that carry Docker Hub credentials"
			f.Evidence = append(f.Evidence, "with the containerd image store, the daemon sends a mirror no credentials; when the pull "+
				"carries Docker Hub credentials, the mirror's refusal ends it instead of falling back to Docker Hub")
			fix = "Let the mirror serve pulls without a login, or remove it from \"registry-mirrors\" in " + daemonJSON(in) + "."
		case in.Plan.Containerd:
			f.Title = "The mirror " + res.Base + " asks for a login; anonymous pulls go on to Docker Hub"
			f.Evidence = append(f.Evidence, "with the containerd image store, the daemon sends a mirror no credentials; "+
				"after docker login, pulls would stop at this mirror instead")
		default:
			f.Title = "The mirror " + res.Base + " refused the Docker Hub credentials; the daemon goes on to the next endpoint"
			f.Evidence = append(f.Evidence, "with the classic image store, the daemon sends a mirror the Docker Hub credentials")
		}
	case probe.KindRefused, probe.KindDNS, probe.KindNotRegistry:
		f.Title = "The mirror " + res.Base + " is not usable; the daemon skips it at once"
	case probe.KindNotFound:
		f.Title = "The mirror " + res.Base + " does not have " + in.Plan.Ref.String() + "; the daemon goes on to the next endpoint"
		fix = ""
	default:
		f.Title = "The mirror " + res.Base + " fails (" + res.Kind + "); the daemon then tries the next endpoint"
		if slowest >= 2*time.Second {
			f.Severity = Slow
		}
	}
	if !fallbackWorks {
		f.Severity = Problem
	}
	f.Fix = fix
	return f
}

// slowFindings looks at the timings of the endpoint in use.
func slowFindings(res *probe.Result) []Finding {
	limits := map[string]time.Duration{"dns": time.Second, "connect": time.Second, "tls": time.Second, "wait": 2 * time.Second}
	names := map[string]string{"dns": "the DNS lookup", "connect": "connecting", "tls": "the TLS handshake", "wait": "waiting for the answer"}
	var out []Finding
	for _, q := range res.Requests {
		u, _ := url.Parse(q.URL)
		host := q.URL
		if u != nil {
			host = u.Host
		}
		for _, ph := range q.Phases {
			if lim, ok := limits[ph.Name]; ok && ph.Took >= lim {
				out = append(out, Finding{Severity: Slow, Kind: "slow-" + ph.Name,
					Title: fmt.Sprintf("%s is slow: %s took %s (%s %s)", host, names[ph.Name], ph.Took.Round(10*time.Millisecond), q.Method, q.What)})
			}
		}
	}
	return out
}

func registryFinding(in Input, res *probe.Result) Finding {
	r := in.Plan.Ref
	host := res.Endpoint.Host
	f := Finding{Severity: Problem, Kind: res.Kind, Evidence: evidence(res)}
	proxied := res.Endpoint.Proxy != nil
	switch res.Kind {
	case probe.KindTLSAuthority:
		f.Title = host + "'s certificate is signed by a CA the daemon does not know"
		dir := res.Endpoint.CertsPath
		trust := "add the CA to the trust store of the daemon's host and restart Docker"
		where := " (or put it in " + dir + "/ca.crt)"
		if dir == "" {
			where = " (for Docker Hub the classic image store reads no certs.d)"
		}
		if publicRegistry(res.Endpoint.Host) {
			// Public registries use public CAs: something on the way swaps
			// the certificate.
			f.Title = "Something between the daemon and " + host + " intercepts TLS"
			f.Evidence = append(f.Evidence, host+" uses a publicly trusted certificate, so an unknown CA means another certificate was put in its place "+
				"(a company proxy, antivirus or a captive portal)")
			f.Fix = "Add the intercepting CA to the trust store of the daemon's host and restart Docker" + where +
				", or have registry traffic exempted from interception."
			break
		}
		switch {
		case dir == "":
			f.Fix = "To trust its CA, " + trust + where + "."
		case res.Endpoint.CertsDir != "" && contains(res.Endpoint.CertFiles, "ca.crt"):
			f.Evidence = append(f.Evidence, dir+"/ca.crt is there, but it does not sign this certificate")
			f.Fix = "Replace " + dir + "/ca.crt with the CA that signed the registry's certificate."
		default:
			f.Fix = "Copy the registry's CA certificate to " + dir + "/ca.crt on the daemon's host (no restart needed), " +
				"or " + trust + "."
		}
	case probe.KindTLSExpired:
		f.Title = host + "'s certificate has expired (or is not valid yet)"
		f.Fix = "Renew the registry's certificate; check the clock on both machines."
	case probe.KindTLSName:
		f.Title = host + "'s certificate is for another name"
		f.Fix = "Use the name the certificate is for, or issue one that includes " + res.Endpoint.Host + "."
	case probe.KindTLSOther:
		f.Title = "The TLS connection to " + host + " fails"
	case probe.KindHTTPOnHTTPS:
		f.Title = host + " speaks plain HTTP, and the daemon only uses HTTPS for it"
		f.Fix = "Add \"" + host + "\" to \"insecure-registries\" in " + daemonJSON(in) + " and restart Docker, or give the registry TLS."
	case probe.KindProxy:
		if res.Endpoint.ProxyMasked {
			f.Severity, f.Kind = Note, "proxy-unchecked"
			f.Title = "pullwhy could not check " + host + " through the daemon's proxy: it needs a password, which the daemon does not show"
			f.Fix = "Run with --pull to see what the daemon itself gets."
			break
		}
		f.Title = "The daemon's proxy " + proxyHost(res) + " does not get through to " + host
		if internal(res.Endpoint.Host) {
			f.Evidence = append(f.Evidence, host+" looks internal: add it to no-proxy if the proxy cannot reach it")
		}
		f.Fix = "Check the proxy, and \"proxies\" in " + daemonJSON(in) + " (or the docker service's HTTP(S)_PROXY); add internal registries to no-proxy."
	case probe.KindTimeout, probe.KindRefused, probe.KindReset, probe.KindDNS:
		what := map[string]string{probe.KindTimeout: "does not answer", probe.KindRefused: "refuses connections",
			probe.KindReset: "drops the connection", probe.KindDNS: "cannot be found in DNS"}[res.Kind]
		f.Title = host + " " + what
		schemeProxy := in.Info.HTTPSProxy
		if strings.HasPrefix(res.Base, "http://") {
			schemeProxy = in.Info.HTTPProxy
		}
		switch {
		case proxied:
			f.Evidence = append(f.Evidence, "the daemon goes through the proxy "+proxyHost(res)+" for it")
			if internal(res.Endpoint.Host) {
				f.Evidence = append(f.Evidence, host+" looks internal: add it to no-proxy if the proxy cannot reach it")
			}
		case schemeProxy != "" && !loopback(res.Endpoint.Host):
			f.Evidence = append(f.Evidence, "the daemon has a proxy, but no-proxy excludes "+host)
		}
		f.Fix = "Check the address, the network path and any firewall between the daemon and " + host + "."
	case probe.KindUnauthorized:
		if r.Registry == "docker.io" {
			// Docker Hub answers 401 for a repository that does not exist
			// as for a private one (checked).
			f.Kind = probe.KindNotFound
			if in.Auth.Anonymous() {
				f.Title = r.Repo + " is not on Docker Hub, or it is private"
				f.Fix = "Check the name (and the tag); for a private repository, run docker login."
			} else {
				f.Title = r.Repo + " is not on Docker Hub, or the account from " + in.Auth.Source + " cannot read it"
				f.Fix = "Check the name; run docker login again if the account should have access."
			}
			f.Evidence = append(f.Evidence, "Docker Hub gives the same answer for a repository that does not exist and for a private one")
			break
		}
		if in.Auth.Anonymous() {
			f.Title = host + " needs a login for " + r.Repo
			f.Fix = "Run docker login" + loginHost(r.Registry) + " (with an account that can read " + r.Repo + ")."
		} else {
			f.Title = host + " refused the credentials from " + in.Auth.Source
			f.Fix = "Run docker login" + loginHost(r.Registry) + " again; a token may have expired or lack read access to " + r.Repo + "."
		}
	case probe.KindNotFound:
		f.Title = r.String() + " is not there"
		if r.Registry == "docker.io" && in.Auth.Anonymous() {
			f.Evidence = append(f.Evidence, "Docker Hub answers the same for a private repository without a login")
		}
		f.Fix = "Check the name and the tag; for a private repository, docker login" + loginHost(r.Registry) + "."
	case probe.KindRateLimited:
		f.Title = "Docker Hub's pull limit is used up"
		if rl := res.RateLimit; rl != nil {
			f.Evidence = append(f.Evidence, fmt.Sprintf("%d of %d pulls left per %s, counted for %s", rl.Remaining, rl.Limit, window(rl.Window), rl.Source))
		}
		f.Fix = "Log in (docker login) for a higher limit, wait for the window to pass, or use a pull-through mirror."
	case probe.KindNotRegistry:
		f.Title = res.Base + " does not look like a registry"
		f.Fix = "Check the registry's address and port."
	default:
		f.Title = res.Base + " fails (" + res.Kind + ")"
	}
	return f
}

func rateFinding(rl *probe.RateLimit) (Finding, bool) {
	if rl.Limit <= 0 || rl.Remaining*10 >= rl.Limit {
		return Finding{}, false
	}
	sev := Slow
	if rl.Remaining == 0 {
		sev = Problem
	}
	return Finding{Severity: sev, Kind: "rate-limit-low", Title: fmt.Sprintf("Only %d Docker Hub pulls are left this %s", rl.Remaining, window(rl.Window)),
		Evidence: []string{fmt.Sprintf("%d of %d per %s, counted for %s", rl.Remaining, rl.Limit, window(rl.Window), rl.Source)},
		Fix:      "Log in (docker login) for a higher limit, or use a pull-through mirror."}, true
}

func pullFindings(in Input, t *pull.Timeline, used *probe.Result) []Finding {
	var out []Finding
	if t.Error != "" {
		f := Finding{Severity: Problem, Kind: t.Kind, Title: "The daemon's pull failed", Evidence: []string{t.Error}}
		if t.Unfinished {
			f.Title = fmt.Sprintf("The daemon was still pulling after %s", t.Took.Round(time.Second))
		}
		out = append(out, f)
	}
	// Silences, added up by phase.
	byPhase := map[string]time.Duration{}
	var phases []string
	for _, g := range t.Gaps {
		if _, ok := byPhase[g.Phase]; !ok {
			phases = append(phases, g.Phase)
		}
		byPhase[g.Phase] += g.Took
	}
	for _, ph := range phases {
		took := byPhase[ph]
		f := Finding{Severity: Slow, Kind: "stall", Title: fmt.Sprintf("The pull made no progress for %s (%s)", took.Round(time.Second), ph)}
		if ph == "before the first layer" {
			f.Kind = "slow-start"
			f.Title = fmt.Sprintf("The daemon spent %s resolving the image before any layer", took.Round(time.Second))
		}
		if ph != "downloading" {
			for _, res := range in.Results {
				if res.Endpoint.Mirror && res.Kind == probe.KindTimeout {
					f.Evidence = append(f.Evidence, "likely time spent waiting for the mirror "+res.Base)
				}
			}
		}
		out = append(out, f)
	}
	if s := t.Slowest(); s != nil && s.Rate() < 1e6 {
		out = append(out, Finding{Severity: Slow, Kind: "slow-download",
			Title: fmt.Sprintf("Layer %s (%s) came at %s", s.ID, size(s.Total), rate(s.Rate()))})
	}
	return out
}

func evidence(res *probe.Result) []string {
	var ev []string
	for _, q := range res.Requests {
		if q.Kind == "" {
			continue
		}
		s := fmt.Sprintf("%s %s: ", q.Method, q.URL)
		switch {
		case q.Kind == probe.KindTimeout:
			s += fmt.Sprintf("no answer in %s", q.Took.Round(100*time.Millisecond))
		case q.Error != "":
			if strings.Contains(q.Error, q.URL) {
				s = q.Error // Go's errors already name the request
			} else {
				s += q.Error
			}
			if q.Message != "" {
				s += " (" + q.Message + ")"
			}
		default:
			s += fmt.Sprintf("%d", q.Status)
			if q.Message != "" {
				s += " " + q.Message
			}
		}
		ev = append(ev, s)
	}
	return ev
}

// publicRegistry reports whether host is a well-known public registry, whose
// certificates come from public CAs.
func publicRegistry(host string) bool {
	h := strings.Split(host, ":")[0]
	for _, p := range []string{"registry-1.docker.io", "auth.docker.io", "docker.io", "ghcr.io", "quay.io", "gcr.io", "mcr.microsoft.com",
		"registry.k8s.io", "public.ecr.aws", "registry.gitlab.com", "nvcr.io", "docker.elastic.co"} {
		if h == p || strings.HasSuffix(h, ".gcr.io") || strings.HasSuffix(h, ".pkg.dev") {
			return true
		}
	}
	return false
}

func proxyHost(res *probe.Result) string {
	if res.Endpoint.Proxy != nil {
		return res.Endpoint.Proxy.Host
	}
	return "(none)"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// dedupe drops a pull failure that repeats a probe finding of the same kind.
func dedupe(fs []Finding) []Finding {
	seen := map[string]bool{}
	for _, f := range fs {
		if f.Title != "The daemon's pull failed" {
			seen[f.Kind] = true
		}
	}
	var out []Finding
	for _, f := range fs {
		if f.Title == "The daemon's pull failed" && seen[f.Kind] {
			continue
		}
		out = append(out, f)
	}
	return out
}

func order(fs []Finding) []Finding {
	rank := map[string]int{Problem: 0, Slow: 1, Note: 2}
	sort.SliceStable(fs, func(i, j int) bool { return rank[fs[i].Severity] < rank[fs[j].Severity] })
	return fs
}

// Worst is the most serious severity ("" for none).
func Worst(fs []Finding) string {
	if len(fs) == 0 {
		return ""
	}
	return fs[0].Severity
}

func window(sec int) string {
	switch {
	case sec == 3600:
		return "hour"
	case sec > 0 && sec%3600 == 0:
		return fmt.Sprintf("%d hours", sec/3600)
	case sec > 0:
		return fmt.Sprintf("%d s", sec)
	}
	return "window"
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func rate(bps float64) string {
	switch {
	case bps >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.0f KB/s", bps/1e3)
	}
	return fmt.Sprintf("%.0f B/s", bps)
}

// Size and Rate are for the report.
func Size(n int64) string     { return size(n) }
func Rate(bps float64) string { return rate(bps) }
