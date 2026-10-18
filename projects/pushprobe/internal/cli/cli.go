// Package cli is pushprobe's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/check"
	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/runner"
)

// Version is set at build time.
var Version = "dev"

// Env is what the command line works with, replaceable in tests.
type Env struct {
	Stdout, Stderr io.Writer
	LookupEnv      func(string) (string, bool)
	ReadFile       func(string) ([]byte, error)
	// Reload asks a running agent to read its file again (SIGHUP).
	Reload <-chan struct{}
}

// DefaultEnv uses the process's own.
func DefaultEnv() Env {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	reload := make(chan struct{}, 1)
	go func() {
		for range hup {
			select {
			case reload <- struct{}{}:
			default:
			}
		}
	}()
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.LookupEnv, ReadFile: os.ReadFile, Reload: reload}
}

const usage = `Usage: pushprobe COMMAND [OPTIONS]

Runs checks where your monitoring cannot reach (a LAN, another site, behind
NAT) and pushes the results to Uptime Kuma push monitors and Gatus external
endpoints.

Commands:
  run               Run the checks on their intervals and send the results
                    (SIGHUP reads the file again)
  check [NAME...]   Run the checks once and print the results; nothing is sent
  validate          Check the file, including where results go
  version           Print the version

Options:
  -c, --config FILE   The file (default $PUSHPROBE_CONFIG, else pushprobe.yaml)

The file may use ${NAME} for environment variables whose names start with
PUSHPROBE_, KUMA_ or GATUS_; no other variable can be read.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 a check failed, 130 interrupted.
`

var (
	errUsage  = errors.New("usage")
	errFailed = errors.New("a check failed")
)

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

// Run runs pushprobe and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errFailed):
		return 3
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "pushprobe: %s\nRun 'pushprobe help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case ctx.Err() != nil:
		fmt.Fprintln(env.Stderr, "pushprobe: interrupted")
		return 130
	default:
		fmt.Fprintf(env.Stderr, "pushprobe: %v\n", err)
		return 1
	}
}

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
		fmt.Fprintf(env.Stdout, "pushprobe %s\n", Version)
		return nil
	case "check", "validate", "run":
	default:
		return usagef("unknown command %q", cmd)
	}
	fs := flag.NewFlagSet("pushprobe "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := ""
	fs.StringVar(&file, "c", "", "")
	fs.StringVar(&file, "config", "", "")
	var names []string
	for {
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(env.Stdout, usage)
			return nil
		} else if err != nil {
			return usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if file == "" {
		file, _ = env.LookupEnv("PUSHPROBE_CONFIG")
	}
	if file == "" {
		file = "pushprobe.yaml"
	}
	if cmd == "run" {
		if len(names) > 0 {
			return usagef("run takes no names")
		}
		if _, err := env.ReadFile(file); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("there is no %s here: name the file with -c FILE or PUSHPROBE_CONFIG", file)
		}
		return runAgent(ctx, env, file)
	}
	data, err := env.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("there is no %s here: name the file with -c FILE or PUSHPROBE_CONFIG", file)
	} else if err != nil {
		return err
	}
	switch cmd {
	case "validate":
		if len(names) > 0 {
			return usagef("validate takes no names")
		}
		c, err := config.Load(data, env.LookupEnv, config.ForRun)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		fmt.Fprintf(env.Stdout, "%s is fine: %s.\n", file, describe(c))
		return nil
	}
	c, err := config.Load(data, env.LookupEnv, config.ForCheck)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return checkOnce(ctx, env, c, names)
}

func describe(c *config.Config) string {
	kuma, gatus := 0, 0
	for _, ch := range c.Checks {
		if ch.Kuma != nil {
			kuma++
		}
		if ch.Gatus != nil {
			gatus++
		}
	}
	return fmt.Sprintf("%s, %d reporting to Uptime Kuma, %d to Gatus", plural(len(c.Checks), "check"), kuma, gatus)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// parallel is how many checks run at once.
const parallel = 8

// checkOnce runs the checks (all, or those named) once, at the same time,
// and prints the results in the file's order.
func checkOnce(ctx context.Context, env Env, c *config.Config, names []string) error {
	var picked []config.Check
	if len(names) == 0 {
		picked = c.Checks
	}
	for _, n := range names {
		found := false
		for _, ch := range c.Checks {
			if ch.Name == n {
				picked = append(picked, ch)
				found = true
			}
		}
		if !found {
			return usagef("there is no check named %q", n)
		}
	}
	type outcome struct {
		r        check.Result
		attempts int
	}
	results := make([]outcome, len(picked))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, ch := range picked {
		checker, err := check.New(ch)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, n := runner.Once(ctx, checker, time.Duration(ch.Timeout), *ch.Retries)
			results[i] = outcome{r, n}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	failed := 0
	for i, ch := range picked {
		o := results[i]
		status := "ok"
		if !o.r.OK {
			status = "FAIL"
			failed++
		}
		extra := ""
		if o.attempts > 1 {
			extra = fmt.Sprintf(" (%d attempts)", o.attempts)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s%s\n", status, ch.Name, o.r.Message, round(o.r.Duration), extra)
	}
	w.Flush()
	fmt.Fprintf(env.Stdout, "%s: %d ok, %d failed. Nothing was sent.\n", plural(len(picked), "check"), len(picked)-failed, failed)
	if failed > 0 {
		return errFailed
	}
	return nil
}

func round(d time.Duration) time.Duration {
	if d > time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}
