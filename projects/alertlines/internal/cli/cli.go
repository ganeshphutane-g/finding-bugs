// Package cli is alertlines' command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/config"
	"github.com/ganeshphutane-g/alertlines/internal/enrich"
	"github.com/ganeshphutane-g/alertlines/internal/loki"
	"github.com/ganeshphutane-g/alertlines/internal/redact"
)

// Version is set at build time.
var Version = "dev"

// Env is what the command line works with, replaceable in tests.
type Env struct {
	Stdout, Stderr io.Writer
	LookupEnv      func(string) (string, bool)
	ReadFile       func(string) ([]byte, error)
}

// DefaultEnv uses the process's own.
func DefaultEnv() Env {
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.LookupEnv, ReadFile: os.ReadFile}
}

const usage = `Usage: alertlines COMMAND [OPTIONS]

Adds the matching log lines to the alerts Loki's ruler sends to Alertmanager.
Point the ruler's alertmanager_url at alertlines; it passes every alert on.

Commands:
  run       Take alerts from the ruler (POST /api/v2/alerts), add log lines
            to the firing ones and pass them all on to Alertmanager
  check     Show the query and the lines alertlines would add to an alert:
              alertlines check --expr 'sum by (app) (count_over_time({app="shop"} |= "ERROR" [5m])) > 0' --label app=shop
              alertlines check --alert alert.json   (an alert as Alertmanager's API shows it)
  version   Print the version

Options:
  -c, --config FILE      The file (default $ALERTLINES_CONFIG, else alertlines.yaml, if there)
      --loki URL         Loki (instead of loki.url)
      --tenant NAME      The tenant to ask Loki as (check)
      --listen ADDR      Where to take alerts (run; default 127.0.0.1:9097)
      --alertmanager URL An Alertmanager to pass alerts on to (run; repeat for more)

The file may use ${NAME} for variables whose names start with ALERTLINES_.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 no lines found (check), 4 no log
query for the alert (check).
`

var (
	errUsage   = errors.New("usage")
	errNoLines = errors.New("no lines")
	errNoQuery = errors.New("no log query")
)

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

// Run runs alertlines and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errNoLines):
		return 3
	case errors.Is(err, errNoQuery):
		return 4
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "alertlines: %s\nRun 'alertlines help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	default:
		fmt.Fprintf(env.Stderr, "alertlines: %v\n", err)
		return 1
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return usagef("name a command")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "alertlines %s\n", Version)
		return nil
	case "check", "run":
	default:
		return usagef("unknown command %q", cmd)
	}
	fs := flag.NewFlagSet("alertlines "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var file, lokiURL, expr, alertFile, tenant, listen string
	var labels, alertmanagers multi
	fs.StringVar(&file, "c", "", "")
	fs.StringVar(&file, "config", "", "")
	fs.StringVar(&lokiURL, "loki", "", "")
	fs.StringVar(&expr, "expr", "", "")
	fs.StringVar(&alertFile, "alert", "", "")
	fs.Var(&labels, "label", "")
	fs.StringVar(&tenant, "tenant", "", "")
	fs.StringVar(&listen, "listen", "", "")
	fs.Var(&alertmanagers, "alertmanager", "")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(env.Stdout, usage)
		return nil
	} else if err != nil {
		return usagef("%v", err)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected %q", fs.Arg(0))
	}
	if cmd == "run" {
		if expr != "" || alertFile != "" || len(labels) > 0 || tenant != "" {
			return usagef("--expr, --alert, --label and --tenant are for check")
		}
		c, err := loadConfig(env, file, overrides{loki: lokiURL, listen: listen, alertmanagers: alertmanagers}, true)
		if err != nil {
			return err
		}
		return runProxy(ctx, env, c)
	}
	if listen != "" || len(alertmanagers) > 0 {
		return usagef("--listen and --alertmanager are for run")
	}
	c, err := loadConfig(env, file, overrides{loki: lokiURL}, false)
	if err != nil {
		return err
	}
	var alerts []enrich.Alert
	switch {
	case expr != "" && alertFile != "":
		return usagef("give --expr or --alert, not both")
	case expr != "":
		a := enrich.Alert{Labels: map[string]string{"alertname": "check"}, GeneratorURL: ExploreURL(expr)}
		for _, l := range labels {
			k, v, ok := strings.Cut(l, "=")
			if !ok || k == "" {
				return usagef("--label %q: expected name=value", l)
			}
			a.Labels[k] = v
		}
		alerts = append(alerts, a)
	case alertFile != "":
		data, err := env.ReadFile(alertFile)
		if err != nil {
			return err
		}
		if alerts, err = ParseAlerts(data); err != nil {
			return fmt.Errorf("%s: %w", alertFile, err)
		}
	default:
		return usagef("give --expr EXPRESSION or --alert FILE")
	}
	e, err := NewEnricher(c)
	if err != nil {
		return err
	}
	if tenant == "" {
		tenant = c.Loki.Tenant
	}
	found, planned := false, false
	for _, a := range alerts {
		fmt.Fprintf(env.Stdout, "alert %s\n", describeLabels(a.Labels))
		a.EndsAt = time.Time{} // checked as firing
		p, err := e.PlanFor(a)
		if err != nil {
			fmt.Fprintf(env.Stdout, "  skipped: %v\n", err)
			continue
		}
		planned = true
		fmt.Fprintf(env.Stdout, "  query: %s\n  from:  %s to %s\n", p.Query, p.Start.UTC().Format(time.RFC3339), p.End.UTC().Format(time.RFC3339))
		if p.Loose != "" {
			fmt.Fprintf(env.Stdout, "  (if that finds nothing: %s)\n", p.Loose)
		}
		if len(p.Match) > 0 {
			fmt.Fprintf(env.Stdout, "  keeping lines whose streams agree with %s\n", describeLabels(p.Match))
		}
		qctx, cancel := context.WithTimeout(ctx, time.Duration(c.Timeout))
		adds, err := e.Enrich(qctx, a, tenant)
		cancel()
		if err != nil {
			return fmt.Errorf("asking Loki: %w", err)
		}
		name := c.Annotation
		fmt.Fprintf(env.Stdout, "  %s_count: %s\n", name, adds[name+"_count"])
		if adds[name] != "" {
			found = true
			fmt.Fprintf(env.Stdout, "  %s:\n", name)
			for _, l := range strings.Split(adds[name], "\n") {
				fmt.Fprintf(env.Stdout, "    %s\n", l)
			}
		}
	}
	switch {
	case !planned:
		return errNoQuery
	case !found:
		return errNoLines
	}
	return nil
}

