package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/deliver"
	"github.com/ganeshphutane-g/pushprobe/internal/runner"
)

// agent is one running configuration.
type agent struct {
	status *runner.Status
	stop   context.CancelFunc
	done   sync.WaitGroup
}

// build makes the jobs and routes for a configuration.
func build(c *config.Config, logf func(string, ...any)) ([]runner.Job, []*deliver.Route, error) {
	var kumaClient, gatusClient *http.Client
	if t := c.Targets.Kuma; t != nil {
		kumaClient = deliver.Client(t.Insecure)
	}
	if t := c.Targets.Gatus; t != nil {
		gatusClient = deliver.Client(t.Insecure)
	}
	var jobs []runner.Job
	var routes []*deliver.Route
	for _, ch := range c.Checks {
		checker, err := check.New(ch)
		if err != nil {
			return nil, nil, err
		}
		j := runner.Job{Check: ch, Checker: checker}
		add := func(s deliver.Sender) {
			r := &deliver.Route{Sender: s, MaxAge: time.Duration(ch.Interval), Log: logf, Backoff: 2 * time.Second}
			j.Routes = append(j.Routes, r)
			routes = append(routes, r)
		}
		if ch.Kuma != nil {
			add(deliver.Kuma{URL: c.Targets.Kuma.URL, Token: ch.Kuma.PushToken, Client: kumaClient})
		}
		if ch.Gatus != nil {
			token := ch.Gatus.Token
			if token == "" {
				token = c.Targets.Gatus.Token
			}
			add(deliver.Gatus{URL: c.Targets.Gatus.URL, Endpoint: ch.Gatus.Endpoint, Token: token, Client: gatusClient})
		}
		jobs = append(jobs, j)
	}
	return jobs, routes, nil
}

// start runs a configuration until stopped.
func start(ctx context.Context, c *config.Config, logf func(string, ...any)) (*agent, error) {
	jobs, routes, err := build(c, logf)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithCancel(ctx)
	a := &agent{status: &runner.Status{Jobs: jobs}, stop: cancel}
	for _, r := range routes {
		a.done.Add(1)
		go func() { defer a.done.Done(); r.Run(actx) }()
	}
	a.done.Add(1)
	go func() { defer a.done.Done(); runner.Schedule(actx, jobs, a.status, logf) }()
	return a, nil
}

func (a *agent) close() {
	a.stop()
	a.done.Wait()
}

// runAgent is `pushprobe run`.
func runAgent(ctx context.Context, env Env, file string, vars func() (func(string) (string, bool), string, error)) error {
	logf := func(f string, a ...any) {
		fmt.Fprintf(env.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(f, a...))
	}
	// Each load reads the env file again, so a token sync just saved is
	// picked up by a reload.
	load := func() (*config.Config, error) {
		data, err := env.ReadFile(file)
		if err != nil {
			return nil, err
		}
		lookup, note, err := vars()
		if err != nil {
			return nil, err
		}
		c, err := config.Load(data, lookup, config.ForRun)
		if err != nil {
			if note != "" {
				return nil, fmt.Errorf("%s: %w\n  (%s)", file, err, note)
			}
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		return c, nil
	}
	c, err := load()
	if err != nil {
		return err
	}
	a, err := start(ctx, c, logf)
	if err != nil {
		return err
	}
	var current atomic.Pointer[agent]
	current.Store(a)
	logf("pushprobe %s started: %s", Version, describe(c))

	if c.Listen != "" {
		l, err := net.Listen("tcp", c.Listen)
		if err != nil {
			a.close()
			return fmt.Errorf("listen %s: %w", c.Listen, err)
		}
		srv := &http.Server{Handler: handler(func() *runner.Status { return current.Load().status }), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(l)
		defer srv.Close()
		logf("serving /healthz and /metrics on %s", l.Addr())
	}

	for {
		select {
		case <-ctx.Done():
			current.Load().close()
			logf("stopped")
			return nil
		case <-env.Reload:
			next, err := load()
			if err != nil {
				logf("reload failed, so the old configuration keeps running: %v", err)
				continue
			}
			if next.Listen != c.Listen {
				logf("note: a change of listen takes effect after a restart")
				next.Listen = c.Listen
			}
			current.Load().close()
			a, err := start(ctx, next, logf)
			if err != nil {
				// The old one is stopped; start it again.
				logf("reload failed, so the old configuration keeps running: %v", err)
				if a, err = start(ctx, c, logf); err != nil {
					return err
				}
			} else {
				c = next
				logf("reloaded: %s", describe(c))
			}
			current.Store(a)
		}
	}
}

// handler serves /healthz and /metrics.
func handler(status func() *runner.Status) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, status())
	})
	return mux
}

func writeMetrics(w interface{ Write([]byte) (int, error) }, st *runner.Status) {
	var b strings.Builder
	metric := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	checks := st.Checks()
	metric("pushprobe_check_up", "gauge", "1 if the check's last run passed, 0 if it failed.")
	for _, c := range checks {
		fmt.Fprintf(&b, "pushprobe_check_up{check=%s} %d\n", label(c.Name), boolInt(c.OK))
	}
	metric("pushprobe_check_duration_seconds", "gauge", "How long the check's last run took.")
	for _, c := range checks {
		fmt.Fprintf(&b, "pushprobe_check_duration_seconds{check=%s} %g\n", label(c.Name), c.Duration.Seconds())
	}
	metric("pushprobe_check_runs_total", "counter", "Runs of the check.")
	for _, c := range checks {
		fmt.Fprintf(&b, "pushprobe_check_runs_total{check=%s} %d\n", label(c.Name), c.Runs)
	}
	metric("pushprobe_check_failures_total", "counter", "Failed runs of the check.")
	for _, c := range checks {
		fmt.Fprintf(&b, "pushprobe_check_failures_total{check=%s} %d\n", label(c.Name), c.Failures)
	}
	metric("pushprobe_deliveries_total", "counter", "Results sent on, by outcome (delivered, failed, dropped).")
	for _, j := range st.Jobs {
		for _, r := range j.Routes {
			s := r.Stats()
			for _, o := range []struct {
				name string
				n    int64
			}{{"delivered", s.Delivered}, {"failed", s.Failed}, {"dropped", s.Dropped}} {
				fmt.Fprintf(&b, "pushprobe_deliveries_total{check=%s,target=%s,outcome=%q} %d\n", label(j.Check.Name), label(r.Sender.Target()), o.name, o.n)
			}
		}
	}
	w.Write([]byte(b.String()))
}

func label(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
