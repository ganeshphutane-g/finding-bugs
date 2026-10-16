// Package cli implements the kumafile command line.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ganeshphutane-g/kumafile/internal/apply"
	"github.com/ganeshphutane-g/kumafile/internal/config"
	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

// Version is set at build time.
var Version = "dev"

const usage = `Usage: kumafile COMMAND [OPTIONS]

Keep an Uptime Kuma 2.x configuration in a file: monitors, groups,
notifications, tags, maintenance windows, status pages, proxies, Docker hosts
and remote browsers.

Commands:
  export    Write the instance's configuration to a file
  diff      Show how the instance differs from a file (changes nothing)
  apply     Make the instance match a file (shows the changes and asks first)
  version   Show the version

  kumafile export [-o FILE]
  kumafile diff [FILE]      (FILE defaults to kumafile.yaml; - reads stdin)
  kumafile apply [FILE]

Options:
      --url URL            The instance (default $KUMAFILE_URL)
  -o, --output FILE        export: where to write (default kumafile.yaml; - for
                           standard output)
      --secrets FILE       export: where to write secrets (default: the output
                           file with .env instead of .yaml)
      --with-secrets       export: keep secrets in the file itself
      --env-file FILE      diff, apply: where ${NAME} values are (default: the file
                           with .env instead of .yaml, if it exists); the
                           environment takes precedence
      --prune              diff, apply: also delete what is only on the instance
  -y, --yes                apply: do not ask

Login comes from the environment, never from the command line:
  KUMAFILE_USERNAME and KUMAFILE_PASSWORD (or KUMAFILE_PASSWORD_FILE), and
  KUMAFILE_TOTP (a one-time code) or KUMAFILE_TOTP_SECRET(_FILE) with
  two-factor login; or KUMAFILE_TOKEN(_FILE), a token from an earlier login.
  An instance with authentication turned off needs none.

Exit codes: 0 done (diff: in sync), 1 error, 2 bad usage, 3 diff: differences.
`

var errUsage = errors.New("usage")

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

// Env is the outside world the command sees.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	ReadFile       func(string) ([]byte, error)
	WriteFile      func(name string, data []byte, perm os.FileMode) error
}

// DefaultEnv uses the process environment.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv,
		ReadFile: os.ReadFile, WriteFile: writeFile}
}

// writeFile replaces a file in one step (a temporary file renamed over it),
// so an interrupted export never leaves half a file.
func writeFile(name string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// Run runs kumafile and returns the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "kumafile: %s\nRun 'kumafile --help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case errors.Is(err, errDifferent):
		return 3
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "kumafile: interrupted")
		return 130
	default:
		fmt.Fprintf(env.Stderr, "kumafile: %v\n", err)
		return 1
	}
}

