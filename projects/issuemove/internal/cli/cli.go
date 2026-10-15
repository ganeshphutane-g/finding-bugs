// Package cli implements the issuemove command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/ganeshphutane-g/issuemove/internal/gitea"
	"github.com/ganeshphutane-g/issuemove/internal/move"
	"github.com/ganeshphutane-g/issuemove/internal/plan"
	"github.com/ganeshphutane-g/issuemove/internal/ref"
)

// Version is set at build time.
var Version = "dev"

const usage = `Usage: issuemove COMMAND [OPTIONS] SOURCE TARGET

Move issues between Gitea or Forgejo repositories, with their comments,
labels, milestones, attachments and reactions. The originals are closed with a
link to their new place.

Commands:
  plan      Show what would happen (changes nothing)
  move      Do it (shows the plan and asks first)
  version   Show the version

SOURCE is owner/repo#12, owner/repo#12,15, owner/repo#12-20, or owner/repo
(with --state and --label to choose). TARGET is owner/repo.

Options:
      --url URL           The instance (default $ISSUEMOVE_URL)
      --source-url URL    The source instance, when it differs
      --target-url URL    The target instance, when it differs
      --state STATE       With owner/repo: open (default), closed or all
      --label NAMES       With owner/repo: only issues with these labels (comma-separated)
      --json              Print the plan as JSON (plan only)
      --keep-open         Leave the originals open (they still get the link)
      --lock              Lock the originals' conversation
      --match-users       On another instance, post as the accounts with the
                          same name and email (default: name authors in the text)
  -y, --yes               Do not ask before moving

Tokens come from the environment, never from the command line:
  ISSUEMOVE_SOURCE_TOKEN and ISSUEMOVE_TARGET_TOKEN (or files named by
  ISSUEMOVE_SOURCE_TOKEN_FILE and ISSUEMOVE_TARGET_TOKEN_FILE); on one
  instance, ISSUEMOVE_TOKEN (or ISSUEMOVE_TOKEN_FILE) serves for both.
An admin token on the target posts everything as its original authors.

Exit codes: 0 done, 1 error or answered no, 2 bad usage, 3 the plan is
blocked, 130 interrupted.
`

var (
	errUsage   = errors.New("usage")
	errBlocked = errors.New("blocked")
)

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

// Env is the outside world the command sees.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	ReadFile       func(string) ([]byte, error)
}

// DefaultEnv uses the process environment.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, ReadFile: os.ReadFile}
}

// Run runs issuemove and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "issuemove: %s\nRun 'issuemove --help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case errors.Is(err, errBlocked):
		fmt.Fprintln(env.Stderr, "issuemove: blocked; see the problems listed above")
		return 3
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "issuemove: interrupted")
		return 130
	default:
		fmt.Fprintf(env.Stderr, "issuemove: %v\n", err)
		return 1
	}
}

