package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/ref"
)

// Platform is os/arch[/variant].
type Platform struct {
	OS, Arch, Variant string
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Compatible lists what the daemon accepts for p, best first (as
// containerd's platforms.Only).
func (p Platform) Compatible() []Platform {
	arm := func(vs ...string) []Platform {
		var out []Platform
		for _, v := range vs {
			out = append(out, Platform{OS: p.OS, Arch: "arm", Variant: v})
		}
		return out
	}
	switch {
	case p.Arch == "arm64":
		return append([]Platform{{OS: p.OS, Arch: "arm64", Variant: p.Variant}}, arm("v8", "v7", "v6", "v5")...)
	case p.Arch == "arm" && p.Variant == "v8":
		return arm("v8", "v7", "v6", "v5")
	case p.Arch == "arm" && p.Variant == "v7":
		return arm("v7", "v6", "v5")
	case p.Arch == "arm" && p.Variant == "v6":
		return arm("v6", "v5")
	}
	return []Platform{p}
}

// ParsePlatform reads linux/arm64 or linux/arm/v7.
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return Platform{}, fmt.Errorf("platform %q: use os/arch or os/arch/variant, such as linux/arm64", s)
	}
	p := Platform{OS: parts[0], Arch: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

// DaemonPlatform maps /info's OSType and Architecture (uname names) to a
// platform.
func DaemonPlatform(osType, arch string) Platform {
	p := Platform{OS: osType}
	switch arch {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64":
		p.Arch = "arm64"
	case "armv7l", "armhf":
		p.Arch, p.Variant = "arm", "v7"
	case "armv6l":
		p.Arch, p.Variant = "arm", "v6"
	case "i386", "i686":
		p.Arch = "386"
	default:
		p.Arch = arch
	}
	if p.OS == "" {
		p.OS = "linux"
	}
	return p
}

// Hop is one request of a blob download (redirects included).
type Hop struct {
	Host   string        `json:"host"`
	Status int           `json:"status"`
	Took   time.Duration `json:"took_ns"`
	Kind   string        `json:"kind,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// Deep is what --deep found.
type Deep struct {
	Requests  []Request `json:"requests"`
	Platforms []string  `json:"platforms,omitempty"` // in the index
	Platform  string    `json:"platform"`            // wanted
	// Missing: the index has no manifest for Platform.
	Missing   bool          `json:"platform_missing,omitempty"`
	Layer     string        `json:"layer,omitempty"`
	LayerSize int64         `json:"layer_bytes,omitempty"`
	Hops      []Hop         `json:"hops,omitempty"`
	Read      int64         `json:"read_bytes,omitempty"`
	ReadTook  time.Duration `json:"read_ns,omitempty"`
	Kind      string        `json:"kind,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// Rate is the measured download speed, bytes a second.
func (d *Deep) Rate() float64 {
	if d.ReadTook <= 0 {
		return 0
	}
	return float64(d.Read) / d.ReadTook.Seconds()
}

// DeepCheck gets the manifest (and the platform's, from an index), then reads
// up to maxBytes of the largest layer for at most readFor, following
// redirects one hop at a time. On Docker Hub the manifest GETs count as a
// pull.
func DeepCheck(ctx context.Context, res *Result, r *ref.Ref, o Options, want Platform, maxBytes int64, readFor time.Duration) *Deep {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	d := &Deep{Platform: want.String()}
	c, err := Client(res.Endpoint, o, res.TLSSkipped != "")
	if err != nil {
		d.Kind, d.Error = KindOther, err.Error()
		return d
	}
	// Manifests follow redirects as the probe does; blob redirects are
	// followed by hand, one hop at a time, on a second client.
	cb, err := Client(res.Endpoint, o, res.TLSSkipped != "")
	if err != nil {
		d.Kind, d.Error = KindOther, err.Error()
		return d
	}
	cb.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	header := http.Header{"Accept": {Accept}}
	if res.Token != "" {
		header.Set("Authorization", "Bearer "+res.Token)
	} else if res.Basic && !o.Auth.Anonymous() {
		req, _ := http.NewRequest("GET", "/", nil)
		req.SetBasicAuth(o.Auth.Username, o.Auth.Password)
		header.Set("Authorization", req.Header.Get("Authorization"))
	}
	base := res.Endpoint.Base + "/v2/" + r.Repo
	get := func(reference string) (map[string]any, string, bool) {
		var body []byte
		u := base + "/manifests/" + reference
		if res.Endpoint.Mirror && o.Containerd {
			u += "?ns=docker.io"
		}
		rec, resp := doFull(ctx, c, o, "manifest", "GET", u, header, "", func(rd io.Reader) { body, _ = io.ReadAll(io.LimitReader(rd, 4<<20)) })
		if rec.Kind == "" && rec.Status != http.StatusOK {
			rec.Kind = statusKind(rec.Status)
			if rec.Error == "" {
				rec.Error = fmt.Sprintf("the registry answered %d", rec.Status)
			}
		}
		d.Requests = append(d.Requests, rec)
		if rec.Kind != "" {
			d.Kind, d.Error = rec.Kind, rec.Error+rec.Message
			return nil, "", false
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			d.Kind, d.Error = KindOther, "the manifest is not JSON"
			return nil, "", false
		}
		mt := resp.Header.Get("Content-Type")
		if s, ok := m["mediaType"].(string); ok && s != "" {
			mt = s
		}
		return m, mt, true
	}
	m, mt, ok := get(r.Reference())
	if !ok {
		return d
	}
	if strings.Contains(mt, "index") || strings.Contains(mt, "manifest.list") {
		// The daemon takes the best compatible entry (containerd's
		// platforms.Only): exact first, then fallbacks such as arm/v7 on
		// arm64.
		var chosen string
		rank := len(want.Compatible()) + 1
		list, _ := m["manifests"].([]any)
		for _, e := range list {
			em, _ := e.(map[string]any)
			p, _ := em["platform"].(map[string]any)
			pl := Platform{OS: str(p["os"]), Arch: str(p["architecture"]), Variant: str(p["variant"])}
			if pl.OS == "unknown" { // attestations
				continue
			}
			if pl.Arch == "arm" && pl.Variant == "" {
				pl.Variant = "v7" // as containerd normalizes it
			}
			d.Platforms = append(d.Platforms, pl.String())
			for i, c := range want.Compatible() {
				if i < rank && pl.OS == c.OS && pl.Arch == c.Arch && (c.Variant == "" || pl.Variant == "" || pl.Variant == c.Variant) {
					chosen, rank = str(em["digest"]), i
				}
			}
		}
		if chosen == "" {
			d.Missing = true
			return d
		}
		if m, _, ok = get(chosen); !ok {
			return d
		}
	}
	layers, _ := m["layers"].([]any)
	for _, l := range layers {
		lm, _ := l.(map[string]any)
		if size, _ := lm["size"].(float64); int64(size) > d.LayerSize {
			d.LayerSize, d.Layer = int64(size), str(lm["digest"])
		}
	}
	if d.Layer == "" {
		return d
	}
	readBlob(ctx, cb, o, d, base+"/blobs/"+d.Layer, header, maxBytes, readFor)
	return d
}

// readBlob follows redirects by hand (to time each hop and name the hosts,
// such as a CDN) and reads the start of the blob.
func readBlob(ctx context.Context, c *http.Client, o Options, d *Deep, u string, header http.Header, maxBytes int64, readFor time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout+readFor)
	defer cancel()
	h := header.Clone()
	h.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))
	for hop := 0; hop < 5; hop++ {
		pu, _ := url.Parse(u)
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			d.Kind, d.Error = KindOther, err.Error()
			return
		}
		for k, v := range h {
			req.Header[k] = v
		}
		start := time.Now()
		resp, err := c.Do(req)
		hp := Hop{Host: pu.Host, Took: time.Since(start)}
		if err != nil {
			hp.Kind, hp.Error = Classify(err), RedactQueries(err.Error())
			d.Hops = append(d.Hops, hp)
			d.Kind, d.Error = hp.Kind, pu.Host+": "+hp.Error
			return
		}
		hp.Status = resp.StatusCode
		d.Hops = append(d.Hops, hp)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc, err := resp.Location()
			resp.Body.Close()
			if err != nil {
				d.Kind, d.Error = KindOther, "a redirect without a Location"
				return
			}
			// The next host gets no registry credentials (as the daemon does).
			if loc.Host != pu.Host {
				h.Del("Authorization")
			}
			u = loc.String()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			d.Kind, d.Error = statusKind(resp.StatusCode), fmt.Sprintf("%s answered %s", pu.Host, resp.Status)
			return
		}
		readStart := time.Now()
		buf := make([]byte, 64<<10)
		for d.Read < maxBytes && time.Since(readStart) < readFor {
			n, err := resp.Body.Read(buf)
			d.Read += int64(n)
			if err != nil {
				break
			}
		}
		d.ReadTook = time.Since(readStart)
		return
	}
	d.Kind, d.Error = KindOther, "too many redirects"
}

func statusKind(code int) string {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return KindUnauthorized
	case code == http.StatusNotFound:
		return KindNotFound
	case code == http.StatusTooManyRequests:
		return KindRateLimited
	case code >= 500:
		return KindServer
	}
	return KindOther
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
