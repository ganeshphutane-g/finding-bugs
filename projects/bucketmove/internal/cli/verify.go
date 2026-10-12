package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/verify"
)

const verifyUsage = `Usage: bucketmove verify --from SOURCE --to TARGET [OPTIONS]

Compare buckets version by version: the same keys, the same number and order of
versions, the same delete markers, and the same content (SHA-256), headers, user
metadata and tags. Where the target cannot keep versions or tags (as found by a
probe), only what it can keep is compared. Exits with 1 on any difference.

Options:
      --from ENDPOINT   The original server
      --to ENDPOINT     The server the buckets were copied to
      --bucket NAME     Only these buckets (repeatable, or comma-separated)
      --quick           Compare sizes instead of reading and hashing all content
      --format FORMAT   table (default) or json
`

func runVerify(ctx context.Context, env Env, args []string) error {
	fs := flagSet("verify")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	quick := fs.Bool("quick", false, "")
	format := fs.String("format", "table", "")
	var buckets stringList
	fs.Var(&buckets, "bucket", "")
	if help, err := parseFlags(fs, args, verifyUsage, env.Stdout); help || err != nil {
		return err
	}
	if *format != "table" && *format != "json" {
		return usagef("verify: --format must be table or json")
	}
	srcEP, src, err := connect(env, *from, "from")
	if err != nil {
		return err
	}
	dstEP, dst, err := connect(env, *to, "to")
	if err != nil {
		return err
	}
	caps, err := probe.Run(ctx, dst)
	if err != nil {
		return fmt.Errorf("%s: %w", dstEP.Name, err)
	}
	inv, err := inventory.Take(ctx, src, inventory.Options{Buckets: buckets})
	if err != nil {
		return fmt.Errorf("%s: %w", srcEP.Name, err)
	}
	opts := verifyOptionsFor(caps)
	opts.SkipContent = *quick
	if *format == "table" {
		opts.Progress = func(keys int64) {
			if keys%200 == 0 {
				fmt.Fprintf(env.Stderr, "\r%s keys compared...\033[K", count(keys))
			}
		}
	}

	var results []verify.Result
	bad := 0
	for _, b := range inv {
		res, err := verify.Bucket(ctx, src, dst, b.Name, opts)
		fmt.Fprint(env.Stderr, "\r\033[K")
		if err != nil {
			return fmt.Errorf("%s: %w", b.Name, err)
		}
		results = append(results, res)
		if !res.OK() {
			bad++
		}
		if *format == "table" {
			if res.OK() {
				fmt.Fprintf(env.Stdout, "%s: identical (%s keys, %s versions).\n", b.Name, count(res.Keys), count(res.Versions))
			} else {
				writeDiffs(env, res)
			}
		}
	}
	if *format == "json" {
		if results == nil {
			results = []verify.Result{}
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else if opts.LatestOnly || opts.SkipTags {
		fmt.Fprintln(env.Stdout, "\nThe target keeps no version history or tags, so only what it can keep was compared.")
	}
	if bad > 0 {
		return exitCode(1)
	}
	return nil
}

func writeDiffs(env Env, res verify.Result) {
	fmt.Fprintf(env.Stdout, "%s: %d %s:\n", res.Bucket, len(res.Diffs), plural(len(res.Diffs), "difference"))
	for i, d := range res.Diffs {
		if i == 20 {
			fmt.Fprintf(env.Stdout, "  ... and %d more (use --format json for all)\n", len(res.Diffs)-20)
			break
		}
		fmt.Fprintf(env.Stdout, "  - %s\n", d)
	}
}
