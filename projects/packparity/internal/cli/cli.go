// Package cli is packparity's command line.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/parity"
	"github.com/ganeshphutane-g/packparity/internal/run"
)

// Version is set at build time.
var Version = "dev"

// Exit codes.
const (
	exitOK           = 0
	exitError        = 1
	exitUsage        = 2
	exitRepairable   = 3
	exitUnrepairable = 4
)

const usage = `packparity keeps repair data for a restic repository: Reed-Solomon parity in
another directory, so files damaged by bit rot or bad sectors can be rebuilt
and proven correct by their SHA-256 names.

Usage:
  packparity sync   [flags] REPO PARITY   make parity for new files, retire it for gone ones
  packparity check  [flags] REPO PARITY   read files and find damage
  packparity repair [flags] REPO PARITY [NAME|ID...]   rebuild damaged files
  packparity status [flags] REPO PARITY   coverage and last runs
  packparity version

REPO is the repository's directory (a local repository, rest-server's folder
for it, or a mounted share). PARITY is a directory, best on another disk.

Run "packparity COMMAND -h" for a command's flags.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 damage that repair can fix (or a
changed config), 4 damage that cannot be repaired.
`

// Main runs packparity and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "version", "--version":
		fmt.Fprintln(stdout, "packparity", Version)
		return exitOK
	case "sync":
		return cmdSync(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "repair":
		return cmdRepair(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "packparity: unknown command %q\n\n%s", args[0], usage)
	return exitUsage
}

func flags(name string, stderr io.Writer, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: packparity %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags that may come before or after the positional
// arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "packparity:", err)
	return exitError
}

func days(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration (7d, 36h)", s)
	}
	return d, nil
}

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flags("sync", stderr, "sync [flags] REPO PARITY")
	red := fs.Int("redundancy", parity.Defaults.Redundancy, "parity per data for new files, in percent (1-100)")
	keep := fs.String("keep-removed", "7d", "how long to keep parity for files that are gone")
	mass := fs.Bool("allow-mass-removal", false, "retire parity even when more than a quarter of the files are gone")
	accept := fs.Bool("accept-config", false, "take a changed config as the new one (after restic migrate)")
	quiet := fs.Bool("q", false, "print only problems")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 2 {
		fs.Usage()
		return exitUsage
	}
	kr, err := days(*keep)
	if err != nil {
		fmt.Fprintln(stderr, "packparity: --keep-removed:", err)
		return exitUsage
	}
	if kr == 0 {
		kr = time.Nanosecond // 0d: retire at once
	}
	o := run.Options{Repo: pos[0], Parity: pos[1], Params: parity.Params{Redundancy: *red, BlockSize: parity.DefaultBlockSize},
		KeepRemoved: kr, AllowMassRemoval: *mass, AcceptConfig: *accept}
	if !*quiet {
		o.Out = stdout
	}
	if _, err := parity.NewLayout(1, o.Params); err != nil {
		fmt.Fprintln(stderr, "packparity: --redundancy:", err)
		return exitUsage
	}
	start := time.Now()
	res, err := run.Sync(o)
	if err != nil {
		return fail(stderr, err)
	}
	if !*quiet {
		fmt.Fprintf(stdout, "sync: %d new files covered (%s), %d retired, %d brought back, %d purged, in %s\n",
			res.Added, run.Size(res.AddedBytes), res.Removed, res.Restored, res.Purged, time.Since(start).Round(time.Millisecond))
	}
	for _, n := range res.Skipped {
		fmt.Fprintf(stdout, "skipped %s: new and not matching its name yet (still being written?); the next sync tries again\n", n)
	}
	code := exitOK
	for _, n := range res.Damaged {
		fmt.Fprintf(stdout, "DAMAGED %s: its contents do not match its name, and it has no parity to repair it from\n", n)
		code = exitUnrepairable
	}
	if res.ConfigChanged {
		fmt.Fprintln(stdout, "CONFIG CHANGED: the repository's config is not the one the parity has. If restic changed it (restic migrate), run sync --accept-config; if not, it may be damaged or this may be another repository: run packparity check")
		if code == exitOK {
			code = exitRepairable
		}
	}
	if len(res.Damaged) > 0 {
		fmt.Fprintln(stdout, "Run restic check --read-data, and see restic's troubleshooting guide for damaged files without parity.")
	}
	return code
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flags("check", stderr, "check [flags] REPO PARITY")
	read := fs.String("read", "new", `what to read: "new" (files not verified within --every), "all", or a percentage such as "5%"`)
	every := fs.String("every", "30d", `with --read new: how often each file is read`)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 2 {
		fs.Usage()
		return exitUsage
	}
	co := run.CheckOptions{}
	switch {
	case *read == "all":
		co.All = true
	case *read == "new":
		d, err := days(*every)
		if err != nil {
			fmt.Fprintln(stderr, "packparity: --every:", err)
			return exitUsage
		}
		co.Every = d
	case strings.HasSuffix(*read, "%"):
		p, err := strconv.ParseFloat(strings.TrimSuffix(*read, "%"), 64)
		if err != nil || p <= 0 || p > 100 {
			fmt.Fprintf(stderr, "packparity: --read %s: a percentage must be above 0 and at most 100\n", *read)
			return exitUsage
		}
		co.Sample = p
	default:
		fmt.Fprintf(stderr, "packparity: --read %q: use new, all or a percentage\n", *read)
		return exitUsage
	}
	o := run.Options{Repo: pos[0], Parity: pos[1]}
	start := time.Now()
	res, err := run.Check(o, co)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if res.Reports == nil {
			res.Reports = []run.FileReport{}
		}
		enc.Encode(res)
	} else {
		fmt.Fprintf(stdout, "check: read %d files (%s) in %s\n", res.Read, run.Size(res.Bytes), time.Since(start).Round(time.Millisecond))
		notes := map[string]int{}
		for _, r := range res.Reports {
			switch r.State {
			case run.Missing, run.NoParity:
				notes[r.State]++
			default:
				fmt.Fprintf(stdout, "%-15s %s: %s\n", strings.ToUpper(r.State), r.Name, r.Detail)
			}
		}
		if n := notes[run.NoParity]; n > 0 {
			fmt.Fprintf(stdout, "note: %d files have no parity yet; run packparity sync\n", n)
		}
		if n := notes[run.Missing]; n > 0 {
			fmt.Fprintf(stdout, "note: %d files with parity are gone from the repository: pruned (sync retires their parity) or lost (restic check will say)\n", n)
		}
	}
	switch res.Worst() {
	case run.Unrepairable, run.Mismatch:
		if !*asJSON {
			fmt.Fprintln(stdout, "Some damage cannot be repaired from parity: see restic's troubleshooting guide (restic repair packs, then repair index and snapshots).")
		}
		return exitUnrepairable
	case run.Repairable, run.ParityDamaged:
		if !*asJSON {
			fmt.Fprintln(stdout, "Run packparity repair to fix it.")
		}
		return exitRepairable
	}
	return exitOK
}

