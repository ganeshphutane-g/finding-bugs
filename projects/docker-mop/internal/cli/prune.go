package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/plan"
)

const pruneUsage = `Usage: docker mop prune [system|containers|images|volumes|networks] [OPTIONS]

Show what would be removed, ask for confirmation, then remove exactly those
objects and nothing else. Anything that came into use or changed since the
list was made is skipped and reported; nothing is ever forced.

Options:
  -a, --all             images: include all unused images, not only dangling ones
                        volumes: include named volumes, not only anonymous ones
      --volumes         system: also include anonymous volumes
      --filter FILTER   Same filters as docker prune: until=24h, label=key[=value],
                        label!=key[=value] (repeatable)
  -y, --yes             Do not ask for confirmation (also -f, --force)
      --format FORMAT   table (default) or json (json requires --yes)
`

func runPrune(ctx context.Context, env Env, args []string) error {
	pf, help, err := parsePruneArgs("prune", args, true)
	if err != nil {
		return err
	}
	if help {
		fmt.Fprint(env.Stdout, pruneUsage)
		return nil
	}
	if pf.format == "json" && !pf.yes {
		return usagef("prune: --format json needs --yes, because it cannot ask for confirmation")
	}

	client, p, err := computePlan(ctx, env, pf)
	if err != nil {
		return err
	}

	if pf.format == "table" {
		writePlanTable(env.Stdout, p, time.Now())
		if p.Count() == 0 {
			return nil
		}
		if !pf.yes {
			ok, err := confirm(env.Stdin, env.Stdout, fmt.Sprintf("\nRemove these %d %s? [y/N] ", p.Count(), plural(p.Count(), "object")))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(env.Stdout, "Nothing was removed.")
				return nil
			}
		}
		fmt.Fprintln(env.Stdout)
	}

	res, err := plan.Apply(ctx, client, p)
	if pf.format == "json" {
		if jerr := writeResultJSON(env.Stdout, res); jerr != nil && err == nil {
			err = jerr
		}
		return err
	}
	writeResult(env.Stdout, res)
	if err != nil {
		return fmt.Errorf("stopped before finishing: %w", err)
	}
	return nil
}

// confirm asks a yes/no question. Anything but y or yes, including end of
// input, is a no.
func confirm(in io.Reader, out io.Writer, question string) (bool, error) {
	fmt.Fprint(out, question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	if err == io.EOF && line == "" {
		fmt.Fprintln(out)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

func writeResult(w io.Writer, res plan.Result) {
	var parts []string
	for _, typ := range []string{"container", "network", "volume", "image"} {
		if n := res.Removed(typ); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, typ)))
		}
	}
	if len(parts) == 0 {
		fmt.Fprintln(w, "Nothing was removed.")
	} else {
		fmt.Fprintf(w, "Removed %s. Freed %s.\n", strings.Join(parts, ", "), freedText(res))
	}

	skipped := res.Skipped()
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(w, "\nSkipped %d %s:\n", len(skipped), plural(len(skipped), "object"))
	for _, o := range skipped {
		name := o.Item.Name
		if name == "" || name == "<none>" {
			name = shortID(o.Item.ID)
		}
		fmt.Fprintf(w, "  - %s %s: %s\n", o.Type, name, o.Reason)
	}
}

// freedText reports freed space; image sizes are lower bounds because of
// shared layers.
func freedText(res plan.Result) string {
	s := humanSize(res.Freed())
	if res.Removed("image") > 0 {
		return "at least " + s
	}
	return s
}

func writeResultJSON(w io.Writer, res plan.Result) error {
	out := struct {
		Outcomes   []plan.Outcome `json:"outcomes"`
		Removed    int            `json:"removed"`
		Skipped    int            `json:"skipped"`
		FreedBytes int64          `json:"freed_bytes"`
	}{Outcomes: res.Outcomes, FreedBytes: res.Freed()}
	if out.Outcomes == nil {
		out.Outcomes = []plan.Outcome{}
	}
	for _, o := range res.Outcomes {
		if o.Done {
			out.Removed++
		} else {
			out.Skipped++
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
