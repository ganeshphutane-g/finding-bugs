// Package cli is composehost's command line: the provider protocol Compose
// speaks to it, and the commands people run.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/composehost/internal/protocol"
	"github.com/ganeshphutane-g/composehost/internal/steps"
)

// Version is set at build time.
var Version = "dev"

const usage = `composehost runs host-side steps for a Docker Compose project, declared in the
compose file as a provider service, so plain "docker compose up" runs them:

  services:
    host:
      provider:
        type: composehost
        options:
          mkdir: [./data, ./config]   # created if missing...
          owner: "1000:1000"          # ...with this owner (needs root)
          mode: "0750"                # ...and mode
          wait-mount: /mnt/nas        # wait until mounted
          wait-path: /run/thing.sock  # wait until it exists
          wait-tcp: 127.0.0.1:5432    # wait until it accepts connections
          wait-timeout: 60s
          run: ./scripts/prepare.sh   # a host command, in the project directory
          env: ./scripts/secrets.sh   # its KEY=VALUE output goes to the services
          down: ./scripts/cleanup.sh  # on docker compose down
          stop: ./scripts/pause.sh    # on docker compose stop
          timeout: 5m                 # the longest a command may take
    app:
      image: example/app
      depends_on: [host]

On up the steps run in that order: mkdir, waits, run, env. Services that
depend on the provider service start once all of them succeed; if one fails,
"docker compose up" fails with its message.

Usage:
  composehost check [-f FILE]  show a project's steps, and whether its waits are met now
  composehost compose ...      what Compose calls (up, down, stop, metadata)
  composehost version

check exit codes: 0 fine, 1 error (bad options), 3 a wait is not met yet.
`

// Metadata declares composehost's options to Compose (it passes only these).
var Metadata = protocol.Metadata{
	Description: "Host-side steps for a Compose project: directories, waits, host commands, environment values",
	Up: protocol.Command{Parameters: []protocol.Param{
		{Name: "mkdir", Description: "Create this directory if missing (repeat)", Type: "string"},
		{Name: "owner", Description: "Owner of the mkdir directories, UID:GID or USER:GROUP", Type: "string"},
		{Name: "mode", Description: "Mode of the mkdir directories, such as 0750", Type: "string"},
		{Name: "wait-mount", Description: "Wait until this path is a mount point (repeat)", Type: "string"},
		{Name: "wait-path", Description: "Wait until this path exists (repeat)", Type: "string"},
		{Name: "wait-tcp", Description: "Wait until HOST:PORT accepts connections (repeat)", Type: "string"},
		{Name: "wait-timeout", Description: "How long the waits may take in all", Type: "string", Default: "60s"},
		{Name: "run", Description: "A host command to run (sh -c, in the project directory)", Type: "string"},
		{Name: "env", Description: "A host command whose KEY=VALUE output lines go to the dependent services", Type: "string"},
		{Name: "timeout", Description: "The longest a command may take", Type: "string", Default: "5m"},
	}},
	Down: protocol.Command{Parameters: []protocol.Param{
		{Name: "down", Description: "A host command to run on docker compose down", Type: "string"},
		{Name: "timeout", Description: "The longest it may take", Type: "string", Default: "5m"},
	}},
	Stop: &protocol.Command{Parameters: []protocol.Param{
		{Name: "stop", Description: "A host command to run on docker compose stop", Type: "string"},
		{Name: "timeout", Description: "The longest it may take", Type: "string", Default: "5m"},
	}},
}

