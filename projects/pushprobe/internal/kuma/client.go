// Package kuma talks to Uptime Kuma 2.x through the Socket.IO API its web
// interface uses.
package kuma

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/sio"
)

// Client is a logged-in connection to one instance.
type Client struct {
	BaseURL string
	conn    *sio.Conn
	http    *http.Client

	mu    sync.Mutex
	lists map[string]json.RawMessage // the latest copy of each pushed list
	info  Info
	types []string // monitor types the server announced (nil: none)
	// authState is "loginRequired" or "autoLogin", as the server said on
	// connecting; authReady is closed once it is known.
	authState string
	authReady chan struct{}
}

// Info is what the server says about itself.
type Info struct {
	Version        string `json:"version"`
	DBType         string `json:"dbType"`
	PrimaryBaseURL string `json:"primaryBaseURL"`
	ServerTimezone string `json:"serverTimezone"`
}

// Credentials log in. Token (a JWT from an earlier login) is used when set;
// otherwise Username and Password, with a one-time code from TOTP or
// generated from TOTPSecret when the account has two-factor login.
type Credentials struct {
	Username, Password string
	TOTP, TOTPSecret   string
	Token              string
}

// listEvents are the lists the server pushes after login and after changes.
var listEvents = map[string]bool{
	"monitorList": true, "maintenanceList": true, "notificationList": true, "proxyList": true,
	"dockerHostList": true, "apiKeyList": true, "remoteBrowserList": true, "statusPageList": true,
}

// Connect opens a connection. Log in with Login next.
func Connect(ctx context.Context, baseURL string) (*Client, error) {
	return ConnectWith(ctx, baseURL, nil)
}

// ConnectWith is Connect with an HTTP client of one's own (for TLS settings;
// without a Timeout).
func ConnectWith(ctx context.Context, baseURL string, client *http.Client) (*Client, error) {
	baseURL = strings.TrimSuffix(baseURL, "/")
	// (The websocket library wants a client without a timeout; plain
	// requests get one.)
	plain := &http.Client{Timeout: time.Minute}
	if client != nil {
		plain = &http.Client{Transport: client.Transport, Timeout: time.Minute}
	}
	c := &Client{BaseURL: baseURL, http: plain,
		lists: map[string]json.RawMessage{}, authReady: make(chan struct{})}
	conn, err := sio.DialWith(ctx, baseURL, nil, client, c.onEvent)
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return c, nil
}

func (c *Client) onEvent(name string, args []json.RawMessage) {
	switch {
	case listEvents[name] && len(args) > 0:
		c.mu.Lock()
		c.lists[name] = args[0]
		c.mu.Unlock()
	case name == "info" && len(args) > 0:
		var in Info
		if json.Unmarshal(args[0], &in) == nil {
			c.mu.Lock()
			if in.Version != "" || c.info.Version == "" {
				c.info = in
			}
			c.mu.Unlock()
		}
	case name == "monitorTypeList" && len(args) > 0:
		var pairs [][]json.RawMessage
		if json.Unmarshal(args[0], &pairs) == nil {
			var types []string
			for _, p := range pairs {
				var t string
				if len(p) > 0 && json.Unmarshal(p[0], &t) == nil {
					types = append(types, t)
				}
			}
			c.mu.Lock()
			c.types = types
			c.mu.Unlock()
		}
	case name == "loginRequired" || name == "autoLogin":
		c.mu.Lock()
		if c.authState == "" {
			c.authState = name
			close(c.authReady)
		}
		c.mu.Unlock()
	}
}

