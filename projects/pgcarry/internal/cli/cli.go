// Package cli implements the pgcarry command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/plan"
)

// Version is set at build time.
var Version = "dev"

const usage = `Usage: pgcarry COMMAND [OPTIONS] [SERVICE]

Upgrade the PostgreSQL service of a Docker Compose project to a new major
version, keeping the old data untouched until the new copy is verified.

Commands:
  plan       Show what an upgrade would do and what stands in the way
             (changes nothing)
  version    Show the version

Options:
  -p, --project-directory DIR   The Compose project (default: current folder)
  -f, --file FILE               Compose file (repeatable), as with docker compose
      --project-name NAME       Compose project name
      --to VERSION              Major version to move to, e.g. 18
      --image IMAGE             New image (default: worked out from the current one)
      --json                    Print the plan as JSON

Example:
  pgcarry plan --to 18
`

var errUsage = errors.New("usage")

// Run runs pgcarry and returns the exit code.
func Run(ctx context.Context, stdout, stderr io.Writer, args []string) int {
	err := run(ctx, stdout, args)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "pgcarry: %s\nRun 'pgcarry --help' for usage.\n", strings.TrimPrefix(err.Error(), "usage: "))
		return 2
	case errors.Is(err, errBlocked):
		return 3
	default:
		fmt.Fprintf(stderr, "pgcarry: %v\n", err)
		return 1
	}
}

var errBlocked = errors.New("blocked")

type options struct {
	compose compose.Options
	to      int
	image   string
	json    bool
	service string
}

func usagef(f string, a ...any) error { return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(f, a...)) }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func parse(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("pgcarry", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var files multi
	fs.StringVar(&o.compose.Dir, "p", "", "")
	fs.StringVar(&o.compose.Dir, "project-directory", "", "")
	fs.Var(&files, "f", "")
	fs.Var(&files, "file", "")
	fs.StringVar(&o.compose.Name, "project-name", "", "")
	fs.IntVar(&o.to, "to", 0, "")
	fs.StringVar(&o.image, "image", "", "")
	fs.BoolVar(&o.json, "json", false, "")
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return o, usagef("%v", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	o.compose.Files = files
	if len(pos) > 1 {
		return o, usagef("only one SERVICE can be named")
	}
	if len(pos) == 1 {
		o.service = pos[0]
	}
	if o.to < 0 {
		return o, usagef("--to must be a major version such as 18")
	}
	return o, nil
}

func run(ctx context.Context, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		return usagef("name a command")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintf(stdout, "pgcarry %s\n", Version)
		return nil
	case "plan":
	default:
		return usagef("unknown command %q", cmd)
	}
	o, err := parse(args)
	if err != nil {
		return err
	}
	if o.to == 0 && o.image == "" {
		return usagef("say which version to move to (--to 18) or give the image (--image)")
	}
	cli := &docker.CLI{Bin: os.Getenv("PGCARRY_DOCKER")}
	proj, err := compose.Load(ctx, cli, o.compose)
	if err != nil {
		return err
	}
	rep, err := plan.Build(ctx, plan.Input{CLI: cli, Project: proj, Service: o.service, To: o.to, Image: o.image})
	if err != nil {
		return err
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		rep.Render(stdout)
	}
	if !rep.OK() {
		return errBlocked
	}
	return nil
}