// Main runs composehost and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "composehost", version())
		return 0
	case "compose":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return Provider(ctx, args, stdout, stderr)
	case "check":
		return Check(args[1:], stdout, stderr, nil)
	}
	fmt.Fprintf(stderr, "composehost: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}

// Provider answers one call from Compose.
func Provider(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	out := &protocol.Out{W: stdout}
	call, err := protocol.ParseArgs(args, Metadata.Known())
	if err != nil {
		// Compose shows error lines; a person running it by hand reads stderr.
		out.Error(err.Error())
		fmt.Fprintln(stderr, "composehost:", err)
		return 1
	}
	if call.Command == "metadata" {
		return printMetadata(stdout)
	}
	dir, err := os.Getwd()
	if err != nil {
		out.Error(err.Error())
		return 1
	}
	if err := do(ctx, out, dir, call); err != nil {
		out.Error(err.Error())
		return 1
	}
	return 0
}

func printMetadata(w io.Writer) int {
	b, err := json.Marshal(Metadata)
	if err != nil {
		return 1
	}
	w.Write(append(b, '\n'))
	return 0
}

// plan is a call's steps, checked.
type plan struct {
	mkdir          []string
	owner          *steps.Owner
	mode           *fs.FileMode
	waits          steps.Waits
	run, env, down string
	stop           string
	timeout        time.Duration
}

// planOf checks a call's options and turns them into steps.
func planOf(call protocol.Call) (plan, error) {
	var p plan
	var err error
	if p.timeout, err = duration(call, "timeout", 5*time.Minute); err != nil {
		return p, err
	}
	switch call.Command {
	case "down":
		p.down, err = call.One("down")
		return p, err
	case "stop":
		p.stop, err = call.One("stop")
		return p, err
	}
	if v, err := call.One("owner"); err != nil {
		return p, err
	} else if v != "" {
		o, err := steps.ParseOwner(v)
		if err != nil {
			return p, err
		}
		p.owner = &o
	}
	if v, err := call.One("mode"); err != nil {
		return p, err
	} else if v != "" {
		m, err := steps.ParseMode(v)
		if err != nil {
			return p, err
		}
		p.mode = &m
	}
	p.mkdir = call.Options["mkdir"]
	if (p.owner != nil || p.mode != nil) && len(p.mkdir) == 0 {
		return p, fmt.Errorf("owner and mode apply to mkdir directories, and there are none")
	}
	p.waits = steps.Waits{Mounts: call.Options["wait-mount"], Paths: call.Options["wait-path"], TCP: call.Options["wait-tcp"]}
	if p.waits.Timeout, err = duration(call, "wait-timeout", 60*time.Second); err != nil {
		return p, err
	}
	if p.run, err = call.One("run"); err != nil {
		return p, err
	}
	p.env, err = call.One("env")
	return p, err
}

// do runs the call's steps.
func do(ctx context.Context, out *protocol.Out, dir string, call protocol.Call) error {
	p, err := planOf(call)
	if err != nil {
		return err
	}
	env := []string{"COMPOSEHOST_SERVICE=" + call.Service, "COMPOSE_PROJECT_NAME=" + call.Project}
	switch call.Command {
	case "down", "stop":
		cmd := p.down
		if call.Command == "stop" {
			cmd = p.stop
		}
		if cmd == "" {
			return nil
		}
		_, err := steps.Run(ctx, out, dir, call.Command, cmd, p.timeout, env)
		return err
	}
	// up: mkdir, waits, run, env.
	if err := steps.Mkdir(out, dir, p.mkdir, p.owner, p.mode); err != nil {
		return err
	}
	if err := steps.Wait(ctx, out, dir, p.waits); err != nil {
		return err
	}
	if p.run != "" {
		if _, err := steps.Run(ctx, out, dir, "run", p.run, p.timeout, env); err != nil {
			return err
		}
	}
	if p.env != "" {
		b, err := steps.Run(ctx, out, dir, "env", p.env, p.timeout, env)
		if err != nil {
			return err
		}
		kv, err := steps.ParseEnv(b)
		if err != nil {
			return err
		}
		for _, e := range kv {
			out.RawSetEnv(e[0], e[1])
		}
		out.Info("env: %d %s for the dependent services", len(kv), plural(len(kv), "value", "values"))
	}
	return nil
}

func duration(call protocol.Call, name string, def time.Duration) (time.Duration, error) {
	v, err := call.One(name)
	if err != nil || v == "" {
		return def, err
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q: a duration such as 30s or 5m", name, v)
	}
	return d, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
