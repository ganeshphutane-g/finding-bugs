// Package cli implements the pgcarry command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/plan"
	"github.com/ganeshphutane-g/pgcarry/internal/upgrade"
)

// Version is set at build time.
var Version = "dev"

const usage = `Usage: pgcarry COMMAND [OPTIONS] [SERVICE]

Upgrade the PostgreSQL service of a Docker Compose project to a new major
version, keeping the old data untouched until the new copy is verified.

Commands:
  plan       Show what an upgrade would do and what stands in the way
             (changes nothing)
  upgrade    Copy the data into the new version, verify it, then switch the
             service over (the Compose file is changed; the old data is kept).
             With --no-switch it is a rehearsal: the old service is started
             again afterwards and the new copy is kept for inspection.
  rollback   Go back to the old version and its untouched data
  cleanup    Remove the old data once you are happy with the upgrade
  version    Show the version

Options:
  -p, --project-directory DIR   The Compose project (default: current folder)
  -f, --file FILE               Compose file (repeatable), as with docker compose
      --project-name NAME       Compose project name
      --to VERSION              Major version to move to, e.g. 18
      --image IMAGE             New image (default: worked out from the current one)
      --json                    Print the plan as JSON
      --backup-dir DIR          Where dumps go (default: PROJECT_DIR/pgcarry-backup-SERVICE-TIME)
      --no-switch               Rehearse: copy and verify, then start the old service again
  -y, --yes                     Do not ask before stopping services

Example:
  pgcarry plan --to 18
`

var errUsage = errors.New("usage")

