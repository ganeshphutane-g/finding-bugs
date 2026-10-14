// Package edit changes a Compose file in place for the switch to the new
// data: the service's image, its data mount, and a new top-level volume.
// Only those characters change; comments, order and quoting stay as they
// were. When an edit cannot be made with certainty (a value from a
// variable, a multi-line value, flow-style mappings), it says so and the
// user edits by hand.
package edit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Request describes the switch.
type Request struct {
	Files       []string // Compose files in the order Compose reads them
	Service     string
	Image       string // new image
	MountTarget string // the data mount's current target
	NewTarget   string // its new target
	Kind        string // "volume" or "bind"
	VolumeKey   string // volume: the top-level key now used
	NewKey      string // volume: the new top-level key
	NewName     string // volume: the new volume's Docker name
	BindSuffix  string // bind: appended to the folder, e.g. "-pg18"
}

// File is one changed file.
type File struct {
	Path string
	Old  []byte
	New  []byte
}

// ErrManual means the file must be edited by hand; the message says how.
var ErrManual = errors.New("edit by hand")

func manual(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrManual, fmt.Sprintf(f, a...))
}

type doc struct {
	path  string
	data  []byte
	lines []string
	root  *yaml.Node
	reps  []replacement
	ins   []insertion
}

type replacement struct {
	line, start, end int // 1-based line; rune offsets in the line
	text             string
}

type insertion struct {
	before int // 1-based line
	text   string
}

func load(path string) (*doc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	d := &doc{path: path, data: data, lines: strings.Split(string(data), "\n"), root: &root}
	return d, nil
}

// get follows mapping keys from the document root.
func get(n *yaml.Node, keys ...string) (key, val *yaml.Node) {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	for _, k := range keys {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil, nil
		}
		var foundKey, found *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				foundKey, found = n.Content[i], n.Content[i+1]
			}
		}
		if foundKey == nil {
			return nil, nil
		}
		key, n = foundKey, found
	}
	return key, n
}

