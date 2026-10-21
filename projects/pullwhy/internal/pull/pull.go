// Package pull has the daemon really pull, and makes a timeline of it from
// the progress stream: how long it took to start (resolving, mirrors, auth),
// how fast each layer came, where it stalled, and why it failed.
package pull

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
)

// Puller is the daemon (engine.Client).
type Puller interface {
	Pull(ctx context.Context, name, reference, platform, registryAuth string, fn func(engine.Event)) error
}

// Layer is one layer's story.
type Layer struct {
	ID     string `json:"id"`
	Total  int64  `json:"bytes,omitempty"`
	Cached bool   `json:"cached,omitempty"` // "Already exists"
	// Times are offsets from the pull's start.
	Started    time.Duration `json:"started_ns,omitempty"`    // first "Downloading"
	Downloaded time.Duration `json:"downloaded_ns,omitempty"` // "Download complete"
	Done       time.Duration `json:"done_ns,omitempty"`       // "Pull complete"
	// Longest is the longest wait between two progress lines while
	// downloading.
	Longest time.Duration `json:"longest_wait_ns,omitempty"`

	lastProgress time.Time
}

// Rate is the layer's download speed in bytes a second (0 if unknown).
func (l *Layer) Rate() float64 {
	d := l.Downloaded - l.Started
	if l.Total <= 0 || d <= 0 {
		return 0
	}
	return float64(l.Total) / d.Seconds()
}

// Gap is a stretch with no progress at all.
type Gap struct {
	At    time.Duration `json:"at_ns"` // when it began, from the start
	Took  time.Duration `json:"took_ns"`
	Phase string        `json:"phase"` // "before the first layer", "downloading", "extracting"
}

