package deliver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
)

func report(ok bool, msg string) Report {
	return Report{Check: "nas", Result: check.Result{OK: ok, Message: msg, Duration: 1234 * time.Microsecond}, At: time.Now()}
}

func TestKumaSend(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sub/api/push/tok123" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"ok":false,"msg":"Monitor not found or not active."}`)
			return
		}
		got = append(got, r.URL.RawQuery)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	k := Kuma{URL: srv.URL + "/sub/", Token: "tok123", Client: Client(false)}
	if err := k.Send(context.Background(), report(true, "status 200")); err != nil {
		t.Fatal(err)
	}
	if err := k.Send(context.Background(), report(false, "connection refused")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " | ") != "msg=status+200&ping=1&status=up | msg=connection+refused&status=down" {
		t.Errorf("%q", got)
	}
	k.Token = "gone"
	err := k.Send(context.Background(), report(true, ""))
	if !IsPermanent(err) || !strings.Contains(err.Error(), "deleted or is paused") || strings.Contains(err.Error(), "gone") {
		t.Errorf("%v", err)
	}
}

func TestGatusSend(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer g-token" {
			w.WriteHeader(401)
			fmt.Fprint(w, "invalid token")
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/endpoints/office_printer/external" {
			w.WriteHeader(404)
			return
		}
		got = append(got, r.URL.RawQuery)
	}))
	defer srv.Close()
	g := Gatus{URL: srv.URL, Endpoint: "office_printer", Token: "g-token", Client: Client(false)}
	g.Send(context.Background(), report(true, "connected"))
	g.Send(context.Background(), report(false, "connection refused"))
	if strings.Join(got, " | ") != "duration=1ms&success=true | duration=1ms&error=connection+refused&success=false" {
		t.Errorf("%q", got)
	}
	g.Token = "wrong"
	if err := g.Send(context.Background(), report(true, "")); !IsPermanent(err) || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("%v", err)
	}
	g.Token, g.Endpoint = "g-token", "nope"
	if err := g.Send(context.Background(), report(true, "")); !IsPermanent(err) || !strings.Contains(err.Error(), `no external endpoint "nope"`) {
		t.Errorf("%v", err)
	}
}

func TestErrorsHideTheToken(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	k := Kuma{URL: "http://" + addr, Token: "SECRET-TOKEN", Client: Client(false)}
	err := k.Send(context.Background(), report(true, ""))
	if err == nil || IsPermanent(err) || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("%v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer srv.Close()
	k.URL = srv.URL
	if err := k.Send(context.Background(), report(true, "")); err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "502") {
		t.Errorf("502: %v", err)
	}
}

// fake records what it gets and fails while down is set.
type fake struct {
	mu   sync.Mutex
	got  []string
	down atomic.Bool
	perm bool
}

func (f *fake) Target() string { return "fake" }

func (f *fake) Send(ctx context.Context, r Report) error {
	if f.down.Load() {
		if f.perm {
			return PermanentError{fmt.Errorf("no such monitor")}
		}
		return fmt.Errorf("connection refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r.Result.Message)
	return nil
}

func (f *fake) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRouteKeepsTheNewestWhileDown(t *testing.T) {
	f := &fake{}
	var logs []string
	var logMu sync.Mutex
	r := &Route{Sender: f, MaxAge: time.Minute, Backoff: 10 * time.Millisecond,
		Log: func(format string, a ...any) {
			logMu.Lock()
			logs = append(logs, fmt.Sprintf(format, a...))
			logMu.Unlock()
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Put(report(true, "one"))
	waitFor(t, func() bool { return len(f.messages()) == 1 })

	f.down.Store(true)
	r.Put(report(false, "two"))
	waitFor(t, func() bool { return r.Stats().Failed >= 2 })
	r.Put(report(true, "three"))
	r.Put(report(true, "four"))
	f.down.Store(false)
	waitFor(t, func() bool { return len(f.messages()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := strings.Join(f.messages(), ","); got != "one,four" {
		t.Errorf("delivered %s", got)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if len(logs) != 2 || !strings.Contains(logs[0], "cannot deliver to fake: connection refused") || !strings.Contains(logs[1], "delivered to fake again") {
		t.Errorf("logs %q", logs)
	}
}

func TestRouteDropsOldResults(t *testing.T) {
	f := &fake{}
	r := &Route{Sender: f, MaxAge: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := report(true, "old")
	old.At = time.Now().Add(-time.Hour)
	r.Put(old)
	go r.Run(ctx)
	waitFor(t, func() bool { return r.Stats().Dropped == 1 })
	if len(f.messages()) != 0 {
		t.Error("sent an old result")
	}
}

func TestRouteDoesNotRetryPermanentErrors(t *testing.T) {
	f := &fake{perm: true}
	f.down.Store(true)
	r := &Route{Sender: f, MaxAge: time.Minute, Backoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.Put(report(true, "x"))
	waitFor(t, func() bool { return r.Stats().Failed == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := r.Stats().Failed; n != 1 {
		t.Errorf("retried a permanent error: %d failures", n)
	}
}