type options struct {
	url, srcURL, dstURL string
	state, labels       string
	json                bool
	keepOpen, lock, yes bool
	matchUsers          bool
	help                bool
	pos                 []string
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("issuemove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.url, "url", "", "")
	fs.StringVar(&o.srcURL, "source-url", "", "")
	fs.StringVar(&o.dstURL, "target-url", "", "")
	fs.StringVar(&o.state, "state", "", "")
	fs.StringVar(&o.labels, "label", "", "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.keepOpen, "keep-open", false, "")
	fs.BoolVar(&o.lock, "lock", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
	fs.BoolVar(&o.matchUsers, "match-users", false, "")
	rest := args
	for {
		if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
			o.help = true
			return o, nil
		} else if err != nil {
			return o, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		o.pos = append(o.pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch o.state {
	case "", "open", "closed", "all":
	default:
		return o, usagef("--state must be open, closed or all")
	}
	return o, nil
}

// token reads a token for a side ("SOURCE" or "TARGET"). The side's own
// settings come first; the shared ones only when both sides are one
// instance, so a token is never sent to a server it was not made for.
func token(env Env, side string, shared bool) (string, error) {
	names := []string{"ISSUEMOVE_" + side + "_TOKEN"}
	if shared {
		names = append(names, "ISSUEMOVE_TOKEN")
	}
	for _, name := range names {
		if v := strings.TrimSpace(env.Getenv(name)); v != "" {
			return v, nil
		}
		if f := env.Getenv(name + "_FILE"); f != "" {
			data, err := env.ReadFile(f)
			if err != nil {
				return "", fmt.Errorf("%s_FILE: %w", name, err)
			}
			if v := strings.TrimSpace(string(data)); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%s_FILE: the file is empty", name)
		}
	}
	if !shared {
		return "", fmt.Errorf("no %s token: the instances differ, so set ISSUEMOVE_%s_TOKEN (ISSUEMOVE_TOKEN is used only within one instance)", strings.ToLower(side), side)
	}
	return "", fmt.Errorf("no %s token: set ISSUEMOVE_TOKEN (or ISSUEMOVE_%s_TOKEN)", strings.ToLower(side), side)
}

func run(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		return usagef("name a command")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintf(env.Stdout, "issuemove %s\n", Version)
		return nil
	case "plan", "move":
	default:
		if strings.HasPrefix(cmd, "-") {
			return usagef("the command comes first, as in: issuemove plan %s ...", cmd)
		}
		return usagef("unknown command %q", cmd)
	}
	o, err := parse(args)
	if err != nil {
		return err
	}
	if o.help {
		fmt.Fprint(env.Stdout, usage)
		return nil
	}
	if len(o.pos) != 2 {
		return usagef("need SOURCE and TARGET")
	}
	src, err := ref.ParseSource(o.pos[0])
	if err != nil {
		return usagef("%v", err)
	}
	dst, err := ref.ParseRepo(o.pos[1])
	if err != nil {
		return usagef("%v", err)
	}
	if len(src.Numbers) > 0 && (o.state != "" || o.labels != "") {
		return usagef("--state and --label choose issues only when SOURCE has no numbers")
	}
	base := o.url
	if base == "" {
		base = env.Getenv("ISSUEMOVE_URL")
	}
	srcURL, dstURL := firstOf(o.srcURL, base), firstOf(o.dstURL, base)
	if srcURL == "" || dstURL == "" {
		return usagef("give the instance with --url (or ISSUEMOVE_URL)")
	}
	for _, u := range []string{srcURL, dstURL} {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
			return usagef("%q is not an http(s) URL", u)
		}
		if pu.Scheme == "http" && !loopback(pu.Hostname()) {
			fmt.Fprintf(env.Stderr, "issuemove: warning: %s is not https, so the token travels unencrypted\n", u)
		}
	}
	shared := sameServer(srcURL, dstURL)
	srcTok, err := token(env, "SOURCE", shared)
	if err != nil {
		return err
	}
	dstTok, err := token(env, "TARGET", shared)
	if err != nil {
		return err
	}
	if o.json && cmd != "plan" {
		return usagef("--json is for plan")
	}
	srcC, dstC := gitea.New(srcURL, srcTok), gitea.New(dstURL, dstTok)
	p, err := plan.Build(ctx, plan.Input{Src: srcC, Dst: dstC,
		Source: src, Target: dst, State: o.state, Labels: o.labels,
		KeepOpen: o.keepOpen, Lock: o.lock, MatchUsers: o.matchUsers,
		Progress: func(done, total int) {
			if total >= 20 && (done%20 == 0 || done == total) {
				fmt.Fprintf(env.Stderr, "reading issues: %d/%d\n", done, total)
			}
		}})
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(p); err != nil {
			return err
		}
	} else {
		p.Render(env.Stdout)
	}
	if !p.OK() {
		return errBlocked
	}
	if cmd == "plan" {
		return nil
	}
	if !o.yes {
		yes, err := confirm(ctx, env.Stdin, env.Stdout, fmt.Sprintf("Move %d issue(s) to %s?", len(p.Issues), p.Target))
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("stopped; nothing was changed")
		}
	}
	results, err := move.Run(ctx, move.Options{Src: srcC, Dst: dstC, Plan: p,
		Log: func(s string) { fmt.Fprintln(env.Stdout, s) }})
	if err != nil {
		fmt.Fprintln(env.Stdout, "\nThe move stopped. Once the problem below is solved, run the same command again: what was copied is found and finished, nothing is duplicated.")
		return err
	}
	fmt.Fprintf(env.Stdout, "\nMoved %d issue(s) to %s; the originals have a link to their new place", len(results), p.Target)
	if o.keepOpen {
		fmt.Fprintln(env.Stdout, " and stay open.")
	} else {
		fmt.Fprintln(env.Stdout, " and are closed.")
	}
	return nil
}

// confirm asks a yes/no question; Ctrl-C while waiting stops the program.
func confirm(ctx context.Context, in io.Reader, out io.Writer, q string) (bool, error) {
	fmt.Fprintf(out, "\n%s [y/N] ", q)
	answer := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(in).ReadString('\n')
		answer <- line
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(out)
		return false, ctx.Err()
	case line := <-answer:
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}

// sameServer reports whether two URLs name the same instance.
func sameServer(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/")
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
