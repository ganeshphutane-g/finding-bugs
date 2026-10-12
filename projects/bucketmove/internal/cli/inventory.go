package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
)

// stringList is a repeatable string flag that also splits on commas.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

const inventoryUsage = `Usage: bucketmove inventory --from ENDPOINT [OPTIONS]

List every bucket with what it contains (objects, old versions, delete markers,
sizes) and which features it uses (versioning, object lock, lifecycle rules,
policies, CORS, tags, encryption, notifications, replication).

Options:
      --from ENDPOINT   The server to inventory (URL with credentials or mc alias)
      --bucket NAME     Only these buckets (repeatable, or comma-separated)
      --format FORMAT   table (default) or json
`

func runInventory(ctx context.Context, env Env, args []string) error {
	fs := flagSet("inventory")
	from := fs.String("from", "", "")
	format := fs.String("format", "table", "")
	var buckets stringList
	fs.Var(&buckets, "bucket", "")
	if help, err := parseFlags(fs, args, inventoryUsage, env.Stdout); help || err != nil {
		return err
	}
	if *format != "table" && *format != "json" {
		return usagef("inventory: --format must be table or json")
	}
	ep, c, err := connect(env, *from, "from")
	if err != nil {
		return err
	}
	opts := inventory.Options{Buckets: buckets}
	if *format == "table" && isTerminal(env.Stderr) {
		opts.Progress = func(bucket string, n int64) {
			fmt.Fprintf(env.Stderr, "\r%s: %s versions counted...", bucket, count(n))
		}
	}
	inv, err := inventory.Take(ctx, c, opts)
	if opts.Progress != nil {
		clearLine(env)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", ep.Name, err)
	}
	if *format == "json" {
		if inv == nil {
			inv = []inventory.Bucket{}
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(inv)
	}
	writeInventory(env.Stdout, ep.Name, inv)
	return nil
}

func writeInventory(w io.Writer, name string, inv []inventory.Bucket) {
	if len(inv) == 0 {
		fmt.Fprintf(w, "%s has no buckets.\n", name)
		return
	}
	var objects, old, markers, bytes int64
	for _, b := range inv {
		objects += b.Objects
		old += b.OldVersions
		markers += b.DeleteMarkers
		bytes += b.TotalBytes
	}
	fmt.Fprintf(w, "%s: %d %s, %s objects, %s old versions, %s delete markers, %s in all versions.\n\n",
		name, len(inv), plural(len(inv), "bucket"), count(objects), count(old), count(markers), humanSize(bytes))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET\tOBJECTS\tOLD VERSIONS\tDELETE MARKERS\tSIZE (ALL VERSIONS)\tFEATURES")
	for _, b := range inv {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", b.Name, count(b.Objects), count(b.OldVersions),
			count(b.DeleteMarkers), humanSize(b.TotalBytes), strings.Join(features(b), ", "))
	}
	tw.Flush()

	var unsupported []string
	for _, b := range inv {
		for _, u := range b.Unsupported {
			unsupported = append(unsupported, b.Name+": "+u)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		fmt.Fprintf(w, "\nThe server could not report: %s.\n", strings.Join(unsupported, "; "))
	}
}

// features lists, in words, the settings and object features a bucket uses.
func features(b inventory.Bucket) []string {
	var f []string
	switch b.Versioning {
	case "Enabled":
		f = append(f, "versioning")
	case "Suspended":
		f = append(f, "versioning (suspended)")
	}
	if b.ObjectLock {
		s := "object lock"
		if b.LockDefault != "" {
			s += " (default " + b.LockDefault + ")"
		}
		f = append(f, s)
	}
	if b.Retained > 0 {
		f = append(f, count(b.Retained)+" under retention")
	}
	if b.LegalHolds > 0 {
		f = append(f, count(b.LegalHolds)+" on legal hold")
	}
	if b.LifecycleRules > 0 {
		f = append(f, fmt.Sprintf("%d lifecycle %s", b.LifecycleRules, plural(b.LifecycleRules, "rule")))
	}
	if b.Policy {
		f = append(f, "bucket policy")
	}
	if b.CORSRules > 0 {
		f = append(f, "CORS")
	}
	if len(b.Tags) > 0 {
		f = append(f, "bucket tags")
	}
	if b.TaggedVersions > 0 {
		f = append(f, count(b.TaggedVersions)+" tagged")
	}
	if b.Metadata > 0 {
		f = append(f, count(b.Metadata)+" with metadata")
	}
	if b.Encryption != "" {
		f = append(f, "encryption ("+b.Encryption+")")
	}
	if b.Notifications > 0 {
		f = append(f, "notifications")
	}
	if b.Replication > 0 {
		f = append(f, "replication")
	}
	if len(f) == 0 {
		f = append(f, "-")
	}
	return f
}
