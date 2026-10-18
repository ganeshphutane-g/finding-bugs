package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/deliver"
)

// toggle fails while down is set.
type toggle struct{ down atomic.Bool }

func (c *toggle) Run(context.Context) check.Result {
	if c.down.Load() {
		return check.Result{Message: "connection refused"}
	}
	return check.Result{OK: true, Message: "connected"}
}

type sink struct {
	mu  sync.Mutex
	got []bool
}

func (s *sink) Target() string { return "sink" }
func (s *sink) Send(_ context.Context, r deliver.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r.Result.OK)
	return nil
}
func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.got) }

func TestSchedule(t *testing.T) {
	MaxStartDelay, RetryPause = time.Millisecond, time.Millisecond
	zero := 0
	c := &toggle{}
	snk := &sink{}
	route := &deliver.Route{Sender: snk, MaxAge: time.Second}
	job := Job{Check: config.Check{Name: "printer", Interval: config.Duration(20 * time.Millisecond), Timeout: config.Duration(10 * time.Millisecond), Retries: &zero},
		Checker: c, Routes: []*deliver.Route{route}}
	var logs []string
	var mu sync.Mutex
	logf := func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	st := &Status{}
	ctx, cancel := context.WithCancel(context.Background())
	go route.Run(ctx)
	done := make(chan struct{})
	go func() { Schedule(ctx, []Job{job}, st, logf); close(done) }()
	wait := func(cond func() bool) {
		deadline := time.Now().Add(3 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(func() bool { return snk.count() >= 3 })
	c.down.Store(true)
	wait(func() bool { s := st.Checks(); return len(s) == 1 && !s[0].OK })
	c.down.Store(false)
	wait(func() bool { s := st.Checks(); return s[0].OK })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 3 || logs[0] != "printer: up (connected)" || logs[1] != "printer: now DOWN (connection refused)" || logs[2] != "printer: now up (connected)" {
		t.Errorf("logs: %q", logs)
	}
	s := st.Checks()[0]
	if s.Runs < 5 || s.Failures < 1 || !strings.Contains(s.Message, "connected") {
		t.Errorf("status %+v", s)
	}
}
