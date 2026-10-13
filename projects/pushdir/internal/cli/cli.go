// Package cli implements the pushdir command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
      --max-delete N      Pause deletions if more than N remote files would be
                          deleted within an hour (default 100)
      --quiet DURATION    Wait this long after the last change (default 2s)
      --max-delay DUR     Never hold changes longer than this (default 30s)
      --settle DURATION   Send a file only after it is unchanged this long (default 2s)
      --resync DURATION   Full check this often, 0 to turn off (default 6h)
      --listen ADDR       Serve /status and /metrics on ADDR (e.g. 127.0.0.1:9090)
      --rclone PATH       The rclone program (default: rclone on PATH)
      --version           Show the version

REMOTE:PATH is an rclone remote ("b2:bucket/photos") or an absolute folder.

Everything after -- goes to rclone, e.g. filters and limits:
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
	show   string // "help" or "version": print it and stop
}

func parse(args []string) (config, error) {
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
			return c, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch {
	case version:
		c.show = "version"
		return c, nil
	case help:
		c.show = "help"
		return c, nil
	}
	if len(pos) != 2 {
		return c, usagef("need LOCAL_DIR and REMOTE:PATH")
	}
	switch {
	case c.opts.Quiet <= 0 || c.opts.Settle <= 0 || c.opts.Resync < 0:
		return c, usagef("--quiet and --settle must be more than 0 (e.g. 100ms), --resync 0 or more")
	case c.opts.MaxDelay < c.opts.Quiet:
		return c, usagef("--max-delay must be at least --quiet")
	case c.opts.MaxDelete < 1:
		return c, usagef("--max-delete must be at least 1 (to delete nothing on the remote, leave out --delete)")
	}
	local, err := filepath.Abs(pos[0])
	if err != nil {
		return c, err
	}
	c.opts.Local, c.opts.Remote, c.opts.Runner = local, pos[1], r
	for _, dest := range []struct{ flag, path string }{{"REMOTE:PATH", c.opts.Remote}, {"--backup-dir", r.BackupDir}} {
		if dest.path == "" && dest.flag == "--backup-dir" {
			continue
		}
		if err := checkDest(local, dest.flag, dest.path); err != nil {
			return c, err
		}
	}
	return c, nil
}

// checkDest refuses destinations rclone would take as a relative folder, and
// local folders that overlap the watched one (pushdir would copy its own
// output forever).
func checkDest(local, flag, dest string) error {
	if dest == "" {
		return usagef("%s is empty", flag)
	}
	if strings.HasPrefix(dest, "-") {
		return usagef("%s looks like an option: %q", flag, dest)
	}
	path, isLocal := localPath(dest)
	if !isLocal {
		return nil
	}
	if !filepath.IsAbs(path) {
		return usagef("%s %q is neither a remote (name:path) nor an absolute folder", flag, dest)
	}
	resolve := func(p string) string {
		p = filepath.Clean(p)
		// Resolve symlinks in the part that exists.
		for q, rest := p, ""; ; {
			if real, err := filepath.EvalSymlinks(q); err == nil {
				return filepath.Join(real, rest)
			}
			parent := filepath.Dir(q)
			if parent == q {
				return p
			}
			rest = filepath.Join(filepath.Base(q), rest)
			q = parent
		}
	}
	a, b := resolve(local), resolve(path)
	inside := func(x, y string) bool {
		return x == y || strings.HasPrefix(x, y+string(filepath.Separator)) || y == "/"
	}
	if inside(a, b) || inside(b, a) {
		return usagef("%s %s overlaps the watched folder %s", flag, dest, local)
	}
	return nil
}

// localPath tells whether rclone takes dest as a local path, and which.
func localPath(dest string) (string, bool) {
	if strings.HasPrefix(dest, ":local:") {
		return strings.TrimPrefix(dest, ":local:"), true
	}
	if strings.HasPrefix(dest, ":local,") {
		if _, after, ok := strings.Cut(dest[1:], ":"); ok {
			return after, true
		}
	}
	i := strings.IndexByte(dest, ':')
	if i < 0 || strings.ContainsRune(dest[:i], '/') {
		return dest, true
	}
	return "", false
}

func run(ctx context.Context, env Env, args []string) error {
	c, err := parse(args)
	if err != nil {
		return err
	}
	switch c.show {
	case "version":
		fmt.Fprintf(env.Stdout, "pushdir %s\n", Version)
		return nil
	case "help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	logger := func(s string) {
		fmt.Fprintf(env.Stderr, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
	}
	if env.Getenv("JOURNAL_STREAM") != "" {
		// journald adds its own timestamps.
		logger = func(s string) { fmt.Fprintln(env.Stderr, s) }
	}
	c.opts.Log = logger
	c.opts.Runner.Log = nil
	unlock, err := lock(c.opts.Local)
	if err != nil {
		return err
	}
	defer unlock()
	p := push.New(c.opts)
	if c.listen != "" {
		stop, addr, err := serveStatus(c.listen, p)
		if err != nil {
			return err
		}
		defer stop()
		logger("status on http://" + addr + "/status")
		if host, _, _ := net.SplitHostPort(addr); !net.ParseIP(host).IsLoopback() {
			logger("warning: --listen " + c.listen + " is reachable from other machines, without a password; use 127.0.0.1:PORT to keep it local")
		}
	}
	return p.Run(ctx)
}