// Prepare works out the edits. It returns the changed files, or ErrManual.
func Prepare(req Request) ([]File, error) {
	var docs []*doc
	for _, f := range req.Files {
		d, err := load(f)
		if err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	svcDoc := func(field string) (*doc, *yaml.Node) {
		var d *doc
		var n *yaml.Node
		for _, x := range docs {
			if _, v := get(x.root, "services", req.Service, field); v != nil {
				d, n = x, v
			}
		}
		return d, n
	}
	for _, x := range docs {
		if _, v := get(x.root, "services", req.Service, "extends"); v != nil {
			return nil, manual("service %s uses extends", req.Service)
		}
	}

	// The image.
	d, img := svcDoc("image")
	if img == nil {
		return nil, manual("no image: line for service %s was found", req.Service)
	}
	if err := d.replaceScalar(img, req.Image); err != nil {
		return nil, err
	}

	// The data mount: the last file listing a mount with that target wins.
	var md *doc
	var entry *yaml.Node
	for _, x := range docs {
		_, vols := get(x.root, "services", req.Service, "volumes")
		if vols == nil || vols.Kind != yaml.SequenceNode {
			continue
		}
		for _, e := range vols.Content {
			if mountTarget(e) == strings.TrimSuffix(req.MountTarget, "/") {
				md, entry = x, e
			}
		}
	}
	if entry == nil {
		return nil, manual("the mount at %s was not found in the files", req.MountTarget)
	}
	switch entry.Kind {
	case yaml.ScalarNode:
		parts := strings.Split(entry.Value, ":")
		src := req.NewKey
		if req.Kind == "bind" {
			src = strings.TrimSuffix(parts[0], "/") + req.BindSuffix
		}
		val := src + ":" + req.NewTarget
		if len(parts) > 2 {
			val += ":" + strings.Join(parts[2:], ":")
		}
		if err := md.replaceScalar(entry, val); err != nil {
			return nil, err
		}
	case yaml.MappingNode:
		_, s := get(entry, "source")
		_, t := get(entry, "target")
		if s == nil || t == nil {
			return nil, manual("the data mount has no source or target")
		}
		src := req.NewKey
		if req.Kind == "bind" {
			src = strings.TrimSuffix(s.Value, "/") + req.BindSuffix
		}
		if err := md.replaceScalar(s, src); err != nil {
			return nil, err
		}
		if err := md.replaceScalar(t, req.NewTarget); err != nil {
			return nil, err
		}
	}

	// The new top-level volume, next to the old one.
	if req.Kind == "volume" {
		var vd *doc
		var key, val *yaml.Node
		for _, x := range docs {
			if k, v := get(x.root, "volumes", req.VolumeKey); k != nil {
				vd, key, val = x, k, v
			}
		}
		if key == nil {
			return nil, manual("the volume %s is not declared under volumes:", req.VolumeKey)
		}
		_, parent := get(vd.root, "volumes")
		if parent.Style&yaml.FlowStyle != 0 {
			return nil, manual("the volumes: section is written in flow style")
		}
		for _, x := range docs {
			if k, _ := get(x.root, "volumes", req.NewKey); k != nil {
				return nil, manual("a volume %s is already declared", req.NewKey)
			}
		}
		text, err := newVolume(req, key, val)
		if err != nil {
			return nil, err
		}
		vd.ins = append(vd.ins, insertion{before: key.Line, text: text})
	}

	var out []File
	for _, x := range docs {
		if len(x.reps) == 0 && len(x.ins) == 0 {
			continue
		}
		out = append(out, File{Path: x.path, Old: x.data, New: x.apply()})
	}
	return out, nil
}

// mountTarget is a service volume entry's target.
func mountTarget(e *yaml.Node) string {
	switch e.Kind {
	case yaml.ScalarNode:
		parts := strings.Split(e.Value, ":")
		if len(parts) >= 2 {
			return strings.TrimSuffix(parts[1], "/")
		}
	case yaml.MappingNode:
		if _, t := get(e, "target"); t != nil {
			return strings.TrimSuffix(t.Value, "/")
		}
	}
	return ""
}

// newVolume writes the new key with the old key's options (a new name when
// the old one sets one).
func newVolume(req Request, key, val *yaml.Node) (string, error) {
	indent := strings.Repeat(" ", key.Column-1)
	if val == nil || val.Kind == yaml.ScalarNode && (val.Tag == "!!null" || val.Value == "") {
		return indent + req.NewKey + ":\n", nil
	}
	if val.Kind != yaml.MappingNode || val.Style&yaml.FlowStyle != 0 {
		if val.Kind == yaml.MappingNode && len(val.Content) == 0 {
			return indent + req.NewKey + ": {}\n", nil
		}
		return "", manual("the options of volume %s are written in flow style", req.VolumeKey)
	}
	var m yaml.Node
	m.Kind, m.Tag = yaml.MappingNode, "!!map"
	hasName := false
	external := false
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		if strings.Contains(v.Value, "$") {
			return "", manual("the options of volume %s use variables", req.VolumeKey)
		}
		nk := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k.Value}
		nv := cloneValue(v)
		switch k.Value {
		case "name":
			nv = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: req.NewName}
			hasName = true
		case "external":
			external = v.Value == "true"
		}
		m.Content = append(m.Content, nk, nv)
	}
	if external && !hasName {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: req.NewName})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&m); err != nil {
		return "", err
	}
	child := indent + "  "
	if len(val.Content) > 0 {
		child = strings.Repeat(" ", val.Content[0].Column-1)
	}
	var b strings.Builder
	b.WriteString(indent + req.NewKey + ":\n")
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		b.WriteString(child + line + "\n")
	}
	return b.String(), nil
}

func cloneValue(v *yaml.Node) *yaml.Node {
	c := *v
	c.Line, c.Column, c.HeadComment, c.LineComment, c.FootComment = 0, 0, "", "", ""
	c.Content = nil
	for _, x := range v.Content {
		c.Content = append(c.Content, cloneValue(x))
	}
	return &c
}

// replaceScalar replaces a scalar's text where it is written, keeping its
// quoting.
func (d *doc) replaceScalar(n *yaml.Node, value string) error {
	if n.Kind != yaml.ScalarNode {
		return manual("%s line %d: expected a single value", d.path, n.Line)
	}
	if strings.Contains(n.Value, "$") {
		return manual("%s line %d: %q comes from a variable; set it to %q yourself", d.path, n.Line, n.Value, value)
	}
	if n.Line < 1 || n.Line > len(d.lines) {
		return manual("%s: position of %q unknown", d.path, n.Value)
	}
	line := []rune(d.lines[n.Line-1])
	start := n.Column - 1
	if start < 0 || start >= len(line) {
		return manual("%s line %d: position of %q unknown", d.path, n.Line, n.Value)
	}
	end, raw, ok := scalarSpan(line, start, n.Style)
	if !ok || raw != n.Value {
		return manual("%s line %d: %q is written in a form pgcarry does not edit; set it to %q yourself", d.path, n.Line, n.Value, value)
	}
	d.reps = append(d.reps, replacement{line: n.Line, start: start, end: end, text: render(value, n.Style, inFlow(line, start))})
	return nil
}

