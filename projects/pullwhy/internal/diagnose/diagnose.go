// Package diagnose turns what pullwhy saw into likely causes, each with its
// evidence and the fix, most important first.
package diagnose

import (
	"fmt"
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

// Diagnose finds the likely causes.
func Diagnose(in Input) []Finding {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }
	r := in.Plan.Ref

	if in.CredErr != nil {
		add(Finding{Severity: Problem, Kind: "credentials", Title: "The credentials for " + r.Registry + " cannot be read",
			Evidence: []string{in.CredErr.Error()},
			Fix:      "Install the credential helper named in ~/.docker/config.json, or run docker login " + loginHost(r.Registry) + " again."})
	}

	// The endpoint the daemon will end up using: the first that works.
	var used *probe.Result
	for _, res := range in.Results {
		if res.OK {
			used = res
			break
		}
	}
	last := in.Results[len(in.Results)-1]

	for _, res := range in.Results {
		if res.Endpoint.Mirror && !res.OK {
			add(mirrorFinding(in, res, used != nil))
		}
	}
	if used == nil {
		add(registryFinding(in, last))
	} else {
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
		if in.Info.DockerDesktop() {
			why = "the daemon runs in Docker Desktop's virtual machine"
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
	return registry
}

func mirrorFinding(in Input, res *probe.Result, fallbackWorks bool) Finding {
	f := Finding{Kind: "mirror-" + res.Kind, Evidence: evidence(res)}
	fix := "Remove it from \"registry-mirrors\" in " + DaemonJSON + " (or fix it), then restart Docker."
	switch res.Kind {
	case probe.KindTimeout:
		f.Severity = Slow
		f.Title = "The mirror " + res.Base + " does not answer, and the daemon waits for it on every pull"
		if in.Plan.Containerd {
			f.Evidence = append(f.Evidence, "with the containerd image store, the daemon waits about 30 s for it on each request of a pull "+
				"before it tries the next endpoint (manifest, index, each layer...); a pull of alpine:3 took 2 min instead of 1 s in a test")
		} else {
			f.Evidence = append(f.Evidence, "with the classic image store, the daemon waits about 15 s for it once per pull before it tries the next endpoint")
		}
	case probe.KindRefused, probe.KindDNS, probe.KindNotRegistry:
		f.Severity = Note
		f.Title = "The mirror " + res.Base + " is not usable; the daemon skips it at once"
	case probe.KindNotFound:
		f.Severity = Note
		f.Title = "The mirror " + res.Base + " does not have " + in.Plan.Ref.String() + "; the daemon goes on to the next endpoint"
		fix = ""
	default:
		f.Severity = Slow
		f.Title = "The mirror " + res.Base + " fails (" + res.Kind + "); the daemon then tries the next endpoint"
	}
	if !fallbackWorks {
		f.Severity = Problem
	}
	f.Fix = fix
	return f
}

func registryFinding(in Input, res *probe.Result) Finding {
	r := in.Plan.Ref
	host := res.Endpoint.Host
	f := Finding{Severity: Problem, Kind: res.Kind, Evidence: evidence(res)}
	proxied := res.Endpoint.Proxy != nil
	switch res.Kind {
	case probe.KindTLSAuthority:
		f.Title = host + "'s certificate is signed by a CA the daemon does not know"
		dir := plan.CertsRoot + "/" + host
		if publicRegistry(res.Endpoint.Host) {
			// Public registries use public CAs: something on the way swaps
			// the certificate.
			f.Title = "Something between the daemon and " + host + " intercepts TLS"
			f.Evidence = append(f.Evidence, host+" uses a publicly trusted certificate, so an unknown CA means another certificate was put in its place "+
				"(a company proxy, antivirus or a captive portal)")
			f.Fix = "Add the intercepting CA to the trust store of the daemon's host and restart Docker (or put it in " + dir +
				"/ca.crt), or have registry traffic exempted from interception."
			break
		}
		if res.Endpoint.CertsDir != "" && contains(res.Endpoint.CertFiles, "ca.crt") {
			f.Evidence = append(f.Evidence, dir+"/ca.crt is there, but it does not sign this certificate")
			f.Fix = "Replace " + dir + "/ca.crt with the CA that signed the registry's certificate."
		} else {
			f.Fix = "Copy the registry's CA certificate to " + dir + "/ca.crt on the daemon's host (no restart needed), " +
				"or add the CA to the host's trust store and restart Docker."
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
		f.Fix = "Add \"" + host + "\" to \"insecure-registries\" in " + DaemonJSON + " and restart Docker, or give the registry TLS."
	case probe.KindProxy:
		f.Title = "The daemon's proxy " + proxyHost(res) + " does not get through to " + host
		f.Fix = "Check the proxy, and \"proxies\" in " + DaemonJSON + " (or the docker service's HTTP(S)_PROXY); add internal registries to no-proxy."
	case probe.KindTimeout, probe.KindRefused, probe.KindReset, probe.KindDNS:
		what := map[string]string{probe.KindTimeout: "does not answer", probe.KindRefused: "refuses connections",
			probe.KindReset: "drops the connection", probe.KindDNS: "cannot be found in DNS"}[res.Kind]
		f.Title = host + " " + what
		if proxied {
			f.Evidence = append(f.Evidence, "the daemon goes through the proxy "+proxyHost(res)+" for it")
		} else if in.Info.HTTPSProxy != "" {
			f.Evidence = append(f.Evidence, "the daemon has a proxy, but no-proxy excludes "+host)
		}
		f.Fix = "Check the address, the network path and any firewall between the daemon and " + host + "."
	case probe.KindUnauthorized:
		if in.Auth.Anonymous() {
			f.Title = host + " needs a login for " + r.Repo
			f.Fix = "Run docker login " + loginHost(r.Registry) + " (with an account that can read " + r.Repo + ")."
		} else {
			f.Title = host + " refused the credentials from " + in.Auth.Source
			f.Fix = "Run docker login " + loginHost(r.Registry) + " again; a token may have expired or lack read access to " + r.Repo + "."
		}
	case probe.KindNotFound:
		f.Title = r.String() + " is not there"
		if r.Registry == "docker.io" && in.Auth.Anonymous() {
			f.Evidence = append(f.Evidence, "Docker Hub answers the same for a private repository without a login")
		}
		f.Fix = "Check the name and the tag; for a private repository, docker login " + loginHost(r.Registry) + "."
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
			for _, res := range in.Results {
				if res.Endpoint.Mirror && res.Kind == probe.KindTimeout {
					f.Evidence = append(f.Evidence, "that is the time spent waiting for the mirror "+res.Base)
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