// Timeline is the whole pull.
type Timeline struct {
	Took time.Duration `json:"took_ns"`
	// FirstLayer is how long until the daemon had resolved the image and
	// listed its layers: the time spent on mirrors, auth and the manifest.
	FirstLayer time.Duration `json:"first_layer_ns"`
	Layers     []*Layer      `json:"layers"`
	Gaps       []Gap         `json:"gaps,omitempty"`
	Digest     string        `json:"digest,omitempty"`
	Status     string        `json:"status,omitempty"` // "Downloaded newer image…", "Image is up to date…"
	Error      string        `json:"error,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	// Unfinished: still pulling when the time ran out.
	Unfinished bool `json:"unfinished,omitempty"`
}

// Bytes is what was downloaded in all.
func (t *Timeline) Bytes() int64 {
	var n int64
	for _, l := range t.Layers {
		if !l.Cached {
			n += l.Total
		}
	}
	return n
}

// Slowest is the layer with the lowest download rate (nil if none known).
func (t *Timeline) Slowest() *Layer {
	var s *Layer
	for _, l := range t.Layers {
		if l.Rate() > 0 && l.Total >= 1<<20 && (s == nil || l.Rate() < s.Rate()) {
			s = l
		}
	}
	return s
}

// Run pulls and records. stall is the shortest silence worth reporting.
func Run(ctx context.Context, p Puller, name, reference, platform, auth string, stall time.Duration, now func() time.Time) *Timeline {
	if now == nil {
		now = time.Now
	}
	if stall <= 0 {
		stall = 10 * time.Second
	}
	t := &Timeline{}
	start := now()
	last := start
	layers := map[string]*Layer{}
	phase := "before the first layer"
	seenLayer := false
	err := p.Pull(ctx, name, reference, platform, auth, func(e engine.Event) {
		at := e.At
		if at.IsZero() {
			at = now()
		}
		if gap := at.Sub(last); gap >= stall {
			t.Gaps = append(t.Gaps, Gap{At: last.Sub(start), Took: gap, Phase: phase})
		}
		last = at
		switch {
		case strings.HasPrefix(e.Status, "Digest: "):
			t.Digest = strings.TrimPrefix(e.Status, "Digest: ")
			return
		case strings.HasPrefix(e.Status, "Status: "):
			t.Status = strings.TrimPrefix(e.Status, "Status: ")
			return
		case e.ID == "" || strings.HasPrefix(e.Status, "Pulling from"):
			return
		}
		l := layers[e.ID]
		if l == nil {
			l = &Layer{ID: e.ID}
			layers[e.ID] = l
			t.Layers = append(t.Layers, l)
		}
		if !seenLayer {
			seenLayer = true
			t.FirstLayer = at.Sub(start)
			phase = "before the downloads began"
		}
		switch e.Status {
		case "Already exists":
			l.Cached = true
			l.Done = at.Sub(start)
		case "Downloading":
			phase = "downloading"
			if l.Started == 0 {
				l.Started = at.Sub(start)
			} else if gap := at.Sub(l.lastProgress); gap > l.Longest {
				l.Longest = gap
			}
			l.lastProgress = at
			if e.ProgressDetail.Total > 0 {
				l.Total = e.ProgressDetail.Total
			}
		case "Download complete":
			l.Downloaded = at.Sub(start)
			if l.Started == 0 {
				l.Started = l.Downloaded
			}
		case "Extracting":
			phase = "extracting"
		case "Pull complete":
			l.Done = at.Sub(start)
			phase = "after the layers were extracted"
		}
	})
	end := now()
	t.Took = end.Sub(start)
	// A silence at the end (the daemon still fetching, say from a dead
	// mirror) counts too.
	if gap := end.Sub(last); gap >= stall {
		t.Gaps = append(t.Gaps, Gap{At: last.Sub(start), Took: gap, Phase: phase})
	}
	sort.SliceStable(t.Layers, func(i, j int) bool { return t.Layers[i].Total > t.Layers[j].Total })
	if err != nil {
		t.Error = err.Error()
		var de *engine.DaemonError
		if errors.As(err, &de) {
			t.Error = de.Message
		}
		// The daemon's errors can carry signed CDN URLs.
		t.Error = probe.RedactQueries(t.Error)
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Unfinished = true
			t.Kind = probe.KindTimeout
		} else {
			t.Kind = Classify(t.Error)
		}
	}
	return t
}

// KindPlatform: the image has no manifest for the platform asked for.
const KindPlatform = "platform"

// Classify gives the daemon's error message a kind (the probe's kinds, plus
// KindPlatform).
func Classify(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "toomanyrequests") || strings.Contains(m, "rate limit"):
		return probe.KindRateLimited
	case strings.Contains(m, "no matching manifest for"):
		return KindPlatform
	case strings.Contains(m, "certificate signed by unknown authority"):
		return probe.KindTLSAuthority
	case strings.Contains(m, "certificate has expired") || strings.Contains(m, "certificate is not yet valid"):
		return probe.KindTLSExpired
	case strings.Contains(m, "certificate is valid for") || strings.Contains(m, "doesn't contain any ip sans"):
		return probe.KindTLSName
	case strings.Contains(m, "server gave http response to https client"):
		return probe.KindHTTPOnHTTPS
	case strings.Contains(m, "proxyconnect"):
		return probe.KindProxy
	case strings.Contains(m, "no such host") || strings.Contains(m, "server misbehaving"):
		return probe.KindDNS
	case strings.Contains(m, "connection refused"):
		return probe.KindRefused
	case strings.Contains(m, "i/o timeout") || strings.Contains(m, "deadline exceeded") || strings.Contains(m, "client.timeout") ||
		strings.Contains(m, "tls handshake timeout"):
		return probe.KindTimeout
	case strings.Contains(m, "connection reset"):
		return probe.KindReset
	case strings.Contains(m, "no basic auth credentials") || strings.Contains(m, "authorization failed"):
		// A registry (or a mirror, on the containerd store) asking for a
		// login the daemon did not send.
		return probe.KindUnauthorized
	case strings.Contains(m, "pull access denied") || strings.Contains(m, "repository does not exist"):
		// Docker Hub says this both for a repository that does not exist and
		// for a private one without credentials.
		return probe.KindNotFound
	case strings.Contains(m, "manifest unknown") || strings.Contains(m, ": not found"):
		return probe.KindNotFound
	case strings.Contains(m, "unauthorized") || strings.Contains(m, "authentication required") ||
		strings.Contains(m, "no basic auth credentials") || strings.Contains(m, "requested access to the resource is denied"):
		return probe.KindUnauthorized
	case strings.Contains(m, "x509") || strings.Contains(m, "tls:"):
		return probe.KindTLSOther
	}
	return probe.KindOther
}
