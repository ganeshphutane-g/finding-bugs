// Package cli is packparity's command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
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
                                          (--init the first time)
  packparity check  [flags] REPO PARITY   read files and find damage
  packparity repair [flags] REPO PARITY [NAME|ID...]   rebuild damaged files
  packparity status [flags] REPO PARITY   coverage, damage known, last runs
  packparity version

REPO is the repository's directory (a local repository, rest-server's folder
for it, or a mounted share). PARITY is a directory, best on another disk.

Run "packparity COMMAND -h" for a command's flags.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 damage that repair can fix, or
something to settle (a changed config, a repository folder emptied), 4 damage that cannot be repaired from parity.
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
		fmt.Fprintln(stdout, "packparity", version())
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
		if len(fs.Args()) > 0 && fs.Args()[0] == "--" {
			return append(pos, fs.Args()[1:]...), nil
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// version is the build's version: set by make, else from Go's build
// information (go install), else "dev".
func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}

// usageCode is the exit code for a flag error: 0 for -h.
func usageCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitUsage
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

func waitFlag(fs *flag.FlagSet) *time.Duration {
	return fs.Duration("wait", time.Hour, "how long to wait for another packparity run on the same PARITY to finish")
}

func printJSON(stdout io.Writer, v any) {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flags("sync", stderr, "sync [flags] REPO PARITY")
	initDir := fs.Bool("init", false, "start a new parity directory (it must be missing or empty)")
	red := fs.Int("redundancy", parity.Defaults.Redundancy, "parity per data for new files, in percent (1-100)")
	keep := fs.String("keep-removed", "7d", "how long to keep parity for files that are gone")
	mass := fs.Bool("allow-mass-removal", false, "retire parity even when data/, index/ or keys/ has none of its files left")
	accept := fs.Bool("accept-config", false, "take a changed config as the new one (after restic migrate)")
	quiet := fs.Bool("q", false, "print only problems")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	wait := waitFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return usageCode(err)
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
		kr = time.Nanosecond // 0d: purge at once
	}
	o := run.Options{Repo: pos[0], Parity: pos[1], Params: parity.Params{Redundancy: *red, BlockSize: parity.DefaultBlockSize},
		Init: *initDir, Wait: *wait, KeepRemoved: kr, AllowMassRemoval: *mass, AcceptConfig: *accept}
	if !*quiet && !*asJSON {
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
	code := exitOK
	if len(res.Damaged) > 0 {
		code = exitUnrepairable
	} else if res.ConfigChanged || res.HeldBack > 0 {
		code = exitRepairable
	}
	if *asJSON {
		printJSON(stdout, res)
		return code
	}
	if !*quiet {
		fmt.Fprintf(stdout, "sync: %d new files covered (%s), %d retired, %d brought back, %d purged, in %s\n",
			res.Added, run.Size(res.AddedBytes), res.Removed, res.Restored, res.Purged, time.Since(start).Round(time.Millisecond))
		for _, n := range res.Skipped {
			fmt.Fprintf(stdout, "skipped %s: new and not matching its name yet (still being written?); the next sync tries again\n", n)
		}
		if res.SameDisk {
			fmt.Fprintln(stdout, "note: PARITY is on the same filesystem as REPO; a second disk also survives losing the first")
		}
		if res.ConfigParityRemade {
			fmt.Fprintln(stdout, "note: config's parity was damaged; it was made again from the current config")
		}
	}
	for _, n := range res.Ignored {
		fmt.Fprintf(stdout, "IGNORED %s: not a regular file (restic makes none); it has no parity\n", n)
	}
	for _, n := range res.Damaged {
		fmt.Fprintf(stdout, "DAMAGED %s: it does not match its name, and it has no parity to repair it from\n", n)
	}
	if len(res.Damaged) > 0 {
		fmt.Fprintln(stdout, "Run restic check --read-data, and see restic's troubleshooting guide for damaged files without parity.")
	}
	if res.HeldBack > 0 {
		fmt.Fprintf(stdout, "HELD BACK: %s/ has none of its files left (%d files with parity are gone); their parity is kept. Is a disk unmounted or a link broken? If the files really are gone, run sync --allow-mass-removal\n", res.EmptyFolder, res.HeldBack)
	}
	if res.ConfigChanged {
		fmt.Fprintln(stdout, "CONFIG CHANGED: the repository's config is not the one the parity has. If restic changed it (restic migrate), run sync --accept-config. If restic cannot open the repository, config may be damaged: run packparity repair REPO PARITY config")
	}
	return code
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flags("check", stderr, "check [flags] REPO PARITY")
	read := fs.String("read", "new", `what to read: "new" (each file once within --every), "all", or a percentage such as "5%"`)
	every := fs.String("every", "30d", `with --read new: how often each file is read`)
	asJSON := fs.Bool("json", false, "print JSON")
	wait := waitFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return usageCode(err)
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
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "packparity: --every %s: give a time above zero (30d, 12h); --read all reads everything\n", *every)
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
	o := run.Options{Repo: pos[0], Parity: pos[1], Wait: *wait}
	start := time.Now()
	res, err := run.Check(o, co)
	if err != nil {
		return fail(stderr, err)
	}
	code := exitOK
	switch res.Worst() {
	case run.Unrepairable, run.Mismatch:
		code = exitUnrepairable
	case run.Repairable, run.ParityDamaged, run.ConfigChanged:
		code = exitRepairable
	}
	if *asJSON {
		if res.Reports == nil {
			res.Reports = []run.FileReport{}
		}
		printJSON(stdout, res)
		return code
	}
	fmt.Fprintf(stdout, "check: read %d files (%s) in %s\n", res.Read, run.Size(res.Bytes), time.Since(start).Round(time.Millisecond))
	notes := map[string]int{}
	repairable := false
	for _, r := range res.Reports {
		switch r.State {
		case run.Missing, run.NoParity:
			notes[r.State]++
		default:
			fmt.Fprintf(stdout, "%-15s %s: %s\n", strings.ToUpper(r.State), r.Name, r.Detail)
		}
		if r.State == run.Repairable || r.State == run.ParityDamaged {
			repairable = true
		}
	}
	if n := notes[run.NoParity]; n > 0 {
		fmt.Fprintf(stdout, "note: %d files have no parity yet; run packparity sync\n", n)
	}
	if n := notes[run.Missing]; n > 0 {
		fmt.Fprintf(stdout, "note: %d files with parity are gone from the repository: pruned (sync retires their parity) or lost (restic check will say)\n", n)
	}
	if code == exitUnrepairable {
		fmt.Fprintln(stdout, "Some damage cannot be repaired from parity: see restic's troubleshooting guide (restic repair packs, then repair index and snapshots).")
	}
	if repairable {
		fmt.Fprintln(stdout, "Run packparity repair REPO PARITY to fix what can be fixed.")
	}
	return code
}