func cmdRepair(args []string, stdout, stderr io.Writer) int {
	fs := flags("repair", stderr, "repair [flags] REPO PARITY [NAME|ID...]")
	dry := fs.Bool("dry-run", false, "rebuild and prove, but write nothing")
	out := fs.String("out", "", "write rebuilt files to this directory instead of the repository")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) < 2 {
		fs.Usage()
		return exitUsage
	}
	o := run.Options{Repo: pos[0], Parity: pos[1]}
	res, err := run.Repair(o, run.RepairOptions{Names: pos[2:], DryRun: *dry, OutDir: *out})
	if err != nil {
		return fail(stderr, err)
	}
	if len(res) == 0 {
		fmt.Fprintln(stdout, "repair: nothing known to be damaged (run packparity check, or name the files)")
		return exitOK
	}
	code := exitOK
	for _, r := range res {
		fmt.Fprintf(stdout, "%-14s %s", r.Done, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(stdout, ": %s", r.Detail)
		}
		fmt.Fprintln(stdout)
		if r.Done == "failed" {
			code = exitUnrepairable
		}
	}
	if code == exitOK && !*dry && *out == "" {
		fmt.Fprintln(stdout, "Run restic check --read-data to confirm.")
	}
	return code
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flags("status", stderr, "status [flags] REPO PARITY")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 2 {
		fs.Usage()
		return exitUsage
	}
	st, err := run.GetStatus(run.Options{Repo: pos[0], Parity: pos[1]})
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return exitOK
	}
	pct := 0.0
	if st.RepoBytes > 0 {
		pct = float64(st.ParityBytes) / float64(st.RepoBytes) * 100
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	fmt.Fprintf(stdout, "files       %d of %d covered\n", st.Covered, st.Files)
	fmt.Fprintf(stdout, "size        repository %s, parity %s (%.1f%%)\n", run.Size(st.RepoBytes), run.Size(st.ParityBytes), pct)
	fmt.Fprintf(stdout, "removed     %d parity files kept (%s)\n", st.RemovedFiles, run.Size(st.RemovedBytes))
	fmt.Fprintf(stdout, "last sync   %s\n", when(st.LastSync))
	fmt.Fprintf(stdout, "last check  %s; oldest verification %s", when(st.LastCheck), when(st.OldestVerified))
	if st.NeverVerified > 0 {
		fmt.Fprintf(stdout, ", %d never verified", st.NeverVerified)
	}
	fmt.Fprintln(stdout)
	if st.Quarantined > 0 {
		fmt.Fprintf(stdout, "quarantine  %d damaged files kept after repair\n", st.Quarantined)
	}
	if !st.ConfigHasParity {
		fmt.Fprintln(stdout, "config      no parity yet")
	} else if !st.ConfigMatches {
		fmt.Fprintln(stdout, "config      CHANGED: not the one the parity has (see packparity check)")
	}
	if len(st.Damaged) > 0 {
		fmt.Fprintf(stdout, "damaged     %s\n", strings.Join(st.Damaged, ", "))
		return exitRepairable
	}
	return exitOK
}