func describeLabels(l map[string]string) string {
	var parts []string
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ", ") + "}"
}

// overrides are flags that win over the file.
type overrides struct {
	loki, listen  string
	alertmanagers []string
}

// loadConfig reads the file (if any) and applies the flags.
func loadConfig(env Env, file string, o overrides, forProxy bool) (*config.Config, error) {
	named := file != ""
	if file == "" {
		file, _ = env.LookupEnv("ALERTLINES_CONFIG")
		named = file != ""
	}
	if file == "" {
		file = "alertlines.yaml"
	}
	data, err := env.ReadFile(file)
	flagsOnly := o.loki != "" && (!forProxy || len(o.alertmanagers) > 0)
	switch {
	case errors.Is(err, os.ErrNotExist) && !named && flagsOnly:
		data = nil
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("there is no %s here: name the file with -c FILE or ALERTLINES_CONFIG, or give --loki (and --alertmanager for run)", file)
	case errors.Is(err, os.ErrPermission):
		return nil, fmt.Errorf("%s: this user (uid %d) cannot read it. Make it readable to this user or its group (it may hold secrets, so not to everyone)", file, os.Getuid())
	case err != nil:
		return nil, err
	}
	c, err := config.Parse(data, env.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if o.loki != "" {
		c.Loki.URL = o.loki
	}
	if o.listen != "" {
		c.Listen = o.listen
	}
	if len(o.alertmanagers) > 0 {
		c.Alertmanagers = o.alertmanagers
	}
	if problems := c.Check(forProxy); len(problems) > 0 {
		return nil, fmt.Errorf("%s has problems:\n  - %s", file, strings.Join(problems, "\n  - "))
	}
	return c, nil
}

// NewEnricher makes the enricher for a configuration.
func NewEnricher(c *config.Config) (*enrich.Enricher, error) {
	r, err := redact.New(*c.Redact.Builtin, c.Redact.Patterns)
	if err != nil {
		return nil, err
	}
	l := loki.New(c.Loki.URL, c.Loki.Insecure)
	l.Username, l.Password, l.BearerToken = c.Loki.Username, c.Loki.Password, c.Loki.BearerToken
	return &enrich.Enricher{Loki: l, Redact: r, Opt: enrich.Options{
		MaxLines: c.Lines.Max, MaxLineBytes: c.Lines.MaxLineBytes, MaxBytes: c.Lines.MaxBytes, ShowLabels: c.Lines.Labels,
		Annotation: c.Annotation, DefaultRange: time.Duration(c.Range), MaxRange: time.Duration(c.MaxRange), CacheFor: time.Duration(c.Cache)}}, nil
}

// ExploreURL makes a generatorURL like Loki's ruler does, for an expression.
func ExploreURL(expr string) string {
	pane, _ := json.Marshal(map[string]any{"queries": []map[string]string{{"expr": expr, "queryType": "range"}}})
	return "/explore?left=" + url.QueryEscape(string(pane))
}

// ParseAlerts reads alerts as Alertmanager's API lists them (one, or a list).
func ParseAlerts(data []byte) ([]enrich.Alert, error) {
	type alert struct {
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		GeneratorURL string            `json:"generatorURL"`
	}
	var list []alert
	if err := json.Unmarshal(data, &list); err != nil {
		var one alert
		if err2 := json.Unmarshal(data, &one); err2 != nil {
			return nil, fmt.Errorf("expected an alert or a list of alerts (JSON): %v", err)
		}
		list = []alert{one}
	}
	var out []enrich.Alert
	for _, a := range list {
		out = append(out, enrich.Alert{Labels: a.Labels, Annotations: a.Annotations, GeneratorURL: a.GeneratorURL})
	}
	return out, nil
}