type options struct {
	url         string
	output      string
	secrets     string
	withSecrets bool
	envFile     string
	prune       bool
	yes         bool
	help        bool
	pos         []string
}

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("kumafile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.url, "url", "", "")
	fs.StringVar(&o.output, "o", "", "")
	fs.StringVar(&o.output, "output", "", "")
	fs.StringVar(&o.secrets, "secrets", "", "")
	fs.BoolVar(&o.withSecrets, "with-secrets", false, "")
	fs.StringVar(&o.envFile, "env-file", "", "")
	fs.BoolVar(&o.prune, "prune", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
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
	return o, nil
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
		fmt.Fprintf(env.Stdout, "kumafile %s\n", Version)
		return nil
	case "export", "diff", "apply":
	default:
		if strings.HasPrefix(cmd, "-") {
			return usagef("the command comes first, as in: kumafile export %s ...", cmd)
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
	if cmd == "diff" || cmd == "apply" {
		return compareOrApply(ctx, env, o, cmd == "apply")
	}
	if len(o.pos) > 0 {
		return usagef("export takes no file name; use -o FILE")
	}
	return export(ctx, env, o)
}

var errDifferent = errors.New("the instance differs from the file")

func compareOrApply(ctx context.Context, env Env, o options, doApply bool) error {
	if len(o.pos) > 1 {
		return usagef("give one file")
	}
	path := "kumafile.yaml"
	if len(o.pos) == 1 {
		path = o.pos[0]
	}
	if doApply && path == "-" && !o.yes {
		return usagef("reading the file from standard input needs --yes (there is nobody to ask)")
	}
	want, err := loadFile(env, path, o.envFile)
	if err != nil {
		return err
	}
	c, err := connect(ctx, env, o)
	if err != nil {
		return err
	}
	defer c.Close()
	u := c.url
	st, err := c.Read(ctx)
	if err != nil {
		return err
	}
	have, err := liveDoc(st)
	if err != nil {
		return err
	}
	p := config.Diff(want, have, o.prune)
	fmt.Fprintf(env.Stdout, "Comparing %s with %s (Uptime Kuma %s):\n", path, redact(u), st.Info.Version)
	p.Render(env.Stdout)
	if len(p.Problems) > 0 {
		return fmt.Errorf("the file refers to things that do not exist (listed above with !)")
	}
	if !doApply {
		if !p.InSync() {
			return errDifferent
		}
		return nil
	}
	if p.InSync() {
		return nil
	}
	if !o.yes {
		yes, err := confirm(ctx, env.Stdin, env.Stdout, fmt.Sprintf("Apply these changes to %s?", redact(u)))
		if err != nil {
			return err
		}
		if !yes {
			return errors.New("stopped; nothing was changed")
		}
	}
	err = apply.Run(ctx, apply.Options{Client: c.Client, State: st, Want: want, Plan: p,
		Log: func(s string) { fmt.Fprintln(env.Stdout, s) }})
	if err != nil {
		fmt.Fprintln(env.Stdout, "Apply stopped partway; what is above was done. Run it again to finish: it only changes what still differs.")
		return err
	}
	// Check the result on a fresh connection (some lists are only sent on
	// login).
	cr, _ := credentials(env)
	if c.token != "" {
		cr = kuma.Credentials{Token: c.token}
	}
	c2, err := login(ctx, u, cr)
	if err != nil {
		return fmt.Errorf("applied, but could not check the result: %w", err)
	}
	defer c2.Close()
	st2, err := c2.Read(ctx)
	if err != nil {
		return fmt.Errorf("applied, but could not check the result: %w", err)
	}
	have2, err := liveDoc(st2)
	if err != nil {
		return err
	}
	if p2 := config.Diff(want, have2, o.prune); !p2.InSync() {
		fmt.Fprintln(env.Stdout, "\nThe server did not take all of it; still different:")
		p2.Render(env.Stdout)
		return errors.New("the instance still differs from the file (this server may not support some fields)")
	}
	fmt.Fprintln(env.Stdout, "Done: the instance matches the file.")
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

// loadFile reads a kumafile, filling ${NAME} from the environment and then
// the env file.
func loadFile(env Env, path, envFile string) (*config.Doc, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(env.Stdin)
	} else {
		data, err = env.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	explicit := envFile != ""
	if envFile == "" && path != "-" {
		envFile = strings.TrimSuffix(strings.TrimSuffix(path, ".yaml"), ".yml") + ".env"
	}
	if envFile != "" {
		ed, err := env.ReadFile(envFile)
		switch {
		case err == nil:
			if vars, err = config.ReadEnv(bytes.NewReader(ed)); err != nil {
				return nil, fmt.Errorf("%s: %w", envFile, err)
			}
		case explicit || !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	return config.Load(data, func(name string) (string, bool) {
		if v := env.Getenv(name); v != "" {
			return v, true
		}
		v, ok := vars[name]
		return v, ok
	})
}

// liveDoc is the instance as a loaded file, made by the same export, so
// both sides of a comparison look alike.
func liveDoc(st *kuma.State) (*config.Doc, error) {
	ex, err := config.Make(st, config.ExportOptions{InlineSecrets: true})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(ex.Doc); err != nil {
		return nil, err
	}
	enc.Close()
	return config.Load(buf.Bytes(), func(string) (string, bool) { return "", false })
}

func instanceURL(env Env, o options) (string, error) {
	u := o.url
	if u == "" {
		u = env.Getenv("KUMAFILE_URL")
	}
	if u == "" {
		return "", usagef("give the instance with --url (or KUMAFILE_URL)")
	}
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return "", usagef("%q is not an http(s) URL", u)
	}
	return strings.TrimSuffix(u, "/"), nil
}

// secretEnv reads NAME, or the file named by NAME_FILE.
func secretEnv(env Env, name string) (string, error) {
	if v := env.Getenv(name); v != "" {
		return v, nil
	}
	if f := env.Getenv(name + "_FILE"); f != "" {
		data, err := env.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	return "", nil
}

func credentials(env Env) (kuma.Credentials, error) {
	var cr kuma.Credentials
	var err error
	cr.Username = env.Getenv("KUMAFILE_USERNAME")
	if cr.Password, err = secretEnv(env, "KUMAFILE_PASSWORD"); err != nil {
		return cr, err
	}
	cr.TOTP = env.Getenv("KUMAFILE_TOTP")
	if cr.TOTPSecret, err = secretEnv(env, "KUMAFILE_TOTP_SECRET"); err != nil {
		return cr, err
	}
	if cr.Token, err = secretEnv(env, "KUMAFILE_TOKEN"); err != nil {
		return cr, err
	}
	cr.Token = strings.TrimSpace(cr.Token)
	return cr, nil
}

// session is a logged-in connection, and the token to log in again (a
// one-time code works only once).
type session struct {
	*kuma.Client
	url, token string
}

// connect opens a logged-in connection.
func connect(ctx context.Context, env Env, o options) (*session, error) {
	u, err := instanceURL(env, o)
	if err != nil {
		return nil, err
	}
	cr, err := credentials(env)
	if err != nil {
		return nil, err
	}
	return login(ctx, u, cr)
}

func login(ctx context.Context, u string, cr kuma.Credentials) (*session, error) {
	c, err := kuma.Connect(ctx, u)
	if err != nil {
		return nil, err
	}
	token, err := c.Login(ctx, cr)
	if err != nil {
		c.Close()
		return nil, err
	}
	return &session{Client: c, url: u, token: token}, nil
}

func export(ctx context.Context, env Env, o options) error {
	out := o.output
	if out == "" {
		out = "kumafile.yaml"
	}
	secretsPath := o.secrets
	if out == "-" && secretsPath == "" && !o.withSecrets {
		return usagef("when writing to standard output, name a --secrets file or use --with-secrets")
	}
	if secretsPath == "" && !o.withSecrets {
		secretsPath = strings.TrimSuffix(strings.TrimSuffix(out, ".yaml"), ".yml") + ".env"
	}
	c, err := connect(ctx, env, o)
	if err != nil {
		return err
	}
	defer c.Close()
	u := c.url
	st, err := c.Read(ctx)
	if err != nil {
		return err
	}
	ex, err := config.Make(st, config.ExportOptions{Source: redact(u), InlineSecrets: o.withSecrets})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(ex.Doc); err != nil {
		return err
	}
	enc.Close()
	if len(ex.Secrets) > 0 {
		var sb bytes.Buffer
		if err := config.WriteEnv(&sb, ex.Secrets); err != nil {
			return err
		}
		if err := env.WriteFile(secretsPath, sb.Bytes(), 0o600); err != nil {
			return fmt.Errorf("writing the secrets: %w", err)
		}
	}
	if out == "-" {
		_, err = env.Stdout.Write(buf.Bytes())
	} else {
		err = env.WriteFile(out, buf.Bytes(), 0o644)
	}
	if err != nil {
		return err
	}
	for _, w := range ex.Warnings {
		fmt.Fprintf(env.Stderr, "kumafile: warning: %s\n", w)
	}
	if out != "-" {
		fmt.Fprintf(env.Stderr, "Wrote %s: %s.\n", out, summary(st))
		if len(ex.Secrets) > 0 {
			fmt.Fprintf(env.Stderr, "Wrote %d secret(s) to %s (readable only by you; keep it out of git).\n", len(ex.Secrets), secretsPath)
		}
	}
	return nil
}

// redact drops any password from a URL shown in the file.
func redact(u string) string {
	pu, err := url.Parse(u)
	if err != nil || pu.User == nil {
		return u
	}
	pu.User = nil
	return pu.String()
}

func summary(st *kuma.State) string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(len(st.Monitors), "monitor", "monitors")
	add(len(st.Notifications), "notification", "notifications")
	add(len(st.Tags), "tag", "tags")
	add(len(st.Maintenance), "maintenance window", "maintenance windows")
	add(len(st.StatusPages), "status page", "status pages")
	add(len(st.Proxies), "proxy", "proxies")
	add(len(st.DockerHosts), "Docker host", "Docker hosts")
	add(len(st.RemoteBrowsers), "remote browser", "remote browsers")
	if len(parts) == 0 {
		return "nothing configured"
	}
	return strings.Join(parts, ", ")
}
