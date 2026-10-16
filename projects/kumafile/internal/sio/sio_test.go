package sio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeServer speaks just enough Engine.IO/Socket.IO: it opens, pings,
// pushes an event and answers "echo" calls.
func fakeServer(t *testing.T, pongs chan<- bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket.io/" || r.URL.Query().Get("EIO") != "4" {
			http.Error(w, "bad path", 400)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		ws.Write(ctx, websocket.MessageText, []byte(`0{"sid":"x","pingInterval":25000,"pingTimeout":20000}`))
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			msg := string(data)
			switch {
			case msg == "40":
				ws.Write(ctx, websocket.MessageText, []byte(`40{"sid":"y"}`))
				ws.Write(ctx, websocket.MessageText, []byte(`2`))
				ws.Write(ctx, websocket.MessageText, []byte(`42["hello",{"n":1}]`))
			case msg == "3":
				pongs <- true
			case strings.HasPrefix(msg, "42"):
				i := 2
				for msg[i] >= '0' && msg[i] <= '9' {
					i++
				}
				id := msg[2:i]
				var args []json.RawMessage
				json.Unmarshal([]byte(msg[i:]), &args)
				ws.Write(ctx, websocket.MessageText, []byte("43"+id+"["+string(args[1])+"]"))
			}
		}
	}))
}

func TestCallsEventsAndPings(t *testing.T) {
	pongs := make(chan bool, 1)
	srv := fakeServer(t, pongs)
	defer srv.Close()
	var mu sync.Mutex
	var events []string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, srv.URL, nil, func(name string, args []json.RawMessage) {
		mu.Lock()
		events = append(events, name+" "+string(args[0]))
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 3; i++ {
		res, err := c.Call(ctx, "echo", map[string]any{"i": i})
		if err != nil || len(res) != 1 || string(res[0]) != `{"i":`+string(rune('0'+i))+`}` {
			t.Fatalf("call %d: %s %v", i, res, err)
		}
	}
	select {
	case <-pongs:
	case <-ctx.Done():
		t.Fatal("no pong")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != `hello {"n":1}` {
		t.Fatalf("events %q", events)
	}
}

func TestNotSocketIO(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, srv.URL, nil, nil); err == nil {
		t.Fatal("dialled a server that is not Socket.IO")
	}
}

func TestPendingCallsEndWithTheConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		ws.Write(ctx, websocket.MessageText, []byte(`0{"sid":"x"}`))
		ws.Read(ctx)
		ws.Write(ctx, websocket.MessageText, []byte(`40{"sid":"y"}`))
		ws.Read(ctx) // the call, never answered
		ws.Close(websocket.StatusGoingAway, "bye")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, srv.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, "x"); err == nil || ctx.Err() != nil {
		t.Fatalf("got %v (ctx %v)", err, ctx.Err())
	}
}

// silentServer accepts the WebSocket and then says nothing more than
// wanted.
func silentServer(t *testing.T, openAndConnect bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		if openAndConnect {
			ws.Write(ctx, websocket.MessageText, []byte(`0{"sid":"x","pingInterval":300,"pingTimeout":200}`))
			ws.Read(ctx)
			ws.Write(ctx, websocket.MessageText, []byte(`40{"sid":"y"}`))
		}
		<-ctx.Done()
	}))
}

func TestNothingHangsForever(t *testing.T) {
	HandshakeTimeout, CallTimeout = 300*time.Millisecond, 300*time.Millisecond
	defer func() { HandshakeTimeout, CallTimeout = 30*time.Second, 2*time.Minute }()
	mute := silentServer(t, false)
	defer mute.Close()
	start := time.Now()
	if _, err := Dial(context.Background(), mute.URL, nil, nil); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("a server that never opens: %v after %s", err, time.Since(start))
	}
	quiet := silentServer(t, true)
	defer quiet.Close()
	c, err := Dial(context.Background(), quiet.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("a call never answered: %v", err)
	}
	// No pings for longer than the server promised: the connection is
	// given up.
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a silent connection was kept")
	}
}
