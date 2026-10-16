package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Change is one field that differs.
type Change struct {
	Field    string
	From, To any
	Secret   bool // never shown
}

// Op is what apply does with an object.
type Op string

// The operations.
const (
	Create Op = "create"
	Update Op = "update"
	Delete Op = "delete"
	Keep   Op = "keep" // on the instance only, kept (no --prune)
)

// Action is what happens to one object.
type Action struct {
	Kind    Kind
	Op      Op
	Want    *Item // from the file (nil for delete and keep)
	Have    *Item // from the instance (nil for create)
	Changes []Change
}

// Label names the object.
func (a Action) Label() string {
	if a.Want != nil {
		return a.Want.Label
	}
	return a.Have.Label
}

// Plan is the difference between a file and an instance.
type Plan struct {
	Actions  []Action
	Problems []string // references that do not resolve: apply refuses
}

// InSync reports a plan with nothing to do.
func (p *Plan) InSync() bool {
	for _, a := range p.Actions {
		if a.Op != Keep {
			return false
		}
	}
	return true
}

// unordered fields compare as sets.
var unordered = map[string]map[string]bool{
	"monitors":    {"notifications": true, "tags": true},
	"maintenance": {"monitors": true, "status_pages": true, "weekdays": true, "daysOfMonth": true},
}

// Diff compares a file (want) with an instance (have, loaded from its
// export). With prune, objects only on the instance are deleted.
func Diff(want, have *Doc, prune bool) *Plan {
	p := &Plan{}
	for _, k := range Kinds {
		for _, w := range want.Items[k.Key] {
			h := have.Get(k.Key, w.Key)
			if h == nil {
				p.Actions = append(p.Actions, Action{Kind: k, Op: Create, Want: w})
				continue
			}
			if ch := compare(k.Key, w, h); len(ch) > 0 {
				p.Actions = append(p.Actions, Action{Kind: k, Op: Update, Want: w, Have: h, Changes: ch})
			}
		}
		for _, h := range have.Items[k.Key] {
			if want.Get(k.Key, h.Key) == nil {
				op := Keep
				if prune {
					op = Delete
				}
				p.Actions = append(p.Actions, Action{Kind: k, Op: op, Have: h})
			}
		}
	}
	p.Problems = references(want, have, prune)
	return p
}

