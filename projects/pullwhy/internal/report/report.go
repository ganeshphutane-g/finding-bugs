// Package report prints what pullwhy found, for people (text) or scripts
// (JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/diagnose"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
	"github.com/ganeshphutane-g/pullwhy/internal/pull"
)

// Daemon is the daemon's side.
type Daemon struct {
	Host    string `json:"host"`
	From    string `json:"from"`
	Version string `json:"version"`
	Store   string `json:"image_store"` // containerd or classic
	Proxy   string `json:"proxy,omitempty"`
	NoProxy string `json:"no_proxy,omitempty"`
	Local   bool   `json:"probes_local"`
}

// Step is one endpoint of the plan, for the JSON report.
type Step struct {
	Endpoint string `json:"endpoint"`
	Mirror   bool   `json:"mirror,omitempty"`
	Insecure string `json:"insecure,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	CertsDir string `json:"certs_dir,omitempty"`
	*probe.Result
}

// Report is the whole thing.
type Report struct {
	Image       string             `json:"image"`
	Daemon      Daemon             `json:"daemon"`
	Credentials string             `json:"credentials"`
	Steps       []Step             `json:"plan"`
	Deep        *probe.Deep        `json:"deep,omitempty"`
	Pull        *pull.Timeline     `json:"pull,omitempty"`
	Findings    []diagnose.Finding `json:"findings"`
}

// New puts the parts together.
func New(image string, d Daemon, credentials string, p *plan.Plan, results []*probe.Result, deep *probe.Deep, t *pull.Timeline, fs []diagnose.Finding) *Report {
	r := &Report{Image: image, Daemon: d, Credentials: credentials, Deep: deep, Pull: t, Findings: fs}
	if r.Findings == nil {
		r.Findings = []diagnose.Finding{}
	}
	for i, e := range p.Endpoints {
		s := Step{Endpoint: e.Base, Mirror: e.Mirror, CertsDir: e.CertsDir}
		if e.Insecure {
			s.Insecure = e.InsecureWhy
		}
		if e.Proxy != nil {
			s.Proxy = e.Proxy.Redacted()
		}
		if i < len(results) {
			s.Result = results[i]
		}
		r.Steps = append(r.Steps, s)
	}
	return r
}

// JSON writes the report as JSON.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func ms(d time.Duration) string {
	switch {
	case d >= 10*time.Second:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(100 * time.Millisecond).String()
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// Text writes the report for people.
func (r *Report) Text(w io.Writer) {
	d := r.Daemon
	fmt.Fprintf(w, "pullwhy %s\n\n", r.Image)
	fmt.Fprintf(w, "daemon       Docker %s, %s image store, %s (%s)\n", d.Version, d.Store, d.Host, d.From)
	if d.Proxy != "" {
		fmt.Fprintf(w, "proxy        %s", d.Proxy)
		if d.NoProxy != "" {
			fmt.Fprintf(w, " (not for %s)", d.NoProxy)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "credentials  %s\n", r.Credentials)

	for i, s := range r.Steps {
		label := "registry"
		if s.Mirror {
			label = "mirror"
		}
		fmt.Fprintf(w, "\n%d. %s %s", i+1, label, s.Endpoint)
		var notes []string
		if s.Proxy != "" {
			notes = append(notes, "through "+s.Proxy)
		}
		if s.Insecure != "" {
			notes = append(notes, "insecure: "+s.Insecure)
		}
		if s.CertsDir != "" {
			notes = append(notes, "certificates from "+s.CertsDir)
		}
		if len(notes) > 0 {
			fmt.Fprintf(w, "  (%s)", strings.Join(notes, "; "))
		}
		fmt.Fprintln(w)
		if s.Result == nil {
			continue
		}
		for _, q := range s.Requests {
			fmt.Fprintf(w, "   %-9s %-4s %s\n", q.What, q.Method, shorten(q.URL, s.Endpoint))
			fmt.Fprintf(w, "             %s\n", outcome(q))
		}
		res := s.Result
		if res.OK {
			fmt.Fprintf(w, "   found     %s (%s)\n", res.Digest, kindOfManifest(res.MediaType))
		}
		if rl := res.RateLimit; rl != nil {
			fmt.Fprintf(w, "   limit     %d of %d pulls left per %ds, counted for %s\n", rl.Remaining, rl.Limit, rl.Window, rl.Source)
		}
	}

	if dp := r.Deep; dp != nil {
		fmt.Fprintf(w, "\ndeep check (counts as a pull on Docker Hub)\n")
		if len(dp.Platforms) > 0 {
			fmt.Fprintf(w, "   platforms %s (wanted %s)\n", strings.Join(dp.Platforms, ", "), dp.Platform)
		}
		if dp.Layer != "" {
			fmt.Fprintf(w, "   layer     %s, %s\n", dp.Layer, diagnose.Size(dp.LayerSize))
		}
		for _, h := range dp.Hops {
			o := fmt.Sprintf("%d", h.Status)
			if h.Kind != "" {
				o = h.Kind + ": " + h.Error
			}
			fmt.Fprintf(w, "   from      %s  %s  %s\n", h.Host, o, ms(h.Took))
		}
		if dp.Read > 0 {
			fmt.Fprintf(w, "   read      %s in %s: %s\n", diagnose.Size(dp.Read), ms(dp.ReadTook), diagnose.Rate(dp.Rate()))
		}
	}

	if t := r.Pull; t != nil {
		fmt.Fprintf(w, "\npull by the daemon\n")
		if len(t.Layers) > 0 {
			fmt.Fprintf(w, "   start     first layer after %s\n", ms(t.FirstLayer))
		} else if t.Error == "" {
			fmt.Fprintf(w, "   layers    none to download (all were here already)\n")
		}
		for _, l := range t.Layers {
			switch {
			case l.Cached:
				fmt.Fprintf(w, "   layer     %s already there\n", l.ID)
			case l.Rate() > 0:
				fmt.Fprintf(w, "   layer     %s %s at %s", l.ID, diagnose.Size(l.Total), diagnose.Rate(l.Rate()))
				if l.Longest >= time.Second {
					fmt.Fprintf(w, " (longest wait %s)", ms(l.Longest))
				}
				fmt.Fprintln(w)
			default:
				fmt.Fprintf(w, "   layer     %s done at %s\n", l.ID, ms(l.Done))
			}
		}
		if t.Error != "" {
			fmt.Fprintf(w, "   failed    %s\n", t.Error)
		} else {
			fmt.Fprintf(w, "   done      %s in %s\n", strings.TrimSpace(t.Status), ms(t.Took))
		}
	}

	fmt.Fprintln(w)
	if len(r.Findings) == 0 {
		fmt.Fprintln(w, "Nothing wrong found: the daemon can reach the image, and nothing was slow.")
		return
	}
	fmt.Fprintln(w, "Likely cause")
	for _, f := range r.Findings {
		fmt.Fprintf(w, "\n%-8s %s\n", f.Severity+":", f.Title)
		for _, e := range f.Evidence {
			fmt.Fprintf(w, "         - %s\n", e)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "         Fix: %s\n", f.Fix)
		}
	}
}

func shorten(u, base string) string {
	if strings.HasPrefix(u, base) {
		return strings.TrimPrefix(u, base)
	}
	return u
}

func outcome(q probe.Request) string {
	var phases []string
	for _, p := range q.Phases {
		phases = append(phases, p.Name+" "+ms(p.Took))
	}
	timing := ms(q.Took)
	if len(phases) > 0 {
		timing += " (" + strings.Join(phases, ", ") + ")"
	} else if q.Reused {
		timing += " (same connection)"
	}
	switch {
	case q.Kind == probe.KindTimeout:
		return "no answer in " + ms(q.Took)
	case q.Kind != "" && q.Status == 0:
		return q.Kind + ": " + q.Error
	case q.Kind != "":
		s := fmt.Sprintf("%d %s, %s", q.Status, q.Kind, timing)
		if q.Message != "" {
			s += ": " + q.Message
		}
		return s
	}
	return fmt.Sprintf("%d, %s", q.Status, timing)
}

func kindOfManifest(mt string) string {
	switch {
	case strings.Contains(mt, "index"), strings.Contains(mt, "manifest.list"):
		return "a multi-platform index"
	case strings.Contains(mt, "manifest"):
		return "a single-platform manifest"
	}
	return mt
}
