// Package deliver sends check results to Uptime Kuma and Gatus.
package deliver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
)

// Report is one result of one check.
type Report struct {
	Check  string
	Result check.Result
	At     time.Time
}

// Sender delivers a report to one place.
type Sender interface {
	// Send delivers it. An error wrapped in Permanent will not get better by
	// trying again (a wrong token, a deleted monitor).
	Send(ctx context.Context, r Report) error
	// Target names where it goes, for logs: "Uptime Kuma", "Gatus".
	Target() string
}

// PermanentError will not get better by retrying.
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports a PermanentError.
func IsPermanent(err error) bool {
	var p PermanentError
	return errors.As(err, &p)
}

// Client is an HTTP client for one server.
func Client(insecure bool) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure}
	return &http.Client{Transport: t, Timeout: 15 * time.Second,
		// A push answered with a redirect would lose its method or token.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Kuma sends to an Uptime Kuma push monitor.
type Kuma struct {
	URL    string // the server, e.g. https://kuma.example.com
	Token  string
	Client *http.Client
}

func (k Kuma) Target() string { return "Uptime Kuma" }

// Send calls /api/push/<token>?status=up|down&msg=…&ping=….
func (k Kuma) Send(ctx context.Context, r Report) error {
	q := url.Values{"msg": {r.Result.Message}}
	if r.Result.OK {
		q.Set("status", "up")
		q.Set("ping", strconv.FormatInt(max(r.Result.Duration.Milliseconds(), 0), 10))
	} else {
		q.Set("status", "down")
	}
	u := strings.TrimSuffix(k.URL, "/") + "/api/push/" + url.PathEscape(k.Token) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return PermanentError{errors.New("bad Kuma address")}
	}
	return do(k.Client, req, func(code int, body string) error {
		if code == http.StatusNotFound {
			return PermanentError{errors.New("Uptime Kuma has no active push monitor with this token (it was deleted or is paused)")}
		}
		return nil
	})
}

// Gatus sends to a Gatus external endpoint.
type Gatus struct {
	URL      string // the server, e.g. https://status.example.com
	Endpoint string // the endpoint's key: group_name
	Token    string
	Client   *http.Client
}

func (g Gatus) Target() string { return "Gatus" }

// Send calls POST /api/v1/endpoints/<key>/external?success=…&error=…&duration=….
func (g Gatus) Send(ctx context.Context, r Report) error {
	q := url.Values{"success": {strconv.FormatBool(r.Result.OK)}, "duration": {r.Result.Duration.Round(time.Millisecond).String()}}
	if !r.Result.OK {
		q.Set("error", r.Result.Message)
	}
	u := strings.TrimSuffix(g.URL, "/") + "/api/v1/endpoints/" + url.PathEscape(g.Endpoint) + "/external?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return PermanentError{errors.New("bad Gatus address")}
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	return do(g.Client, req, func(code int, body string) error {
		switch code {
		case http.StatusUnauthorized:
			return PermanentError{errors.New("Gatus refused the token")}
		case http.StatusNotFound:
			return PermanentError{fmt.Errorf("Gatus has no external endpoint %q", g.Endpoint)}
		}
		return nil
	})
}

// do sends a request. A 2xx answer is delivered; special picks out
// answers with a meaning of their own; other 4xx answers are permanent, and
// 5xx, 429 and network errors are worth retrying. Errors never contain the
// URL, which holds the token.
func do(c *http.Client, req *http.Request, special func(code int, body string) error) error {
	resp, err := c.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return errors.New("timed out")
		}
		return errors.New(strings.TrimPrefix(err.Error(), "dial tcp "))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	body := strings.TrimSpace(string(data))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if err := special(resp.StatusCode, body); err != nil {
		return err
	}
	err = fmt.Errorf("the server answered %s", resp.Status)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
		return PermanentError{err}
	}
	return err
}

// Route delivers one check's results to one place. It keeps only the newest
// result not yet delivered: Kuma and Gatus record a result when it arrives,
// so replaying old ones after an outage would mislead.
type Route struct {
	Sender Sender
	// MaxAge is how old a result may get before it is dropped instead of
	// sent (a newer one is due by then).
	MaxAge time.Duration
	// Log reports delivery trouble and recovery, once each.
	Log func(format string, a ...any)
	// Backoff is the first pause after a failed delivery; it doubles up to
	// MaxAge/2.
	Backoff time.Duration

	mu      sync.Mutex
	pending *Report
	wake    chan struct{}
	stats   Stats
}

// Stats count deliveries.
type Stats struct {
	Delivered, Failed, Dropped int64
	// Failing is set while deliveries fail.
	Failing bool
	LastErr string
}

// Put hands over a result; it replaces any not yet delivered.
func (r *Route) Put(rep Report) {
	r.mu.Lock()
	if r.pending != nil {
		r.stats.Dropped++
	}
	r.pending = &rep
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Stats returns the counts so far.
func (r *Route) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

func (r *Route) take() *Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending
	r.pending = nil
	return p
}

// putBack keeps a result that failed to go out, unless a newer one came.
func (r *Route) putBack(rep *Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = rep
	}
}

// Run delivers until ctx ends.
func (r *Route) Run(ctx context.Context) {
	r.mu.Lock()
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
	}
	wake := r.wake
	r.mu.Unlock()
	backoff := r.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	pause := backoff
	for {
		rep := r.take()
		if rep == nil {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				continue
			}
		}
		if r.MaxAge > 0 && time.Since(rep.At) > r.MaxAge {
			r.mu.Lock()
			r.stats.Dropped++
			r.mu.Unlock()
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := r.Sender.Send(sctx, *rep)
		cancel()
		if ctx.Err() != nil {
			return
		}
		r.mu.Lock()
		wasFailing, lastErr := r.stats.Failing, r.stats.LastErr
		if err == nil {
			r.stats.Delivered++
			r.stats.Failing, r.stats.LastErr = false, ""
			r.mu.Unlock()
			if wasFailing {
				r.logf("%s: delivered to %s again", rep.Check, r.Sender.Target())
			}
			pause = backoff
			continue
		}
		r.stats.Failed++
		r.stats.Failing, r.stats.LastErr = true, err.Error()
		r.mu.Unlock()
		if !wasFailing || lastErr != err.Error() {
			r.logf("%s: cannot deliver to %s: %v", rep.Check, r.Sender.Target(), err)
		}
		if IsPermanent(err) {
			// Retrying the same will not help; the next result tries again.
			pause = backoff
			continue
		}
		r.putBack(rep)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		limit := r.MaxAge / 2
		if limit <= 0 {
			limit = 5 * time.Minute
		}
		if pause*2 <= limit {
			pause *= 2
		}
	}
}

func (r *Route) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}
