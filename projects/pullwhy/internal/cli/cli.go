// Package cli is pullwhy's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/creds"
	"github.com/ganeshphutane-g/pullwhy/internal/diagnose"
	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/plan"
	"github.com/ganeshphutane-g/pullwhy/internal/probe"
	"github.com/ganeshphutane-g/pullwhy/internal/pull"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
	"github.com/ganeshphutane-g/pullwhy/internal/report"
)

// Version is set at build time.
var Version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitProblem = 3
	exitSlow    = 4
)

const usage = `pullwhy shows why a docker pull is slow or fails: it walks the pull the way
the daemon will (mirrors, proxy, certificates, auth, the manifest), times each
step, and names the likely cause.

Usage:
  pullwhy [flags] IMAGE

Flags:
  --pull           also have the daemon really pull, and time it
  --deep           also read the manifest and the start of the largest layer
                   (counts as a pull on Docker Hub)
  --platform P     the platform for --deep and --pull (default: the daemon's)
  --timeout D      the most to wait for each request (default 10s)
  --pull-timeout D the most to wait for --pull (default 10m)
  --json           print JSON
  --version

Exit codes: 0 fine, 1 error, 2 bad usage, 3 a problem was found, 4 it works but
something is slow.
`

// Env is what the command reads from outside: overridden in tests.
type Env struct {
	Getenv    func(string) string
	ConfigDir string // DOCKER_CONFIG, or ~/.docker
	Runner    creds.Runner
	Resolver  plan.Resolver
}

// DefaultEnv is the real one.
func DefaultEnv() Env {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".docker")
	}
	return Env{Getenv: os.Getenv, ConfigDir: dir, Runner: creds.RunHelper, Resolver: plan.DefaultResolver}
}

// Main runs pullwhy and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, stdout, stderr, DefaultEnv())
}

// Run is Main with its surroundings given.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("pullwhy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	doPull := fs.Bool("pull", false, "")
	deep := fs.Bool("deep", false, "")
	platformFlag := fs.String("platform", "", "")
	timeout := fs.Duration("timeout", 10*time.Second, "")
	pullTimeout := fs.Duration("pull-timeout", 10*time.Minute, "")
	asJSON := fs.Bool("json", false, "")
	version := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *version {
		fmt.Fprintln(stdout, "pullwhy", Version)
		return exitOK
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if *timeout <= 0 || *pullTimeout <= 0 {
		fmt.Fprintln(stderr, "pullwhy: --timeout and --pull-timeout must be more than 0")
		return exitUsage
	}
	r, err := ref.Parse(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "pullwhy:", err)
		return exitUsage
	}
	var want probe.Platform
	if *platformFlag != "" {
		if want, err = probe.ParsePlatform(*platformFlag); err != nil {
			fmt.Fprintln(stderr, "pullwhy:", err)
			return exitUsage
		}
	}

	ep, err := engine.Find(env.Getenv, env.ConfigDir)
	if err != nil {
		fmt.Fprintln(stderr, "pullwhy:", err)
		return exitError
	}
	client, err := engine.New(ep)
	if err != nil {
		fmt.Fprintln(stderr, "pullwhy:", err)
		return exitError
	}
	ictx, cancel := context.WithTimeout(ctx, *timeout)
	info, err := client.Info(ictx)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "pullwhy:", err)
		return exitError
	}
	// sameHost: pullwhy can read the daemon's files (certs.d); sameNet: its
	// probes see the daemon's network (not a rootless daemon's namespace).
	sameHost := ep.Local() && runtime.GOOS == "linux" && !info.DockerDesktop()
	sameNet := sameHost && !info.Rootless()
	p, err := plan.Make(ctx, r, info, env.Resolver, sameHost)
	if err != nil {
		fmt.Fprintln(stderr, "pullwhy:", err)
		return exitError
	}
	if *platformFlag == "" {
		want = probe.DaemonPlatform(info.OSType, info.Architecture)
	}

	auth, credErr := creds.Lookup(ctx, env.ConfigDir, r.Registry, env.Runner)
	credText := "none (anonymous)"
	if !auth.Anonymous() {
		credText = "from " + auth.Source
	} else if credErr != nil {
		credText = "could not be read: " + credErr.Error()
	}

	opts := probe.Options{Timeout: *timeout, Auth: auth, Containerd: p.Containerd, Proxy: plan.ProxyFunc(info), UserAgent: "pullwhy/" + Version}
	var results []*probe.Result
	for _, e := range p.Endpoints {
		o := opts
		if e.Mirror && p.Containerd {
			// The containerd store sends mirrors no credentials (tested).
			o.Auth = nil
		}
		results = append(results, probe.Endpoint(ctx, e, r, o))
	}

	var dp *probe.Deep
	if *deep {
		for _, res := range results {
			if res.OK {
				dp = probe.DeepCheck(ctx, res, r, opts, want, 8<<20, 5*time.Second)
				break
			}
		}
	}

	var t *pull.Timeline
	if *doPull {
		pctx, cancel := context.WithTimeout(ctx, *pullTimeout)
		name, reference := r.FromImage()
		platform := ""
		if *platformFlag != "" {
			platform = want.String()
		}
		if !*asJSON {
			fmt.Fprintf(stderr, "pullwhy: pulling %s with the daemon (up to %s)...\n", r, *pullTimeout)
		}
		t = pull.Run(pctx, client, name, reference, platform, auth.Header(creds.ServerAddress(r.Registry)), 10*time.Second, nil)
		cancel()
	}

	findings := diagnose.Diagnose(diagnose.Input{Info: info, Local: sameNet, Plan: p, Results: results, Auth: auth, CredErr: credErr,
		Deep: dp, Pull: t, Timeout: *timeout, DaemonHost: ep.Host})

	store := "classic"
	if p.Containerd {
		store = "containerd"
	}
	proxy := info.HTTPSProxy
	if proxy == "" {
		proxy = info.HTTPProxy
	}
	rep := report.New(r.String(), report.Daemon{Host: ep.Host, From: ep.From, Version: info.ServerVersion, Store: store,
		Proxy: proxy, NoProxy: info.NoProxy, Local: sameNet}, credText, p, results, dp, t, findings)
	if *asJSON {
		if err := rep.JSON(stdout); err != nil {
			fmt.Fprintln(stderr, "pullwhy:", err)
			return exitError
		}
	} else {
		rep.Text(stdout)
	}
	switch diagnose.Worst(findings) {
	case diagnose.Problem:
		return exitProblem
	case diagnose.Slow:
		return exitSlow
	}
	return exitOK
}
