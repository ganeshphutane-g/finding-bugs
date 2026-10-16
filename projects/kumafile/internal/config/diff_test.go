package config

import (
	"bytes"
	"strings"
	"testing"
)

func load(t *testing.T, s string, vars map[string]string) *Doc {
	t.Helper()
	d, err := Load([]byte(s), func(n string) (string, bool) { v, ok := vars[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLoadProblems(t *testing.T) {
	for in, want := range map[string]string{
		"monitorz: []": `unknown section "monitorz"`,
		"version: 2":   "version 2 is not supported",
		"monitors:\n  - name: a\n    type: http\n  - name: A\n    type: ping": "appears twice",
		"monitors:\n  - name: a":                     `monitor "a": type is missing`,
		"monitors:\n  - name: a / b\n    type: http": "names cannot contain",
		"tags:\n  - name: x\n    color: ${COLOR}":    "no value for COLOR",
		"tags: {name: x}":                            "expected a list",
	} {
		_, err := Load([]byte(in), func(string) (string, bool) { return "", false })
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", in, err, want)
		}
	}
	d := load(t, "tags:\n  - name: x\n    color: $${not} ${C}", map[string]string{"C": "red"})
	if got := d.Items["tags"][0].Fields["color"]; got != "${not} red" || !d.Items["tags"][0].Secret["color"] {
		t.Fatalf("expansion: %q %v", got, d.Items["tags"][0].Secret)
	}
}

const live = `
tags:
  - {name: prod, color: "#111"}
  - {name: old, color: "#222"}
notifications:
  - name: Chat
    type: discord
    settings: {discordWebhookUrl: "https://x/hook", discordUsername: kuma}
monitors:
  - {name: Web, type: group}
  - name: API
    group: Web
    type: http
    url: https://api
    interval: 120
    notifications: [Chat]
    tags: [prod, {name: old, value: x}]
  - {name: Gone, type: ping, hostname: h}
`

func TestDiff(t *testing.T) {
	have := load(t, live, nil)
	want := load(t, `
tags:
  - {name: prod, color: "#111"}
notifications:
  - name: Chat
    type: discord
    settings: {discordWebhookUrl: "${HOOK}", discordUsername: kuma}
monitors:
  - {name: Web, type: group}
  - name: API
    group: WEB
    type: http
    url: https://api
    notifications: [chat]
    tags: [prod]
  - {name: New, type: ping, hostname: n}
`, map[string]string{"HOOK": "https://x/other"})
	p := Diff(want, have, false)
	var buf bytes.Buffer
	p.Render(&buf)
	out := buf.String()
	for _, w := range []string{
		"~ notification Chat\n      settings.discordWebhookUrl: (secret, changed)",
		"~ monitor WEB / API\n      interval: 120 -> 60\n      tags:",
		"+ monitor New (ping)",
		"Only on the instance, kept (--prune deletes them): monitor Gone, tag old",
		"1 to create, 2 to change, 0 to delete.",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "https://x/other") || strings.Contains(out, "group:") || len(p.Problems) != 0 {
		t.Errorf("a secret, a case-only change or a problem was reported:\n%s", out)
	}
	// With --prune, the tag the API monitor still has on the instance goes,
	// and what refers to removed things is a problem.
	p = Diff(load(t, "monitors:\n  - {name: A, type: http, notifications: [Chat], tags: [nope]}", nil), have, true)
	if len(p.Problems) != 2 || !strings.Contains(strings.Join(p.Problems, "\n"), `notification "Chat" does not exist`) {
		t.Fatalf("problems %q", p.Problems)
	}
	deletes := 0
	for _, a := range p.Actions {
		if a.Op == Delete {
			deletes++
		}
	}
	if deletes != 6 {
		t.Fatalf("%d deletes", deletes)
	}
	renamed := load(t, strings.Replace(live, "name: prod,", "name: Prod,", 1), nil)
	if p := Diff(renamed, have, false); p.InSync() || p.Actions[0].Changes[0].Field != "name" {
		t.Fatalf("a change of case is a rename: %+v", p.Actions)
	}
	if Diff(have, have, true).InSync() != true {
		t.Fatal("a file differs from itself")
	}
}

func TestSame(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{nil, "", true}, {[]any{}, map[string]any{}, true}, {num(161), "161", true}, {1, true, true},
		{0, false, true}, {2, true, false}, {"a", "A", false}, {map[string]any{"x": nil}, map[string]any{}, true},
		{[]any{"a", "b"}, []any{"b", "a"}, false}, {num(60), 60, true}, {nil, false, false},
	} {
		if got := same(c.a, c.b); got != c.want {
			t.Errorf("same(%#v, %#v) = %v", c.a, c.b, got)
		}
	}
}
