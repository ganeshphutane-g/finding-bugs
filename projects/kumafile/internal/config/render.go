package config

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Render writes the plan for people. Secrets are never shown.
func (p *Plan) Render(w io.Writer) {
	counts := map[Op]int{}
	var kept []string
	for _, a := range p.Actions {
		counts[a.Op]++
		switch a.Op {
		case Create:
			extra := ""
			if t := str(a.Want.Fields["type"]); t != "" {
				extra = " (" + t + ")"
			}
			fmt.Fprintf(w, "  + %s %s%s\n", a.Kind.One, clean(a.Label()), clean(extra))
		case Update:
			fmt.Fprintf(w, "  ~ %s %s\n", a.Kind.One, clean(a.Label()))
			for _, c := range a.Changes {
				if c.Secret {
					fmt.Fprintf(w, "      %s: (secret, changed)\n", clean(c.Field))
				} else {
					fmt.Fprintf(w, "      %s: %s -> %s\n", clean(c.Field), show(c.From), show(c.To))
				}
			}
		case Delete:
			fmt.Fprintf(w, "  - %s %s\n", a.Kind.One, clean(a.Label()))
		case Keep:
			kept = append(kept, a.Kind.One+" "+clean(a.Label()))
		}
	}
	if len(kept) > 0 {
		sort.Strings(kept)
		fmt.Fprintf(w, "  Only on the instance, kept (--prune deletes them): %s\n", strings.Join(kept, ", "))
	}
	for _, pr := range p.Problems {
		fmt.Fprintf(w, "  ! %s\n", clean(pr))
	}
	if p.InSync() {
		fmt.Fprintln(w, "The instance matches the file.")
		return
	}
	fmt.Fprintf(w, "%d to create, %d to change, %d to delete.\n", counts[Create], counts[Update], counts[Delete])
}

// clean makes names and values safe to print (no control characters).
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// show writes a value briefly.
func show(v any) string {
	return clean(show1(v))
}

func show1(v any) string {
	if isEmpty(v) {
		return "(none)"
	}
	if s, ok := v.(string); ok {
		if s == strings.TrimSpace(s) && !strings.ContainsAny(s, "\n\"") && len(s) <= 80 {
			return s
		}
		b, _ := json.Marshal(s)
		if len(b) > 82 {
			return string(b[:79]) + `..."`
		}
		return string(b)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}
