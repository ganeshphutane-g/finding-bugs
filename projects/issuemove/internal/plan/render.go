package plan

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Render writes the plan for people.
func (p *Plan) Render(w io.Writer) {
	out := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	where := "on the same instance"
	if !p.SameInstance {
		where = "to another instance"
	}
	out("Move %d issue(s) from %s to %s (%s)", len(p.Issues), p.Source, p.Target, where)
	for _, ip := range p.Issues {
		is := ip.Issue
		comments := 0
		for _, c := range ip.Comments {
			if _, note := MoveNote(c, p.SourceUser); !note {
				comments++
			}
		}
		extra := []string{fmt.Sprintf("%d comment(s)", comments)}
		if ip.Attachments > 0 {
			extra = append(extra, fmt.Sprintf("%d attachment(s), %s", ip.Attachments, human(ip.Bytes)))
		}
		n := len(ip.Reactions)
		for _, r := range ip.CommentReac {
			n += len(r)
		}
		if n > 0 {
			extra = append(extra, fmt.Sprintf("%d reaction(s)", n))
		}
		var labels []string
		for _, l := range is.Labels {
			labels = append(labels, l.Name)
		}
		if len(labels) > 0 {
			extra = append(extra, "labels "+strings.Join(labels, ", "))
		}
		if is.Milestone != nil {
			extra = append(extra, "milestone "+is.Milestone.Title)
		}
		out("  #%-5d %-8s %s (%s)", is.Number, is.State, is.Title, strings.Join(extra, "; "))
	}
	if len(p.NewLabels) > 0 {
		var n []string
		for _, l := range p.NewLabels {
			n = append(n, l.Name)
		}
		out("Labels to create in %s: %s", p.Target, strings.Join(n, ", "))
	}
	if len(p.NewMilestones) > 0 {
		var n []string
		for _, m := range p.NewMilestones {
			n = append(n, m.Title)
		}
		out("Milestones to create in %s: %s", p.Target, strings.Join(n, ", "))
	}
	var as, attr []string
	for src, dst := range p.Authors {
		if dst != "" {
			as = append(as, src)
		} else {
			attr = append(attr, src)
		}
	}
	sort.Strings(as)
	sort.Strings(attr)
	if len(as) > 0 {
		out("Posted as their original authors: %s", strings.Join(as, ", "))
	}
	if len(attr) > 0 {
		out("Named in the text (posted by the token's user): %s", strings.Join(attr, ", "))
	}
	then := "closed"
	switch {
	case p.KeepOpen && p.Lock:
		then = "left open and locked"
	case p.KeepOpen:
		then = "left open"
	case p.Lock:
		then = "closed and locked"
	}
	out("Original dates are kept in the text; the originals get a link to the new issues and are %s.", then)
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		out("")
		out("%s:", title)
		for _, it := range items {
			out("  - %s", it)
		}
	}
	section("Blocked", p.Blockers)
	section("Warnings", p.Warnings)
	out("")
	if p.OK() {
		out("Nothing blocks the move. Nothing has been changed.")
	} else {
		out("%d problem(s) must be solved first. Nothing has been changed.", len(p.Blockers))
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
