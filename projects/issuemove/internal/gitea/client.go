// Package gitea is a small client for the Gitea and Forgejo API (v1), with
// only what issuemove needs.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one instance.
type Client struct {
	BaseURL string // e.g. https://git.example.com
	Token   string
	HTTP    *http.Client
	// Sudo, when set, makes requests as that user (site admins only).
	Sudo string
}

// New makes a client for an instance.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token,
		HTTP: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: keepTokenHome}}
}

// keepTokenHome drops the token and Sudo headers when a redirect leaves the
// origin (scheme, host or port) the request was made to. (Go itself drops
// them only when the host name changes.)
func keepTokenHome(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		req.Header.Del("Authorization")
		req.Header.Del("Sudo")
	}
	return nil
}

// retries is how often a failed read is tried again.
var retries = []time.Duration{time.Second, 3 * time.Second, 10 * time.Second}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

// As returns a client acting as user (empty: as the token's own user).
func (c *Client) As(user string) *Client {
	cp := *c
	cp.Sudo = user
	return &cp
}

// APIError is a non-2xx answer.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsStatus reports a given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body io.Reader, ctype string, out any) (http.Header, error) {
	// Reads are retried when the server is busy or the connection fails;
	// writes are not, as a write that did happen would be repeated.
	for attempt := 0; ; attempt++ {
		h, err := c.do1(ctx, method, path, query, body, ctype, out)
		if method != http.MethodGet || attempt == len(retries) || ctx.Err() != nil {
			return h, err
		}
		var ae *APIError
		if err == nil || (errors.As(err, &ae) && !retryable(ae.Status)) {
			return h, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retries[attempt]):
		}
	}
}

func (c *Client) do1(ctx context.Context, method, path string, query url.Values, body io.Reader, ctype string, out any) (http.Header, error) {
	u := c.BaseURL + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	if c.Sudo != "" {
		req.Header.Set("Sudo", c.Sudo)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		if resp.StatusCode == http.StatusUnauthorized {
			msg = "the token was refused; check that it is right and has not expired"
		}
		return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

// get reads one object.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.do(ctx, http.MethodGet, path, query, nil, "", out)
	return err
}

// send sends a JSON body.
func (c *Client) send(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	_, err := c.do(ctx, method, path, nil, body, "application/json", out)
	return err
}

// list reads every page of a list endpoint.
func list[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	if query == nil {
		query = url.Values{}
	}
	const limit = 50
	var all []T
	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))
		query.Set("limit", strconv.Itoa(limit))
		var items []T
		h, err := c.do(ctx, http.MethodGet, path, query, nil, "", &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if total, err := strconv.Atoi(h.Get("X-Total-Count")); err == nil {
			if len(all) >= total {
				return all, nil
			}
		} else if len(items) < limit {
			return all, nil
		}
		if len(items) == 0 {
			return all, nil
		}
	}
}

// upload sends a file as multipart form data.
func (c *Client) upload(ctx context.Context, path, field, name string, content io.Reader, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, content); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, path, url.Values{"name": {name}}, &buf, w.FormDataContentType(), out)
	return err
}

// DownloadAttachment fetches an attachment's content by its UUID, from
// this instance's /attachments/ route (so the token goes to the configured
// instance, whatever URL the server reports, and is dropped if the server
// redirects elsewhere).
func (c *Client) DownloadAttachment(ctx context.Context, uuid string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/attachments/"+url.PathEscape(uuid), nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading attachment %s: HTTP %d", uuid, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<30))
}

// Raw sends any JSON request (for tools and tests).
func (c *Client) Raw(ctx context.Context, method, path string, in, out any) error {
	return c.send(ctx, method, path, in, out)
}
