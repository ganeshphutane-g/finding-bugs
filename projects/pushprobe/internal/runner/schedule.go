package runner

import (
	"context"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/deliver"
)

// Job is a check with where its results go.
type Job struct {
	Check   config.Check
	Checker check.Checker
	Routes  []*deliver.Route
}

// CheckStatus is the latest about one check.
type CheckStatus struct {
	Name     string
	Known    bool // it has run at least once
	OK       bool
	Message  string
	Duration time.Duration
	At       time.Time
	Runs     int64
	Failures int64
}

// Status is the latest about every check, for /healthz and /metrics.
type Status struct {
	mu     sync.Mutex
	checks map[string]*CheckStatus
	Jobs   []Job
}

// Checks returns a copy, sorted by name.
func (s *Status) Checks() []CheckStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckStatus
	for _, c := range s.checks {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// record stores a result and reports whether the check changed state.
func (s *Status) record(name string, r check.Result, at time.Time) (changed, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checks == nil {
		s.checks = map[string]*CheckStatus{}
	}
	c := s.checks[name]
	if c == nil {
		c = &CheckStatus{Name: name}
		s.checks[name] = c
	}
	first = !c.Known
	changed = first || c.OK != r.OK
	c.Known, c.OK, c.Message, c.Duration, c.At = true, r.OK, r.Message, r.Duration, at
	c.Runs++
	if !r.OK {
		c.Failures++
	}
	return changed, first
}

// MaxStartDelay bounds the random wait before a check's first run, which
// spreads checks out.
var MaxStartDelay = 5 * time.Second

// Schedule runs every job on its interval until ctx ends, and hands each
// result to the job's routes. logf gets one line per change of state.
func Schedule(ctx context.Context, jobs []Job, st *Status, logf func(string, ...any)) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runJob(ctx, j, st, logf)
		}()
	}
	wg.Wait()
}

func runJob(ctx context.Context, j Job, st *Status, logf func(string, ...any)) {
	interval := time.Duration(j.Check.Interval)
	delay := time.Duration(0)
	if limit := min(interval, MaxStartDelay); limit > 0 {
		delay = time.Duration(rand.Int64N(int64(limit)))
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		r, attempts := Once(ctx, j.Checker, time.Duration(j.Check.Timeout), *j.Check.Retries)
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if changed, first := st.record(j.Check.Name, r, now); changed {
			state := "up"
			if !r.OK {
				state = "DOWN"
			}
			tries := ""
			if attempts > 1 {
				tries = " after " + strconv.Itoa(attempts) + " attempts"
			}
			if first {
				logf("%s: %s (%s)%s", j.Check.Name, state, r.Message, tries)
			} else {
				logf("%s: now %s (%s)%s", j.Check.Name, state, r.Message, tries)
			}
		}
		for _, route := range j.Routes {
			route.Put(deliver.Report{Check: j.Check.Name, Result: r, At: now})
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
