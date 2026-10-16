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

	"github.com/ganeshphutane-g/kumafile/internal/sio"
)

// Client is a logged-in connection to one instance.
type Client struct {
	BaseURL string
	conn    *sio.Conn
	http    *http.Client

	mu    sync.Mutex
	lists map[string]json.RawMessage // the latest copy of each pushed list
	info  Info
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
	baseURL = strings.TrimSuffix(baseURL, "/")
	c := &Client{BaseURL: baseURL, http: &http.Client{Timeout: time.Minute},
		lists: map[string]json.RawMessage{}, authReady: make(chan struct{})}
	conn, err := sio.Dial(ctx, baseURL, nil, c.onEvent)
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
		return "", errors.New("the instance needs a login: set a username and password (or a token)")
	}
	var r struct {
		result
		Token         string `json:"token"`
		TokenRequired bool   `json:"tokenRequired"`
	}
	data := map[string]any{"username": cr.Username, "password": cr.Password, "token": ""}
	if err := c.call(ctx, &r, "login", data); err != nil {
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
			return "", fmt.Errorf("%s has two-factor login: give a one-time code or the TOTP secret", cr.Username)
		}
		data["token"] = code
		if err := c.call(ctx, &r, "login", data); err != nil {
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
	return errors.New(r.Msg)
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
