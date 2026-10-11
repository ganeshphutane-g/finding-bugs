// Package cli implements the docker-mop commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
)

// Version is set at build time with -ldflags "-X ...cli.Version=v1.2.3".
var Version = "dev"

const pluginName = "mop"

// metadataCommand is the hidden subcommand the docker CLI calls to discover plugins.
const metadataCommand = "docker-cli-plugin-metadata"

// Env is everything a command needs from the outside world, so tests can
// swap any of it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// NewClient connects to the daemon. Tests may replace it.
	NewClient func(engine.Endpoint) (*engine.Client, error)
}

// DefaultEnv uses the real process environment.
func DefaultEnv() Env {
	return Env{
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Getenv:    os.Getenv,
		NewClient: engine.New,
	}
}

// errUsage marks errors caused by bad arguments; they exit with code 2.
var errUsage = errors.New("usage error")

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }
func (e usageError) Is(target error) bool {
	return target == errUsage
}

func usagef(format string, args ...any) error {
	return usageError{msg: fmt.Sprintf(format, args...)}
}

// Run executes docker-mop with the given arguments (without the program name)
// and returns the process exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "docker mop: %v\nRun 'docker mop help' for usage.\n", err)
		return 2
	default:
		fmt.Fprintf(env.Stderr, "docker mop: %v\n", err)
		return 1
	}
}

func run(ctx context.Context, env Env, args []string) error {
	if len(args) > 0 && args[0] == metadataCommand {
		return printMetadata(env.Stdout)
	}
	// When called by the docker CLI, the arguments are docker's own global
	// flags (if any), then the plugin name.
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && containsArg(args, pluginName) {
		var err error
		if args, env, err = globalFlags(args, env); err != nil {
			return err
		}
	}
	if len(args) > 0 && args[0] == pluginName {
		args = args[1:]
	}
	if len(args) == 0 {
		printHelp(env.Stdout)
		return nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		printHelp(env.Stdout)
		return nil
	case "version", "--version":
		return runVersion(ctx, env, rest)
	case "plan", "preview":
		return runPlan(ctx, env, rest)
	case "prune":
		return runPrune(ctx, env, rest)
	case "volume", "volumes":
		return runVolume(ctx, env, rest)
	default:
		return usagef("unknown command %q", cmd)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// globalFlags applies the docker CLI's global flags that come before the
// plugin name (docker --context prod mop plan), by overriding the matching
// environment variables, and returns the arguments from the plugin name on.
func globalFlags(args []string, env Env) ([]string, Env, error) {
	overrides := map[string]string{}
	for len(args) > 0 && args[0] != pluginName {
		name, value, hasValue := strings.Cut(args[0], "=")
		takeValue := func() (string, error) {
			if hasValue {
				args = args[1:]
				return value, nil
			}
			if len(args) < 2 {
				return "", usagef("docker flag %s needs a value", name)
			}
			v := args[1]
			args = args[2:]
			return v, nil
		}
		var err error
		switch name {
		case "-c", "--context":
			overrides["DOCKER_CONTEXT"], err = takeValue()
		case "-H", "--host":
			overrides["DOCKER_HOST"], err = takeValue()
		case "--config":
			overrides["DOCKER_CONFIG"], err = takeValue()
		case "-l", "--log-level":
			_, err = takeValue()
		case "--tls":
			overrides["DOCKER_TLS"] = "1"
			args = args[1:]
		case "--tlsverify":
			overrides["DOCKER_TLS_VERIFY"] = "1"
			args = args[1:]
		case "-D", "--debug":
			args = args[1:]
		default:
			return nil, env, usagef("docker flag %s is not supported by docker mop; set the matching DOCKER_* environment variable instead", name)
		}
		if err != nil {
			return nil, env, err
		}
	}
	if len(overrides) > 0 {
		base := env.Getenv
		env.Getenv = func(k string) string {
			if v, ok := overrides[k]; ok {
				return v
			}
			return base(k)
		}
		// An explicit host or context wins over the other source.
		if _, ok := overrides["DOCKER_CONTEXT"]; ok {
			if _, hostSet := overrides["DOCKER_HOST"]; !hostSet {
				overrides["DOCKER_HOST"] = ""
			}
		}
	}
	return args, env, nil
}

func printMetadata(w io.Writer) error {
	meta := map[string]string{
		"SchemaVersion":    "0.1.0",
		"Vendor":           "Ganesh Phutane",
		"Version":          Version,
		"ShortDescription": "Preview prunes before they delete anything; rename and copy volumes",
		"URL":              "https://github.com/ganeshphutane-g/docker-mop",
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(meta)
}

const helpText = `Usage: docker mop COMMAND

See exactly what a prune will delete before it deletes anything,
and rename or copy volumes.

Commands:
  plan        Show what a prune would remove, without removing anything
  prune       Remove exactly what plan shows, after confirmation
  volume      Copy or rename volumes, with every file verified
  version     Show docker-mop and Docker daemon versions
  help        Show this help

Run 'docker mop COMMAND --help' for more information on a command.
`

func printHelp(w io.Writer) {
	fmt.Fprint(w, helpText)
}

func connect(env Env) (*engine.Client, error) {
	ep, err := engine.ResolveEndpoint(env.Getenv)
	if err != nil {
		return nil, err
	}
	return env.NewClient(ep)
}

func runVersion(ctx context.Context, env Env, args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(env.Stdout, "Usage: docker mop version\n\nShow docker-mop and Docker daemon versions.")
			return nil
		}
		return usagef("version takes no arguments, got %q", strings.Join(args, " "))
	}

	fmt.Fprintf(env.Stdout, "docker-mop:  %s\n", Version)
	client, err := connect(env)
	if err != nil {
		return err
	}
	v, err := client.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Docker:      %s (API %s, %s/%s)\n", v.Version, v.APIVersion, v.Os, v.Arch)
	fmt.Fprintf(env.Stdout, "Daemon:      %s (from %s)\n", client.Endpoint.Host, client.Endpoint.Source)
	return nil
}
