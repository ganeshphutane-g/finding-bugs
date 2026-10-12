// Package cli implements the bucketmove commands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ganeshphutane-g/bucketmove/internal/endpoint"
	"github.com/minio/minio-go/v7"
)

// Version is set at build time with -ldflags "-X ...cli.Version=v1.2.3".
var Version = "dev"

// Env is everything a command needs from the outside world.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	HomeDir string
}

// DefaultEnv uses the real process environment.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, HomeDir: home}
}

var errUsage = errors.New("usage error")

type usageError struct{ msg string }

func (e usageError) Error() string        { return e.msg }
func (e usageError) Is(target error) bool { return target == errUsage }

func usagef(format string, args ...any) error {
	return usageError{msg: fmt.Sprintf(format, args...)}
}

// Run executes bucketmove with args (without the program name) and returns
// the exit code.
func Run(ctx context.Context, env Env, args []string) int {
	err := run(ctx, env, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(env.Stderr, "bucketmove: %v\nRun 'bucketmove help' for usage.\n", err)
		return 2
	default:
		fmt.Fprintf(env.Stderr, "bucketmove: %v\n", err)
		return 1
	}
}

const helpText = `Usage: bucketmove COMMAND [OPTIONS]

Move S3 buckets off MinIO (or any S3 server) without losing object versions or
settings by accident.

Commands:
  inventory   List buckets, their contents and the features they use
  version     Show the version; with --from, also test the connection

Endpoints are URLs with credentials (https://KEY:SECRET@host:9000), the
BUCKETMOVE_FROM / BUCKETMOVE_TO variables, or MinIO client (mc) aliases.

Run 'bucketmove COMMAND --help' for more information on a command.
`

func run(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, helpText)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, helpText)
		return nil
	case "version", "--version":
		return runVersion(ctx, env, rest)
	case "inventory", "inv":
		return runInventory(ctx, env, rest)
	default:
		return usagef("unknown command %q", cmd)
	}
}

// flagSet returns a FlagSet that reports errors instead of printing them.
func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string, usage string, out io.Writer) (help bool, err error) {
	var h bool
	fs.BoolVar(&h, "h", false, "")
	fs.BoolVar(&h, "help", false, "")
	if err := fs.Parse(args); err != nil {
		return false, usagef("%s: %v", fs.Name(), err)
	}
	if h {
		fmt.Fprint(out, usage)
		return true, nil
	}
	if fs.NArg() > 0 {
		return false, usagef("%s: unexpected argument %q", fs.Name(), strings.Join(fs.Args(), " "))
	}
	return false, nil
}

// connect resolves an endpoint reference and builds a client.
func connect(env Env, ref, role string) (endpoint.Endpoint, *minio.Client, error) {
	ep, err := endpoint.Resolve(ref, role, endpoint.Env{Getenv: env.Getenv, HomeDir: env.HomeDir})
	if err != nil {
		return ep, nil, usagef("%v", err)
	}
	c, err := ep.Client(nil)
	if err != nil {
		return ep, nil, fmt.Errorf("%s: %w", ep.Name, err)
	}
	return ep, c, nil
}

const versionUsage = `Usage: bucketmove version [--from ENDPOINT]

Show the bucketmove version. With --from, also connect to the endpoint and list
its buckets, to check the address and credentials.
`

func runVersion(ctx context.Context, env Env, args []string) error {
	fs := flagSet("version")
	from := fs.String("from", "", "")
	if help, err := parseFlags(fs, args, versionUsage, env.Stdout); help || err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "bucketmove %s\n", Version)
	if *from == "" {
		return nil
	}
	ep, c, err := connect(env, *from, "from")
	if err != nil {
		return err
	}
	buckets, err := c.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", ep, err)
	}
	fmt.Fprintf(env.Stdout, "Connected to %s: %d %s.\n", ep, len(buckets), plural(len(buckets), "bucket"))
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
