// Package cli implements the kumafile command line.
package cli

import (
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
  version   Show the version

  kumafile export [-o FILE]
  kumafile diff [FILE]      (FILE defaults to kumafile.yaml; - reads stdin)

Options:
      --url URL            The instance (default $KUMAFILE_URL)
  -o, --output FILE        export: where to write (default kumafile.yaml; - for
                           standard output)
      --secrets FILE       export: where to write secrets (default: the output
                           file with .env instead of .yaml)
      --with-secrets       export: keep secrets in the file itself
      --env-file FILE      diff: where ${NAME} values are (default: the file
                           with .env instead of .yaml, if it exists); the
                           environment takes precedence
      --prune              diff: also delete what is only on the instance

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
	case "export", "diff":
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
	if cmd == "diff" {
		return diff(ctx, env, o)
	}
	if len(o.pos) > 0 {
		return usagef("export takes no file name; use -o FILE")
	}
	return export(ctx, env, o)
}

var errDifferent = errors.New("the instance differs from the file")

func diff(ctx context.Context, env Env, o options) error {
	if len(o.pos) > 1 {
		return usagef("diff takes one file")
	}
	path := "kumafile.yaml"
	if len(o.pos) == 1 {
		path = o.pos[0]
	}
	want, err := loadFile(env, path, o.envFile)
	if err != nil {
		return err
	}
	c, u, err := connect(ctx, env, o)
	if err != nil {
		return err
	}
	defer c.Close()
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
	if !p.InSync() {
		return errDifferent
	}
	return nil
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

// connect opens a logged-in connection.
func connect(ctx context.Context, env Env, o options) (*kuma.Client, string, error) {
	u, err := instanceURL(env, o)
	if err != nil {
		return nil, "", err
	}
	cr, err := credentials(env)
	if err != nil {
		return nil, "", err
	}
	c, err := kuma.Connect(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if _, err := c.Login(ctx, cr); err != nil {
		c.Close()
		return nil, "", err
	}
	return c, u, nil
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
	c, u, err := connect(ctx, env, o)
	if err != nil {
		return err
	}
	defer c.Close()
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