// Run runs pgcarry and returns the exit code.
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args []string) int {
	err := run(ctx, stdin, stdout, args)
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
	compose  compose.Options
	to       int
	image    string
	json     bool
	service  string
	backup   string
	noSwitch bool
	yes      bool
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
	fs.StringVar(&o.backup, "backup-dir", "", "")
	fs.BoolVar(&o.noSwitch, "no-switch", false, "")
	fs.BoolVar(&o.yes, "y", false, "")
	fs.BoolVar(&o.yes, "yes", false, "")
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

func run(ctx context.Context, stdin io.Reader, stdout io.Writer, args []string) error {
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
	case "plan", "upgrade", "rollback", "cleanup":
	default:
		return usagef("unknown command %q", cmd)
	}
	o, err := parse(args)
	if err != nil {
		return err
	}
	if err := absolute(&o.compose); err != nil {
		return err
	}
	cli0 := &docker.CLI{Bin: os.Getenv("PGCARRY_DOCKER")}
	switch cmd {
	case "rollback", "cleanup":
		return after(ctx, stdin, stdout, cmd, o, cli0)
	}
	if o.to == 0 && o.image == "" {
		return usagef("say which version to move to (--to 18) or give the image (--image)")
	}
	cli := cli0
	proj, err := compose.Load(ctx, cli, o.compose)
	if err != nil {
		return err
	}
	rep, err := plan.Build(ctx, plan.Input{CLI: cli, Project: proj, Service: o.service, To: o.to, Image: o.image})
	if err != nil {
		return err
	}
	if o.json && cmd == "plan" {
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
	if cmd == "plan" {
		return nil
	}
	return doUpgrade(ctx, stdin, stdout, o, cli, proj, rep)
}

func doUpgrade(ctx context.Context, stdin io.Reader, stdout io.Writer, o options, cli *docker.CLI, proj *compose.Project, rep *plan.Report) error {
	if !o.noSwitch && rep.SwitchProblem != "" {
		return fmt.Errorf("the Compose file cannot be changed automatically: %s\nRehearse with --no-switch, then make the change by hand", rep.SwitchProblem)
	}
	backup := o.backup
	if backup == "" {
		dir := o.compose.Dir
		if dir == "" {
			dir = "."
		}
		backup = filepath.Join(dir, fmt.Sprintf("pgcarry-backup-%s-%s", rep.Service, time.Now().Format("20060102-150405")))
	}
	backup, err := filepath.Abs(backup)
	if err != nil {
		return err
	}
	stops := append([]string{rep.Service}, rep.Dependents...)
	if o.noSwitch {
		fmt.Fprintf(stdout, "\nRehearsal: this stops %s while the data is copied (the old data is not changed), then starts them again on the old data.\n", strings.Join(stops, ", "))
	} else {
		fmt.Fprintf(stdout, "\nThis stops %s while the data is copied (the old data is not changed), changes the Compose file as shown, and starts them on PostgreSQL %d.\n", strings.Join(stops, ", "), rep.Target.Major())
	}
	fmt.Fprintf(stdout, "Dumps go to %s.\n", backup)
	if !o.yes && !confirm(stdin, stdout, "Go ahead?") {
		return errors.New("stopped; nothing was changed")
	}
	m, err := upgrade.Run(ctx, upgrade.Options{CLI: cli, Project: proj, Plan: rep, BackupDir: backup,
		Switch: !o.noSwitch, Log: func(s string) { fmt.Fprintln(stdout, s) }})
	if err != nil {
		return err
	}
	if m.Switched {
		fmt.Fprintf(stdout, "\nDone: %s now runs PostgreSQL %s on %s.\n", rep.Service, m.NewVersion, m.NewData)
		if len(m.Stopped) > 0 {
			fmt.Fprintf(stdout, "Started again: %s.\n", strings.Join(m.Stopped, ", "))
		}
		fmt.Fprintf(stdout, "The old data (%s) is kept unchanged, and the original Compose file is saved in %s.\n", m.OldData, backup)
		fmt.Fprintf(stdout, "To go back:           pgcarry rollback --backup-dir %s\n", backup)
		fmt.Fprintf(stdout, "When you are happy:   pgcarry cleanup --backup-dir %s  (removes the old data)\n", backup)
		return nil
	}
	fmt.Fprintf(stdout, "\nRehearsal passed: PostgreSQL %s holds an exact copy (every table's row count, every sequence, roles and object counts match).\n", m.NewVersion)
	fmt.Fprintf(stdout, "The new data is in %s %s; %s run as before on the old data.\n", rep.MountType, m.NewData, strings.Join(stops, ", "))
	fmt.Fprintf(stdout, "Dumps and a manifest are in %s.\n", backup)
	return nil
}

func confirm(in io.Reader, out io.Writer, q string) bool {
	fmt.Fprintf(out, "%s [y/N] ", q)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// absolute makes the project folder and files absolute, so rollback and
// cleanup work from anywhere later.
func absolute(o *compose.Options) error {
	dir := o.Dir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	o.Dir = abs
	for i, f := range o.Files {
		if !filepath.IsAbs(f) {
			o.Files[i] = filepath.Join(abs, f)
		}
	}
	return nil
}

func after(ctx context.Context, stdin io.Reader, stdout io.Writer, cmd string, o options, cli *docker.CLI) error {
	dir := o.backup
	if dir == "" {
		var err error
		if dir, err = upgrade.LatestBackup(o.compose.Dir); err != nil {
			return err
		}
	}
	m, err := upgrade.LoadManifest(dir)
	if err != nil {
		return err
	}
	switch cmd {
	case "rollback":
		if err := upgrade.CanRollback(m); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "This puts back the original Compose file and starts %s on PostgreSQL %s with the old data (%s).\n", m.Service, m.OldVersion, m.OldData)
		fmt.Fprintf(stdout, "Anything written to the database since the upgrade is not carried back.\n")
		if !o.yes && !confirm(stdin, stdout, "Roll back?") {
			return errors.New("stopped; nothing was changed")
		}
		if err := upgrade.Rollback(ctx, cli, dir); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Rolled back: %s runs PostgreSQL %s on %s again. The new data (%s) is kept.\n", m.Service, m.OldVersion, m.OldData, m.NewData)
	case "cleanup":
		if err := upgrade.CanCleanup(m); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "This removes the old data %s %s for good. %s keeps running on %s; the dumps stay in %s.\n", m.MountType, m.OldData, m.Service, m.NewData, dir)
		if !o.yes && !confirm(stdin, stdout, "Remove the old data?") {
			return errors.New("stopped; nothing was changed")
		}
		if err := upgrade.Cleanup(ctx, cli, dir); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed %s.\n", m.OldData)
		if m.MountType == "volume" {
			fmt.Fprintf(stdout, "The old volume's entry under volumes: in the Compose file is no longer used; you can delete it.\n")
		}
	}
	return nil
}
