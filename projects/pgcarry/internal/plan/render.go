package plan

import (
	"fmt"
	"io"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/pg"
)

// Render writes the plan for people.
func (r *Report) Render(w io.Writer) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	p("Project %s, service %s (%s)", r.Project, r.Service, r.Image)
	where := r.MountName
	if r.MountType == "volume" {
		where = "volume " + where
	} else if r.MountType == "bind" {
		where = "folder " + where
	}
	if where != "" {
		p("Data: %s, mounted at %s", where, r.MountPoint)
	}
	if len(r.ComposeFiles) > 0 {
		p("Compose files (as the service was started): %s", strings.Join(r.ComposeFiles, ", "))
	}
	if c := r.Cluster; c != nil {
		p("Now: PostgreSQL %s, %s in %d database(s), data checksums %s", c.Version, human(c.TotalSizeBytes), userDatabases(c), onOff(c.DataChecksums))
		for _, d := range c.Databases {
			if d.IsTemplate {
				continue
			}
			loc := d.Collate
			if d.LocaleProvider != "libc" {
				loc = d.LocaleProvider + " " + d.Locale
			}
			var exts []string
			for _, e := range d.Extensions {
				if e.Name != "plpgsql" {
					exts = append(exts, e.Name+" "+e.Version)
				}
			}
			line := fmt.Sprintf("  %-20s %9s  %s, %s", d.Name, human(d.SizeBytes), d.Encoding, loc)
			if len(exts) > 0 {
				line += ", extensions: " + strings.Join(exts, ", ")
			}
			p("%s", line)
		}
	}
	if r.Target != nil {
		p("New: %s (PostgreSQL %d), data at %s", r.TargetImage, r.Target.Major(), r.TargetData)
	} else if r.TargetImage != "" {
		p("New: %s", r.TargetImage)
	}
	if r.FreeBytes > 0 {
		p("Free space next to the data: %s", human(r.FreeBytes))
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		p("")
		p("%s:", title)
		for _, it := range items {
			p("  - %s", it)
		}
	}
	section("Blocked", r.Blockers)
	section("Warnings", r.Warnings)
	section("What changes", r.Changes)
	if r.EditDiff != "" {
		p("")
		p("The Compose file change for the switch:")
		fmt.Fprint(w, indent(r.EditDiff, "  "))
	}
	if r.SwitchProblem != "" {
		p("")
		p("The switch needs a manual Compose edit: %s", r.SwitchProblem)
	}
	p("")
	if r.OK() {
		p("Nothing blocks the upgrade. Nothing has been changed.")
	} else {
		p("%d problem(s) must be solved before upgrading. Nothing has been changed.", len(r.Blockers))
	}
}

func userDatabases(c *pg.Cluster) int {
	n := 0
	for _, d := range c.Databases {
		if !d.IsTemplate {
			n++
		}
	}
	return n
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
