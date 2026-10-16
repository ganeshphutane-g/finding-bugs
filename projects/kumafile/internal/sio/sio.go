// Package sio is a small Socket.IO client (protocol v5 over Engine.IO v4,
// WebSocket transport only), with what talking to Uptime Kuma needs: events
// in both directions and acknowledgements.
package sio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Conn is a connection to the default namespace.
type Conn struct {
	ws *websocket.Conn

	wmu sync.Mutex // serialises writes

	mu      sync.Mutex
	nextID  int
	pending map[int]chan []json.RawMessage
	onEvent func(name string, args []json.RawMessage)
	err     error // why the connection ended
	done    chan struct{}

	connected chan struct{}
}

// Dial connects to a Socket.IO server at base (http or https URL of the
// site; the /socket.io/ path is added). onEvent receives every event the
// server sends, in order, on the reading goroutine: it must not block on
// the connection.
func Dial(ctx context.Context, base string, header http.Header, onEvent func(string, []json.RawMessage)) (*Conn, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("%q is not an http(s) URL", base)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/socket.io/"
	u.RawQuery = "EIO=4&transport=websocket"
	ws, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", base, err)
	}
	ws.SetReadLimit(256 << 20)
	c := &Conn{ws: ws, pending: map[int]chan []json.RawMessage{}, onEvent: onEvent,
		done: make(chan struct{}), connected: make(chan struct{})}

	// Engine.IO open packet, then connect to the namespace.
	typ, data, err := ws.Read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, fmt.Errorf("connecting to %s: %w", base, err)
	}
	if typ != websocket.MessageText || len(data) == 0 || data[0] != '0' {
		ws.CloseNow()
		return nil, fmt.Errorf("connecting to %s: not a Socket.IO server", base)
	}
	if err := c.write(ctx, "40"); err != nil {
		ws.CloseNow()
		return nil, err
	}
	go c.readLoop()
	select {
	case <-c.connected:
		return c, nil
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}

func (c *Conn) write(ctx context.Context, s string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, []byte(s))
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// Err is why the connection ended (nil while it is open).
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Close ends the connection.
func (c *Conn) Close() error {
	c.fail(errors.New("connection closed"))
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *Conn) readLoop() {
	ctx := context.Background()
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			c.fail(fmt.Errorf("connection lost: %w", err))
			return
		}
		if typ != websocket.MessageText || len(data) == 0 {
			continue // binary attachments are not used by Uptime Kuma
		}
		switch data[0] {
		case '2': // ping
			if err := c.write(ctx, "3"); err != nil {
				c.fail(err)
				return
			}
		case '1': // close
			c.fail(errors.New("the server closed the connection"))
			return
		case '4':
			if err := c.handle(string(data[1:])); err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// handle reads one Socket.IO packet: type digit, optional namespace,
// optional ack id, JSON payload.
func (c *Conn) handle(p string) error {
	if p == "" {
		return nil
	}
	typ, rest := p[0], p[1:]
	if strings.HasPrefix(rest, "/") { // a namespace other than "/"
		if i := strings.IndexByte(rest, ','); i >= 0 {
			rest = rest[i+1:]
		}
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	id := -1
	if i > 0 {
		id, _ = strconv.Atoi(rest[:i])
	}
	payload := rest[i:]
	switch typ {
	case '0': // connected
		select {
		case <-c.connected:
		default:
			close(c.connected)
		}
	case '1':
		return errors.New("the server disconnected this client")
	case '4':
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal([]byte(payload), &e)
		return fmt.Errorf("the server refused the connection: %s", e.Message)
	case '2': // event
		var args []json.RawMessage
		if err := json.Unmarshal([]byte(payload), &args); err != nil || len(args) == 0 {
			return fmt.Errorf("unreadable event %.80q", payload)
		}
		var name string
		if err := json.Unmarshal(args[0], &name); err != nil {
			return fmt.Errorf("unreadable event name %.80q", payload)
		}
		if c.onEvent != nil {
			c.onEvent(name, args[1:])
		}
	case '3': // ack
		var args []json.RawMessage
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return fmt.Errorf("unreadable answer %.80q", payload)
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- args
		}
	}
	return nil
}

// Call sends an event with arguments and waits for the server's answer
// (its acknowledgement arguments).
func (c *Conn) Call(ctx context.Context, event string, args ...any) ([]json.RawMessage, error) {
	payload, err := json.Marshal(append([]any{event}, args...))
	if err != nil {
		return nil, err
	}
	ch := make(chan []json.RawMessage, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}
	id := c.nextID
	c.nextID++
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(ctx, "42"+strconv.Itoa(id)+string(payload)); err != nil {
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, c.Err()
		}
		return res, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Emit sends an event without waiting for an answer.
func (c *Conn) Emit(ctx context.Context, event string, args ...any) error {
	payload, err := json.Marshal(append([]any{event}, args...))
	if err != nil {
		return err
	}
	return c.write(ctx, "42"+string(payload))
}

// DefaultTimeout bounds a single call when the caller's context has none.
const DefaultTimeout = 2 * time.Minute