// inFlow guesses whether position start is inside a [ ] or { } collection.
func inFlow(line []rune, start int) bool {
	depth := 0
	for _, c := range line[:start] {
		switch c {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth > 0
}

// scalarSpan finds where the scalar starting at start ends, and its value.
func scalarSpan(line []rune, start int, style yaml.Style) (int, string, bool) {
	switch {
	case style&yaml.SingleQuotedStyle != 0:
		var b strings.Builder
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					b.WriteRune('\'')
					i++
					continue
				}
				return i + 1, b.String(), true
			}
			b.WriteRune(line[i])
		}
		return 0, "", false
	case style&yaml.DoubleQuotedStyle != 0:
		var b strings.Builder
		for i := start + 1; i < len(line); i++ {
			switch line[i] {
			case '\\':
				if i+1 >= len(line) {
					return 0, "", false
				}
				i++
				switch line[i] {
				case '"', '\\', '/':
					b.WriteRune(line[i])
				default:
					return 0, "", false // other escapes: leave to the user
				}
			case '"':
				return i + 1, b.String(), true
			default:
				b.WriteRune(line[i])
			}
		}
		return 0, "", false
	case style == 0 || style&yaml.FlowStyle != 0:
		flow := inFlow(line, start)
		end := len(line)
		for i := start; i < len(line); i++ {
			if line[i] == '#' && i > start && (line[i-1] == ' ' || line[i-1] == '\t') {
				end = i
				break
			}
			if flow && (line[i] == ',' || line[i] == ']' || line[i] == '}') {
				end = i
				break
			}
		}
		for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r') {
			end--
		}
		return end, string(line[start:end]), true
	}
	return 0, "", false
}

// render writes value in the original quoting style, or plain when safe.
func render(value string, style yaml.Style, flow bool) string {
	switch {
	case style&yaml.SingleQuotedStyle != 0:
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	case style&yaml.DoubleQuotedStyle != 0 || !plainSafe(value, flow):
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
	}
	return value
}

func plainSafe(v string, flow bool) bool {
	if v == "" || !utf8.ValidString(v) || strings.ContainsAny(v[:1], "!&*-?|>'\"%@`{[#,") ||
		strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.HasSuffix(v, ":") || strings.HasSuffix(v, " ") {
		return false
	}
	return !flow || !strings.ContainsAny(v, ",[]{}")
}

// apply returns the document with its edits made.
func (d *doc) apply() []byte {
	lines := append([]string{}, d.lines...)
	// Replacements, right to left within a line.
	byLine := map[int][]replacement{}
	for _, r := range d.reps {
		byLine[r.line] = append(byLine[r.line], r)
	}
	for ln, reps := range byLine {
		runes := []rune(lines[ln-1])
		for i := 0; i < len(reps); i++ {
			for j := i + 1; j < len(reps); j++ {
				if reps[j].start > reps[i].start {
					reps[i], reps[j] = reps[j], reps[i]
				}
			}
		}
		for _, r := range reps {
			runes = append(runes[:r.start], append([]rune(r.text), runes[r.end:]...)...)
		}
		lines[ln-1] = string(runes)
	}
	// Insertions, bottom up so line numbers stay valid.
	for i := 0; i < len(d.ins); i++ {
		for j := i + 1; j < len(d.ins); j++ {
			if d.ins[j].before > d.ins[i].before {
				d.ins[i], d.ins[j] = d.ins[j], d.ins[i]
			}
		}
	}
	crlf := strings.HasSuffix(strings.SplitN(string(d.data), "\n", 2)[0], "\r")
	for _, in := range d.ins {
		text := strings.TrimSuffix(in.text, "\n")
		if crlf {
			text = strings.ReplaceAll(text, "\n", "\r\n") + "\r"
		}
		at := in.before - 1
		lines = append(lines[:at], append([]string{text}, lines[at:]...)...)
	}
	return []byte(strings.Join(lines, "\n"))
}

// Diff shows the changed lines of a file, with a little context.
func Diff(f File) string {
	a := strings.Split(string(f.Old), "\n")
	b := strings.Split(string(f.New), "\n")
	// Longest common subsequence; Compose files are small.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s (after)\n", f.Path, f.Path)
	last := -10
	for k, o := range ops {
		near := false
		for d := -2; d <= 2; d++ {
			if k+d >= 0 && k+d < len(ops) && ops[k+d].kind != ' ' {
				near = true
			}
		}
		if !near {
			continue
		}
		if k > last+1 {
			out.WriteString("@@\n")
		}
		last = k
		fmt.Fprintf(&out, "%c%s\n", o.kind, strings.TrimSuffix(o.text, "\r"))
	}
	return out.String()
}
