package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ganeshphutane-g/bucketmove/internal/check"
	"github.com/ganeshphutane-g/bucketmove/internal/endpoint"
	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/minio/minio-go/v7"
)

// exitCode makes Run return a specific code without printing an error.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit code %d", int(e)) }

// lossExitCode is returned by check when something would be lost.
const lossExitCode = 3

const checkUsage = `Usage: bucketmove check --from SOURCE --to TARGET [OPTIONS]

Try every feature on the target with a scratch bucket (bucketmove-probe-*,
removed afterwards), then say for each source bucket what would be kept and
what would be lost in a move. Exits with 3 when something would be lost.

Options:
      --from ENDPOINT   The server to move from
      --to ENDPOINT     The server to move to
      --bucket NAME     Only these buckets (repeatable, or comma-separated)
      --format FORMAT   table (default) or json
`

func runCheck(ctx context.Context, env Env, args []string) error {
	fs := flagSet("check")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	format := fs.String("format", "table", "")
	var buckets stringList
	fs.Var(&buckets, "bucket", "")
	if help, err := parseFlags(fs, args, checkUsage, env.Stdout); help || err != nil {
		return err
	}
	if *format != "table" && *format != "json" {
		return usagef("check: --format must be table or json")
	}
	srcEP, src, err := connect(env, *from, "from")
	if err != nil {
		return err
	}
	dstEP, dst, err := connect(env, *to, "to")
	if err != nil {
		return err
	}

	report, err := buildReport(ctx, env, srcEP, src, dstEP, dst, buckets, *format == "table")
	if err != nil {
		return err
	}
	if *format == "json" {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		writeReport(env.Stdout, dstEP.Name, report)
	}
	if report.HasLoss() {
		return exitCode(lossExitCode)
	}
	return nil
}

// buildReport inventories the source, probes the target and compares them.
func buildReport(ctx context.Context, env Env, srcEP endpoint.Endpoint, src *minio.Client, dstEP endpoint.Endpoint, dst *minio.Client, buckets []string, progress bool) (check.Report, error) {
	opts := inventory.Options{Buckets: buckets}
	if progress {
		opts.Progress = func(bucket string, n int64) {
			fmt.Fprintf(env.Stderr, "\r%s: %s versions counted...", bucket, count(n))
		}
		fmt.Fprintf(env.Stderr, "Reading %s...", srcEP.Name)
	}
	inv, err := inventory.Take(ctx, src, opts)
	if progress {
		fmt.Fprint(env.Stderr, "\r\033[K")
	}
	if err != nil {
		return check.Report{}, fmt.Errorf("%s: %w", srcEP.Name, err)
	}
	if progress {
		fmt.Fprintf(env.Stderr, "Testing what %s supports...", dstEP.Name)
	}
	caps, err := probe.Run(ctx, dst)
	if progress {
		fmt.Fprint(env.Stderr, "\r\033[K")
	}
	if err != nil {
		return check.Report{}, fmt.Errorf("%s: %w", dstEP.Name, err)
	}
	existing := map[string]bool{}
	list, err := dst.ListBuckets(ctx)
	if err != nil {
		return check.Report{}, fmt.Errorf("%s: listing buckets: %w", dstEP.Name, err)
	}
	for _, b := range list {
		existing[b.Name] = true
	}
	return check.Compare(inv, caps, existing), nil
}

func writeReport(w io.Writer, target string, r check.Report) {
	var yes, no []string
	for _, f := range probe.Order {
		res := r.Target[f]
		if res.Supported {
			yes = append(yes, f)
		} else {
			no = append(no, fmt.Sprintf("%s (%s)", f, res.Detail))
		}
	}
	fmt.Fprintf(w, "%s supports: %s.\n", target, joinOr(yes, "none of the tested features"))
	if len(no) > 0 {
		fmt.Fprintf(w, "It does not support:\n")
		for _, n := range no {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	fmt.Fprintln(w)

	losing := 0
	for _, b := range r.Buckets {
		status := "OK"
		if len(b.Losses) > 0 {
			status = "WOULD LOSE DATA OR SETTINGS"
			losing++
		}
		extra := ""
		if b.TargetExists {
			extra = " (a bucket with this name already exists on the target)"
		}
		fmt.Fprintf(w, "%s: %s%s\n", b.Name, status, extra)
		for _, l := range b.Losses {
			fmt.Fprintf(w, "  - %s\n", l.What)
		}
		for _, c := range b.Changes {
			fmt.Fprintf(w, "  - changes: %s\n", c)
		}
		if len(b.Manual) > 0 {
			fmt.Fprintf(w, "  - not copied by bucketmove, set up by hand: %s\n", strings.Join(b.Manual, ", "))
		}
	}
	fmt.Fprintln(w)
	switch {
	case len(r.Buckets) == 0:
		fmt.Fprintln(w, "No buckets to move.")
	case losing == 0:
		fmt.Fprintf(w, "All %d %s can move without losing anything bucketmove checks.\n", len(r.Buckets), plural(len(r.Buckets), "bucket"))
	default:
		fmt.Fprintf(w, "%d of %d %s would lose something. 'bucketmove copy' refuses to start unless you pass --accept-loss.\n",
			losing, len(r.Buckets), plural(len(r.Buckets), "bucket"))
	}
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}