func cmdRepair(args []string, stdout, stderr io.Writer) int {
	fs := flags("repair", stderr, "repair [flags] REPO PARITY [NAME|ID...]")
	dry := fs.Bool("dry-run", false, "rebuild and prove, but write nothing")
	out := fs.String("out", "", "write rebuilt files to this directory instead of the repository")
	asJSON := fs.Bool("json", false, "print JSON")
	wait := waitFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return usageCode(err)
	}
	if len(pos) < 2 {
		fs.Usage()
		return exitUsage
	}
	o := run.Options{Repo: pos[0], Parity: pos[1], Wait: *wait}
	res, err := run.Repair(o, run.RepairOptions{Names: pos[2:], DryRun: *dry, OutDir: *out})
	if err != nil {
		return fail(stderr, err)
	}
	code := exitOK
	for _, r := range res {
		if r.Failed() {
			code = exitUnrepairable
		}
	}
	if *asJSON {
		if res == nil {
			res = []run.RepairResult{}
		}
		printJSON(stdout, res)
		return code
	}
	if len(res) == 0 {
		fmt.Fprintln(stdout, "repair: nothing known to be damaged (run packparity check, or name the files)")
		return exitOK
	}
	repaired := false
	for _, r := range res {
		fmt.Fprintf(stdout, "%-19s %s", r.Done, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(stdout, ": %s", r.Detail)
		}
		fmt.Fprintln(stdout)
		repaired = repaired || r.Done == run.Repaired
	}
	if repaired && !*dry && *out == "" {
		fmt.Fprintln(stdout, "Run restic check --read-data to confirm.")
	}
	return code
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flags("status", stderr, "status [flags] REPO PARITY")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return usageCode(err)
	}
	if len(pos) != 2 {
		fs.Usage()
		return exitUsage
	}
	st, err := run.GetStatus(run.Options{Repo: pos[0], Parity: pos[1]})
	if err != nil {
		return fail(stderr, err)
	}
	code := exitOK
	if len(st.DamagedNoParity) > 0 {
		code = exitUnrepairable
	} else if len(st.Damaged) > 0 || st.Config == "changed" || st.Config == "parity damaged" {
		code = exitRepairable
	}
	if *asJSON {
		printJSON(stdout, st)
		return code
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
	if st.Config != "matches" {
		fmt.Fprintf(stdout, "config      %s (see packparity check)\n", strings.ToUpper(st.Config))
	}
	if len(st.Damaged) > 0 {
		fmt.Fprintf(stdout, "damaged     %s (packparity repair can fix these)\n", strings.Join(st.Damaged, ", "))
	}
	if len(st.DamagedNoParity) > 0 {
		fmt.Fprintf(stdout, "damaged     %s (no parity: see restic's troubleshooting guide)\n", strings.Join(st.DamagedNoParity, ", "))
	}
	return code
}
