// Package cli implements the pushdir command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushdir/internal/push"
	"github.com/ganeshphutane-g/pushdir/internal/rclone"
)

// Version is set at build time.
var Version = "dev"

// Env is what the command needs from the outside world.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
}

// DefaultEnv uses the process environment.
func DefaultEnv() Env {
	return Env{Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}

const usage = `Usage: pushdir [OPTIONS] LOCAL_DIR REMOTE:PATH [-- RCLONE_FLAGS...]

Watch LOCAL_DIR and copy every change to REMOTE:PATH with rclone, within
seconds. Starts with a full copy, then sends changed files in batches once they
have stopped changing. Remote files are never deleted unless --delete is given.

Options:
      --delete            Mirror local deletions to the remote
      --backup-dir PATH   Keep deleted and overwritten remote files here instead
                          (an rclone path on the same remote, e.g. remote:old)
      --max-delete N      Pause deletions if one batch or full check would delete
                          more than N files (default 100)
      --quiet DURATION    Wait this long after the last change (default 2s)
      --max-delay DUR     Never hold changes longer than this (default 30s)
      --settle DURATION   Send a file only after it is unchanged this long (default 2s)
      --resync DURATION   Full check this often, 0 to turn off (default 6h)
      --listen ADDR       Serve /status and /metrics on ADDR (e.g. 127.0.0.1:9090)
      --rclone PATH       The rclone program (default: rclone on PATH)
      --version           Show the version

Everything after -- goes to every rclone call, e.g. filters and limits:
  pushdir ~/Photos b2:photos -- --exclude '*.tmp' --bwlimit 5M
`

var errUsage = errors.New("usage")

// Run runs pushdir and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "pushdir: %v\nRun 'pushdir --help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	default:
		fmt.Fprintf(env.Stderr, "pushdir: %v\n", err)
		return 1
	}
}

func usagef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, args...))
}

// config is the parsed command line.
type config struct {
	opts   push.Options
	listen string
}

func parse(args []string) (config, bool, error) {
	var extra []string
	for i, a := range args {
		if a == "--" {
			args, extra = args[:i], args[i+1:]
			break
		}
	}
	fs := flag.NewFlagSet("pushdir", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c config
	var help, version bool
	r := &rclone.Runner{Extra: extra}
	fs.BoolVar(&c.opts.Delete, "delete", false, "")
	fs.StringVar(&r.BackupDir, "backup-dir", "", "")
	fs.IntVar(&c.opts.MaxDelete, "max-delete", 100, "")
	fs.DurationVar(&c.opts.Quiet, "quiet", 2*time.Second, "")
	fs.DurationVar(&c.opts.MaxDelay, "max-delay", 30*time.Second, "")
	fs.DurationVar(&c.opts.Settle, "settle", 2*time.Second, "")
	fs.DurationVar(&c.opts.Resync, "resync", 6*time.Hour, "")
	fs.StringVar(&c.listen, "listen", "", "")
	fs.StringVar(&r.Bin, "rclone", "", "")
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")
	fs.BoolVar(&version, "version", false, "")

	// Allow options before and after the two positional arguments.
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return c, false, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if help || version {
		return c, true, nil
	}
	if len(pos) != 2 {
		return c, false, usagef("need LOCAL_DIR and REMOTE:PATH")
	}
	if c.opts.MaxDelete < 0 || c.opts.Quiet <= 0 || c.opts.MaxDelay < c.opts.Quiet || c.opts.Settle < 0 || c.opts.Resync < 0 {
		return c, false, usagef("durations must be positive, --max-delay at least --quiet, --max-delete not negative")
	}
	if r.BackupDir != "" && !c.opts.Delete {
		// Overwritten files are kept too, so it is still useful.
	}
	local, err := filepath.Abs(pos[0])
	if err != nil {
		return c, false, err
	}
	c.opts.Local, c.opts.Remote, c.opts.Runner = local, pos[1], r
	if strings.HasPrefix(c.opts.Remote, "-") {
		return c, false, usagef("REMOTE:PATH looks like an option: %q", c.opts.Remote)
	}
	return c, false, nil
}

func run(ctx context.Context, env Env, args []string) error {
	c, done, err := parse(args)
	if err != nil {
		return err
	}
	if done {
		for _, a := range args {
			if a == "--version" {
				fmt.Fprintf(env.Stdout, "pushdir %s\n", Version)
				return nil
			}
		}
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	logger := func(s string) {
		fmt.Fprintf(env.Stderr, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
	}
	c.opts.Log = logger
	c.opts.Runner.Log = nil
	p := push.New(c.opts)
	if c.listen != "" {
		stop, err := serveStatus(c.listen, p)
		if err != nil {
			return err
		}
		defer stop()
		logger("status on http://" + c.listen + "/status")
	}
	return p.Run(ctx)
}
