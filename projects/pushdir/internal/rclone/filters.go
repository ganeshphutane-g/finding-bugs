package rclone

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// rclone refuses --files-from-raw (which pushdir uses to send exact batches)
// together with any filter flag. So the user's flags are split: filter flags
// only go to full copies and to Select, which turns the rule flags into one
// --filter-from file in the same order rclone itself applies them.

// ruleFlags are rclone's path filter flags, by the order rclone applies them.
var ruleFlags = []string{"include", "include-from", "exclude", "exclude-from", "filter", "filter-from"}

// selFlags are the other filter flags (they take a value); they combine with
// --filter-from but not with --files-from-raw.
var selFlags = map[string]bool{
	"exclude-if-present":    true,
	"min-age":               true,
	"max-age":               true,
	"min-size":              true,
	"max-size":              true,
	"hash-filter":           true,
	"metadata-filter":       true,
	"metadata-filter-from":  true,
	"metadata-exclude":      true,
	"metadata-exclude-from": true,
	"metadata-include":      true,
	"metadata-include-from": true,
}

// refused are flags that conflict with how pushdir chooses what to send.
var refused = map[string]string{
	"files-from":      "pushdir chooses which files to send itself",
	"files-from-raw":  "pushdir chooses which files to send itself",
	"files-from0":     "pushdir chooses which files to send itself",
	"delete-excluded": "it would delete remote files outside --max-delete's count",
}

// flagSet is the user's extra flags, split up.
type flagSet struct {
	plain []string            // not filters: go to every command
	sel   []string            // other filter flags, as --name=value
	rules map[string][]string // rule flag values by flag name
	n     int                 // number of rule flags
	err   error
}

func (f *flagSet) filtered() bool { return f.n > 0 || len(f.sel) > 0 }

// listing is plain without progress output, which would mix into listings.
func (f *flagSet) listing() []string {
	return dropProgress(f.plain)
}

func dropProgress(flags []string) []string {
	var out []string
	for _, a := range flags {
		if a == "-P" || a == "--progress" || strings.HasPrefix(a, "--progress=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// split returns the user's flags, split up (once).
func (r *Runner) split() *flagSet {
	r.once.Do(func() { r.flags = splitFlags(r.Extra) })
	return &r.flags
}

// Check reports flags pushdir cannot work with, and filter settings in the
// environment (RCLONE_EXCLUDE and the like), which would break the exact
// batches.
func (r *Runner) Check() error {
	fl := r.split()
	if fl.err != nil {
		return fl.err
	}
	for _, name := range append(append([]string{}, ruleFlags...), keys(selFlags)...) {
		env := "RCLONE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if os.Getenv(env) != "" {
			return fmt.Errorf("%s is set in the environment; pass the filter after -- instead (as --%s)", env, name)
		}
	}
	for name := range refused {
		env := "RCLONE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		if os.Getenv(env) != "" {
			return fmt.Errorf("%s is set in the environment; pushdir cannot use --%s: %s", env, name, refused[name])
		}
	}
	// Read the rule files now, so a missing one is reported at start.
	_, err := fl.ruleFile(nil)
	return err
}

func keys(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}

func splitFlags(args []string) flagSet {
	f := flagSet{rules: map[string][]string{}}
	isRule := map[string]bool{}
	for _, n := range ruleFlags {
		isRule[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var name, value string
		hasValue := false
		switch {
		case strings.HasPrefix(a, "--"):
			name, value, hasValue = strings.Cut(a[2:], "=")
			name = strings.ReplaceAll(name, "_", "-")
		case a == "-f" || strings.HasPrefix(a, "-f="):
			name = "filter"
			_, value, hasValue = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-f") && len(a) > 2:
			name, value, hasValue = "filter", a[2:], true
		default:
			f.plain = append(f.plain, a)
			continue
		}
		if why, ok := refused[name]; ok {
			f.err = fmt.Errorf("--%s cannot be used with pushdir: %s", name, why)
			return f
		}
		needsValue := isRule[name] || selFlags[name]
		if needsValue && !hasValue {
			if i+1 >= len(args) {
				f.err = fmt.Errorf("--%s needs a value", name)
				return f
			}
			i++
			value = args[i]
		}
		switch {
		case isRule[name]:
			if strings.HasSuffix(name, "-from") && value == "-" {
				f.err = fmt.Errorf("--%s - (standard input) cannot be used with pushdir; give a file", name)
				return f
			}
			f.rules[name] = append(f.rules[name], value)
			f.n++
		case selFlags[name]:
			f.sel = append(f.sel, "--"+name+"="+value)
		case name == "ignore-case":
			f.sel = append(f.sel, a)
		default:
			f.plain = append(f.plain, a)
		}
	}
	return f
}

// fileRules reads a rule file the way rclone does: lines are trimmed, and
// empty lines and lines starting with # or ; are skipped.
func fileRules(path string) ([]string, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var out []string
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// rules builds a --filter-from file equal to the user's filter flags,
// followed by rules that include just the given paths. rclone applies
// --include, --include-from, --exclude, --exclude-from, --filter and
// --filter-from in that order, then excludes everything else if any include
// rule was given; the user's rules keep the first say, so a batch file is
// selected exactly when rclone would select it.
func (f *flagSet) ruleFile(paths []string) (string, error) {
	var b strings.Builder
	add := func(prefix string, rules []string) {
		for _, r := range rules {
			b.WriteString(prefix + r + "\n")
		}
	}
	addFiles := func(prefix string, files []string) error {
		for _, file := range files {
			rules, err := fileRules(file)
			if err != nil {
				return fmt.Errorf("reading filter file: %w", err)
			}
			add(prefix, rules)
		}
		return nil
	}
	add("+ ", f.rules["include"])
	if err := addFiles("+ ", f.rules["include-from"]); err != nil {
		return "", err
	}
	add("- ", f.rules["exclude"])
	if err := addFiles("- ", f.rules["exclude-from"]); err != nil {
		return "", err
	}
	add("", f.rules["filter"])
	if err := addFiles("", f.rules["filter-from"]); err != nil {
		return "", err
	}
	if len(f.rules["include"])+len(f.rules["include-from"]) > 0 {
		b.WriteString("- /**\n")
	}
	for _, p := range paths {
		b.WriteString("+ /" + globEscape(p) + "\n")
	}
	b.WriteString("- **\n")
	return b.String(), nil
}

// globEscape makes a path match only itself as an rclone glob.
func globEscape(p string) string {
	var b strings.Builder
	runes := []rune(p)
	// rclone trims rule lines, so trailing spaces go in brackets.
	end := len(runes)
	for end > 0 && unicode.IsSpace(runes[end-1]) {
		end--
	}
	for i, c := range runes {
		switch {
		case i >= end:
			b.WriteString("[" + string(c) + "]")
		case strings.ContainsRune(`\*?[]{}`, c):
			b.WriteString(`\` + string(c))
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Filtered reports whether the user gave any filter flags.
func (r *Runner) Filtered() bool { return r.split().filtered() }