// Close ends the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Info is what the server reported about itself (after login).
func (c *Client) Info() Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Login logs in, unless the instance has authentication turned off. It
// returns a token for later logins (empty without authentication).
func (c *Client) Login(ctx context.Context, cr Credentials) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	select {
	case <-c.authReady:
	case <-c.conn.Done():
		return "", c.conn.Err()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	c.mu.Lock()
	state := c.authState
	c.mu.Unlock()
	if state == "autoLogin" {
		return "", nil // lists were sent before autoLogin
	}
	if cr.Token != "" {
		var r result
		if err := c.call(ctx, &r, "loginByToken", cr.Token); err != nil {
			return "", fmt.Errorf("logging in with the token: %w", err)
		}
		return cr.Token, nil
	}
	if cr.Username == "" || cr.Password == "" {
		return "", errors.New("the instance needs a login: set PUSHPROBE_KUMA_USERNAME and PUSHPROBE_KUMA_PASSWORD (or PUSHPROBE_KUMA_TOKEN)")
	}
	var r struct {
		result
		Token         string `json:"token"`
		TokenRequired bool   `json:"tokenRequired"`
	}
	data := map[string]any{"username": cr.Username, "password": cr.Password, "token": ""}
	// The answer asking for a one-time code has no "ok" in it.
	if err := c.Call(ctx, &r, "login", data); err != nil {
		return "", fmt.Errorf("logging in as %s: %w", cr.Username, err)
	}
	if err := r.err(); err != nil && !r.TokenRequired {
		return "", fmt.Errorf("logging in as %s: %w", cr.Username, err)
	}
	if r.TokenRequired {
		code := cr.TOTP
		if code == "" && cr.TOTPSecret != "" {
			var err error
			if code, err = totp(cr.TOTPSecret, time.Now()); err != nil {
				return "", err
			}
		}
		if code == "" {
			return "", fmt.Errorf("%s has two-factor login: set PUSHPROBE_KUMA_TOTP (a one-time code) or PUSHPROBE_KUMA_TOTP_SECRET", cr.Username)
		}
		data["token"] = code
		err := c.call(ctx, &r, "login", data)
		if err != nil && cr.TOTP == "" && r.Msg == "authInvalidToken" {
			// A code works only once: one used by a run moments ago is
			// refused. Wait for the next one.
			now := time.Now()
			next := now.Truncate(30 * time.Second).Add(31 * time.Second)
			select {
			case <-time.After(next.Sub(now)):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			if data["token"], err = totp(cr.TOTPSecret, time.Now()); err == nil {
				err = c.call(ctx, &r, "login", data)
			}
		}
		if err != nil {
			if r.Msg == "authInvalidToken" {
				err = errors.New("the one-time code was not accepted (each code works only once; a run moments ago may have used it)")
			}
			return "", fmt.Errorf("logging in as %s with the one-time code: %w", cr.Username, err)
		}
	}
	return r.Token, nil
}

// result is the usual answer shape: {ok, msg, msgi18n}.
type result struct {
	OK      bool   `json:"ok"`
	Msg     string `json:"msg"`
	MsgI18n bool   `json:"msgi18n"`
}

func (r result) err() error {
	if r.OK {
		return nil
	}
	if r.MsgI18n {
		if m, ok := messages[r.Msg]; ok {
			return errors.New(m)
		}
	}
	if r.Msg == "" {
		return errors.New("the server said no, without a reason")
	}
	msg := Clean(r.Msg, 300)
	if strings.Contains(msg, "Too frequently") {
		msg += " (Uptime Kuma allows about 20 logins a minute, for everyone together; wait a minute, and log in less often with PUSHPROBE_KUMA_TOKEN)"
	}
	return errors.New(msg)
}

// Clean makes text from the server safe to print: control characters
// (which could move the cursor or rename the terminal) become "?", and it
// is cut to max bytes.
func Clean(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
		if b.Len() > max {
			return b.String() + "..."
		}
	}
	return b.String()
}

// messages are English texts for the server's translation keys.
var messages = map[string]string{
	"authIncorrectCreds":        "incorrect username or password",
	"authInvalidToken":          "the token or one-time code is not valid",
	"authUserInactiveOrDeleted": "the user is inactive or deleted",
	"passwordTooWeak":           "the password is too weak",
}

type okResult interface{ err() error }

// call sends an event and decodes the answer into out, which must embed
// result; an answer with ok false is an error.
func (c *Client) call(ctx context.Context, out okResult, event string, args ...any) error {
	res, err := c.conn.Call(ctx, event, args...)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("%s: no answer", event)
	}
	if err := json.Unmarshal(res[0], out); err != nil {
		return fmt.Errorf("%s: unreadable answer: %w", event, err)
	}
	return out.err()
}

// Call sends any event and decodes its first answer argument (for tests
// and tools).
func (c *Client) Call(ctx context.Context, out any, event string, args ...any) error {
	res, err := c.conn.Call(ctx, event, args...)
	if err != nil {
		return err
	}
	if out == nil || len(res) == 0 {
		return nil
	}
	return json.Unmarshal(res[0], out)
}

// list returns the latest pushed copy of a list.
func (c *Client) list(name string) json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists[name]
}

// Refresh asks for a fresh copy of the lists: the server re-sends them when
// the monitor list is requested and after every change, but to be sure the
// connection is renewed.
func (c *Client) Refresh(ctx context.Context) error {
	var r struct {
		result
	}
	return c.call(ctx, &r, "getMonitorList")
}

// HTTP fetches a JSON page of the instance's web API.
func (c *Client) HTTP(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	// Some pages are cached by the server for minutes; ask for a fresh one.
	req.Header.Set("x-apicache-bypass", "true")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %.200s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	return json.Unmarshal(data, out)
}

// Fetch reads a file of the instance (up to max bytes) and its type.
func (c *Client) Fetch(ctx context.Context, path string, max int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > max {
		return nil, "", fmt.Errorf("GET %s: larger than %d bytes", path, max)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// PathEscape escapes a URL path segment.
func PathEscape(s string) string { return url.PathEscape(s) }

// totp makes a time-based one-time code (RFC 6238: SHA-1, 6 digits, 30 s).
func totp(secret string, now time.Time) (string, error) {
	s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return "", errors.New("the TOTP secret is not valid base32")
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(now.Unix()/30))
	h := hmac.New(sha1.New, key)
	h.Write(msg[:])
	sum := h.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000), nil
}

// Do sends a change and returns the server's answer, or its refusal as an
// error.
func (c *Client) Do(ctx context.Context, event string, args ...any) (Object, error) {
	var r struct {
		result
		rest Object
	}
	res, err := c.conn.Call(ctx, event, args...)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("%s: no answer", event)
	}
	if err := decode(res[0], &r.rest); err != nil {
		return nil, fmt.Errorf("%s: unreadable answer: %w", event, err)
	}
	r.OK, _ = r.rest["ok"].(bool)
	r.Msg, _ = r.rest["msg"].(string)
	r.MsgI18n, _ = r.rest["msgi18n"].(bool)
	if err := r.err(); err != nil {
		return nil, err
	}
	return r.rest, nil
}