// fieldsOf lists what to compare: the managed fields, the baseline, and
// whatever the file sets.
func fieldsOf(kind string, w, h *Item) []string {
	set := map[string]bool{}
	for _, f := range managed[kind] {
		set[f] = true
	}
	if kind == "monitors" {
		for f := range baseline {
			set[f] = true
		}
		for _, t := range []string{str(w.Fields["type"]), str(h.Fields["type"])} {
			for f := range typeBaseline[t] {
				set[f] = true
			}
		}
	}
	for f := range kindBaseline[kind] {
		set[f] = true
	}
	for f := range w.Fields {
		set[f] = true
	}
	var out []string
	relevant := maintenanceFields(str(w.Fields["strategy"]))
	for f := range set {
		if kind == "maintenance" && !relevant[f] {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// filled is a field's value, or its baseline when missing.
func filled(kind string, it *Item, typ, f string) any {
	if v, ok := it.Fields[f]; ok {
		return v
	}
	v, _ := kindDefault(kind, typ, f)
	return v
}

func compare(kind string, w, h *Item) []Change {
	typ := str(w.Fields["type"])
	var out []Change
	for _, f := range fieldsOf(kind, w, h) {
		if kind == "notifications" && f == "settings" {
			out = append(out, compareSettings(w, h)...)
			continue
		}
		wv := filled(kind, w, typ, f)
		hv := filled(kind, h, str(h.Fields["type"]), f)
		if !same(normalise(kind, f, wv), normalise(kind, f, hv)) {
			out = append(out, Change{Field: f, From: hv, To: wv, Secret: w.Secret[f] || h.Secret[f]})
		}
	}
	return out
}

// compareSettings compares notification settings one by one.
func compareSettings(w, h *Item) []Change {
	ws, _ := w.Fields["settings"].(map[string]any)
	hs, _ := h.Fields["settings"].(map[string]any)
	keys := map[string]bool{}
	for k := range ws {
		keys[k] = true
	}
	for k := range hs {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []Change
	for _, k := range names {
		if !same(ws[k], hs[k]) {
			out = append(out, Change{Field: "settings." + k, From: hs[k], To: ws[k], Secret: secretKey.MatchString(k) || w.Secret["settings."+k] || h.Secret["settings."+k]})
		}
	}
	return out
}

// normalise makes equal meanings compare equal: tags written as a name or
// {name, value}, sets in any order.
func normalise(kind, f string, v any) any {
	if kind == "monitors" && f == "tags" {
		list, _ := v.([]any)
		var out []any
		for _, t := range list {
			switch x := t.(type) {
			case string:
				out = append(out, map[string]any{"name": strings.ToLower(x), "value": ""})
			case map[string]any:
				out = append(out, map[string]any{"name": strings.ToLower(str(x["name"])), "value": x["value"]})
			}
		}
		v = out
	}
	if kind == "monitors" && (f == "group" || f == "notifications" || f == "proxy" || f == "docker_host" || f == "remote_browser") {
		v = lower(v)
	}
	if kind == "maintenance" && (f == "monitors" || f == "status_pages") {
		v = lower(v)
	}
	if kind == "monitors" && f == "url" {
		if s, ok := v.(string); ok && placeholderURL[s] {
			v = nil
		}
	}
	if kind == "maintenance" && f == "timeRange" {
		v = timeRange(v)
	}
	if kind == "status_pages" && f == "groups" {
		v = lowerGroupRefs(v)
	}
	if unordered[kind][f] {
		if list, ok := v.([]any); ok {
			sorted := append([]any(nil), list...)
			sort.SliceStable(sorted, func(i, j int) bool { return sortKey(sorted[i]) < sortKey(sorted[j]) })
			v = sorted
		}
	}
	return v
}

func lower(v any) any {
	switch x := v.(type) {
	case string:
		return strings.ToLower(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = lower(e)
		}
		return out
	}
	return v
}

// lowerGroupRefs compares status page monitors by their (case-insensitive)
// name, written as "X" or {monitor: X, show_url: true}.
func lowerGroupRefs(v any) any {
	groups, _ := v.([]any)
	var out []any
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		var mons []any
		list, _ := gm["monitors"].([]any)
		for _, m := range list {
			switch x := m.(type) {
			case string:
				mons = append(mons, map[string]any{"monitor": strings.ToLower(x), "show_url": false})
			case map[string]any:
				mons = append(mons, map[string]any{"monitor": strings.ToLower(str(x["monitor"])), "show_url": x["show_url"] == true})
			}
		}
		out = append(out, map[string]any{"name": gm["name"], "monitors": mons})
	}
	return out
}

func sortKey(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// references checks that every name the file refers to exists in the file
// or (unless pruned) on the instance.
func references(want, have *Doc, prune bool) []string {
	exists := func(kind, key string) bool {
		if want.Get(kind, strings.ToLower(key)) != nil {
			return true
		}
		return !prune && have.Get(kind, strings.ToLower(key)) != nil
	}
	var problems []string
	bad := func(it *Item, what, name string) {
		problems = append(problems, fmt.Sprintf("%s %s: %s %q does not exist", KindOf(it.Kind).One, it.Label, what, name))
	}
	for _, m := range want.Items["monitors"] {
		if g := str(m.Fields["group"]); g != "" {
			gi := want.Get("monitors", strings.ToLower(g))
			if gi == nil && !prune {
				gi = have.Get("monitors", strings.ToLower(g))
			}
			if gi == nil {
				bad(m, "group", g)
			} else if str(gi.Fields["type"]) != "group" {
				problems = append(problems, fmt.Sprintf("monitor %s: %q is not a group", m.Label, g))
			}
		}
		for _, n := range list(m.Fields["notifications"]) {
			if !exists("notifications", str(n)) {
				bad(m, "notification", str(n))
			}
		}
		for _, t := range list(m.Fields["tags"]) {
			name := str(t)
			if tm, ok := t.(map[string]any); ok {
				name = str(tm["name"])
			}
			if !exists("tags", name) {
				bad(m, "tag", name)
			}
		}
		for f, kind := range map[string]string{"proxy": "proxies", "docker_host": "docker_hosts", "remote_browser": "remote_browsers"} {
			if v := str(m.Fields[f]); v != "" && !exists(kind, v) {
				bad(m, strings.ReplaceAll(f, "_", " "), v)
			}
		}
	}
	for _, mt := range want.Items["maintenance"] {
		for _, x := range list(mt.Fields["monitors"]) {
			if !exists("monitors", str(x)) {
				bad(mt, "monitor", str(x))
			}
		}
		for _, x := range list(mt.Fields["status_pages"]) {
			if !exists("status_pages", str(x)) {
				bad(mt, "status page", str(x))
			}
		}
	}
	for _, sp := range want.Items["status_pages"] {
		for _, g := range list(sp.Fields["groups"]) {
			gm, _ := g.(map[string]any)
			for _, x := range list(gm["monitors"]) {
				name := str(x)
				if xm, ok := x.(map[string]any); ok {
					name = str(xm["monitor"])
				}
				if !exists("monitors", name) {
					bad(sp, "monitor", name)
				}
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}
