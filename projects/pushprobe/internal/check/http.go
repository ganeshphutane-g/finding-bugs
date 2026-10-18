package check

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

// maxBody is how much of a response is read to look for the expected text.
const maxBody = 1 << 20

type httpCheck struct {
	c      config.HTTP
	client *http.Client
}

func newHTTP(c config.HTTP) httpCheck {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: c.Insecure}
	// Each check sees the service as a new client would.
	t.DisableKeepAlives = true
	client := &http.Client{Transport: t}
	if c.FollowRedirects != nil && !*c.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("more than 10 redirects")
			}
			// The file's headers (often credentials) go only to the address
			// it names: not to another host, and not over plain http.
			first := via[0].URL
			if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
				for k := range c.Headers {
					req.Header.Del(k)
				}
				req.Header.Del("Authorization")
			}
			return nil
		}
	}
	return httpCheck{c: c, client: client}
}

func (h httpCheck) Run(ctx context.Context) Result {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(h.c.Method), h.c.URL, nil)
	if err != nil {
		return fail(start, "bad request: %v", err)
	}
	req.Header.Set("User-Agent", "pushprobe")
	for k, v := range h.c.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) {
			err = uerr.Unwrap()
		}
		return fail(start, "%s", reason(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil && h.c.Contains != "" {
		return fail(start, "status %d, then reading the body: %s", resp.StatusCode, reason(err))
	}
	if !h.statusOK(resp.StatusCode) {
		return fail(start, "status %d, expected %s", resp.StatusCode, h.expected())
	}
	if h.c.Contains != "" && !bytes.Contains(body, []byte(h.c.Contains)) {
		return fail(start, "status %d, but the page does not contain %q", resp.StatusCode, h.c.Contains)
	}
	return ok(start, "status %d", resp.StatusCode)
}

func (h httpCheck) statusOK(code int) bool {
	if len(h.c.ExpectStatus) == 0 {
		return code >= 200 && code < 400
	}
	for _, s := range h.c.ExpectStatus {
		if s == code {
			return true
		}
	}
	return false
}

func (h httpCheck) expected() string {
	if len(h.c.ExpectStatus) == 0 {
		return "200-399"
	}
	return strings.Trim(strings.Join(strings.Fields(fmt.Sprint(h.c.ExpectStatus)), ", "), "[]")
}
