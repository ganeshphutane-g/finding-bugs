package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/engine"
	"github.com/ganeshphutane-g/docker-mop/internal/plan"
)

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// pruneFlags are shared by `plan` and `prune`.
type pruneFlags struct {
	kind    plan.Kind
	all     bool
	volumes bool
	filters stringList
	format  string
	yes     bool
}

const planUsage = `Usage: docker mop plan [system|containers|images|volumes|networks] [OPTIONS]

Show what the matching 'docker ... prune' would remove and how much space it
would free. Nothing is removed.

Options:
  -a, --all             images: include all unused images, not only dangling ones
                        volumes: include named volumes, not only anonymous ones
      --volumes         system: also include anonymous volumes
      --filter FILTER   Same filters as docker prune: until=24h, label=key[=value],
                        label!=key[=value] (repeatable)
      --format FORMAT   table (default) or json
`

func parsePruneArgs(name string, args []string, withYes bool) (pruneFlags, bool, error) {
	var pf pruneFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&pf.all, "a", false, "")
	fs.BoolVar(&pf.all, "all", false, "")
	fs.BoolVar(&pf.volumes, "volumes", false, "")
	fs.Var(&pf.filters, "filter", "")
	fs.StringVar(&pf.format, "format", "table", "")
	if withYes {
		fs.BoolVar(&pf.yes, "y", false, "")
		fs.BoolVar(&pf.yes, "yes", false, "")
		fs.BoolVar(&pf.yes, "f", false, "")
		fs.BoolVar(&pf.yes, "force", false, "")
	}
	var help bool
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")

	kindSet := false
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return pf, false, usagef("%s: %v", name, err)
		}
		if fs.NArg() == 0 {
			break
		}
		if kindSet {
			return pf, false, usagef("%s: unexpected argument %q", name, fs.Arg(0))
		}
		k, err := plan.ParseKind(fs.Arg(0))
		if err != nil {
			return pf, false, usagef("%s: %v", name, err)
		}
		pf.kind = k
		kindSet = true
		rest = fs.Args()[1:]
	}
	if help {
		return pf, true, nil
	}
	if !kindSet {
		pf.kind = plan.System
	}
	if pf.format != "table" && pf.format != "json" {
		return pf, false, usagef("%s: --format must be table or json", name)
	}
	if pf.volumes && pf.kind != plan.System {
		return pf, false, usagef("%s: --volumes only applies to system", name)
	}
	return pf, false, nil
}

func computePlan(ctx context.Context, env Env, pf pruneFlags) (*engine.Client, plan.Plan, error) {
	filters, err := plan.ParseFilters(pf.filters)
	if err != nil {
		return nil, plan.Plan{}, usagef("%v", err)
	}
	client, err := connect(env)
	if err != nil {
		return nil, plan.Plan{}, err
	}
	// Validate flags before the (possibly slow) disk usage call.
	opts := plan.Options{Kind: pf.kind, All: pf.all, Volumes: pf.volumes, Filters: filters}
	if _, err := plan.Compute(plan.State{}, opts); err != nil {
		return nil, plan.Plan{}, usagef("%v", err)
	}
	st, err := plan.Gather(ctx, client, pf.kind)
	if err != nil {
		return nil, plan.Plan{}, err
	}
	p, err := plan.Compute(st, opts)
	return client, p, err
}

func runPlan(ctx context.Context, env Env, args []string) error {
	pf, help, err := parsePruneArgs("plan", args, false)
	if err != nil {
		return err
	}
	if help {
		fmt.Fprint(env.Stdout, planUsage)
		return nil
	}
	_, p, err := computePlan(ctx, env, pf)
	if err != nil {
		return err
	}
	if pf.format == "json" {
		return writePlanJSON(env.Stdout, p)
	}
	writePlanTable(env.Stdout, p, time.Now())
	if p.Count() > 0 {
		fmt.Fprintf(env.Stdout, "\nNothing was removed. To remove exactly these, run: docker mop prune %s\n", reconstructArgs(pf))
	}
	return nil
}

// reconstructArgs rebuilds the flags so the hint can be copied as-is.
func reconstructArgs(pf pruneFlags) string {
	parts := []string{string(pf.kind)}
	if pf.all {
		parts = append(parts, "--all")
	}
	if pf.volumes {
		parts = append(parts, "--volumes")
	}
	for _, f := range pf.filters {
		parts = append(parts, "--filter "+shellQuote(f))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writePlanJSON(w io.Writer, p plan.Plan) error {
	total, atLeast := p.Total()
	out := struct {
		plan.Plan
		Count        int   `json:"count"`
		TotalBytes   int64 `json:"total_bytes"`
		TotalAtLeast bool  `json:"total_is_lower_bound"`
	}{p, p.Count(), total, atLeast}
	for _, g := range []*[]plan.Item{&out.Containers, &out.Networks, &out.Volumes, &out.Images} {
		if *g == nil {
			*g = []plan.Item{}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writePlanTable(w io.Writer, p plan.Plan, now time.Time) {
	if p.Count() == 0 {
		fmt.Fprintln(w, "Nothing to remove.")
		writeNotes(w, p.Notes)
		return
	}
	total, atLeast := p.Total()
	freed := humanSize(total)
	if atLeast {
		freed = "at least " + freed
	}
	fmt.Fprintf(w, "Would remove %s. Frees %s.\n", summary(p), freed)

	section := func(title string, items []plan.Item, showID bool, nameHeader, noteHeader string, showSize bool) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s (%d)\n", title, len(items))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		header := nameHeader
		if showID {
			header = "ID\t" + header
		}
		if showSize {
			header += "\tSIZE"
		}
		header += "\tCREATED\t" + noteHeader
		fmt.Fprintln(tw, header)
		for _, it := range items {
			row := it.Name
			if showID {
				row = shortID(it.ID) + "\t" + row
			}
			if showSize {
				row += "\t" + humanSize(it.Size)
			}
			row += "\t" + humanAge(it.Created, now) + "\t" + it.Note
			fmt.Fprintln(tw, row)
		}
		tw.Flush()
	}
	section("CONTAINERS", p.Containers, true, "NAME", "STATE", true)
	section("NETWORKS", p.Networks, true, "NAME", "NOTE", false)
	section("VOLUMES", volumeRows(p.Volumes), false, "NAME", "KIND", true)
	section("IMAGES", p.Images, true, "TAGS", "NOTE", true)
	writeNotes(w, p.Notes)
}

// volumeRows shortens anonymous volume names (64 hex characters).
func volumeRows(items []plan.Item) []plan.Item {
	out := make([]plan.Item, len(items))
	for i, it := range items {
		if len(it.Name) == 64 {
			it.Name = it.Name[:12] + "…"
		}
		out[i] = it
	}
	return out
}

func writeNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNotes:")
	for _, n := range notes {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

func summary(p plan.Plan) string {
	var parts []string
	add := func(n int, word string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, word)))
		}
	}
	add(len(p.Containers), "container")
	add(len(p.Networks), "network")
	add(len(p.Volumes), "volume")
	add(len(p.Images), "image")
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
