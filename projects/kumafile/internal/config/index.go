package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

// PathSep separates group names in a monitor's place: "Web / API".
const PathSep = " / "

// index finds objects by id and names them for the file.
type index struct {
	st       *kuma.State
	monitors map[int64]kuma.Object
	paths    map[int64][]string // monitor id -> group names then its name
	warnings []string
}

// probeName is the paused monitor apply makes for a moment (see apply).
const probeName = "kumafile probe (temporary)"

func newIndex(st *kuma.State) *index {
	ix := &index{st: st, monitors: map[int64]kuma.Object{}, paths: map[int64][]string{}}
	var ms []kuma.Object
	for _, m := range st.Monitors {
		if m.Str("name") == probeName && m["active"] != true && kuma.Int(m["parent"]) == 0 {
			continue // left behind by an interrupted apply; not configuration
		}
		ms = append(ms, m)
	}
	cp := *st // the state itself is left as it is
	cp.Monitors = ms
	ix.st = &cp
	for _, m := range ms {
		ix.monitors[m.ID()] = m
	}
	for id := range ix.monitors {
		ix.paths[id] = ix.path(id, map[int64]bool{})
	}
	return ix
}

// path follows parents up to the top (guarding against loops).
func (ix *index) path(id int64, seen map[int64]bool) []string {
	m, ok := ix.monitors[id]
	if !ok || seen[id] {
		return nil
	}
	seen[id] = true
	parent := kuma.Int(m["parent"])
	var up []string
	if parent != 0 {
		up = ix.path(parent, seen)
	}
	return append(up, m.Str("name"))
}

// check refuses states the file cannot describe without guessing.
func (ix *index) check() error {
	var problems []string
	seen := map[string]int64{}
	for _, m := range ix.st.Monitors {
		p := ix.paths[m.ID()]
		for _, part := range p {
			if strings.Contains(part, PathSep) {
				problems = append(problems, fmt.Sprintf("monitor %q: the name contains %q, which separates groups in the file; rename it", part, PathSep))
			}
		}
		key := strings.ToLower(strings.Join(p, PathSep))
		if other, dup := seen[key]; dup {
			problems = append(problems, fmt.Sprintf("monitors %d and %d are both %q; give them different names", other, m.ID(), strings.Join(p, PathSep)))
		}
		seen[key] = m.ID()
	}
	dups := func(kind string, os []kuma.Object, field string) {
		seen := map[string]bool{}
		for _, o := range os {
			k := strings.ToLower(o.Str(field))
			if seen[k] {
				problems = append(problems, fmt.Sprintf("two %s are called %q; give them different names", kind, o.Str(field)))
			}
			seen[k] = true
		}
	}
	dups("notifications", ix.st.Notifications, "name")
	dups("tags", ix.st.Tags, "name")
	dups("Docker hosts", ix.st.DockerHosts, "name")
	dups("remote browsers", ix.st.RemoteBrowsers, "name")
	var mts []kuma.Object
	for _, mt := range ix.st.Maintenance {
		mts = append(mts, mt.Object)
	}
	dups("maintenance windows", mts, "title")
	proxies := map[string]bool{}
	for _, p := range ix.st.Proxies {
		k := proxyKey(p)
		if proxies[k] {
			problems = append(problems, fmt.Sprintf("two proxies are %s", k))
		}
		proxies[k] = true
	}
	if len(problems) > 0 {
		return fmt.Errorf("the instance cannot be written to a file as it is:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func proxyKey(p kuma.Object) string {
	return fmt.Sprintf("%s://%s:%d", p.Str("protocol"), p.Str("host"), kuma.Int(p["port"]))
}

// monitorsInOrder lists groups before what they contain, otherwise by id.
func (ix *index) monitorsInOrder() []kuma.Object {
	ms := append([]kuma.Object(nil), ix.st.Monitors...)
	sort.SliceStable(ms, func(i, j int) bool {
		pi, pj := ix.paths[ms[i].ID()], ix.paths[ms[j].ID()]
		if len(pi) != len(pj) {
			return len(pi) < len(pj)
		}
		return ms[i].ID() < ms[j].ID()
	})
	return ms
}

func (ix *index) groupPath(m kuma.Object) string {
	p := ix.paths[m.ID()]
	if len(p) <= 1 {
		return ""
	}
	return strings.Join(p[:len(p)-1], PathSep)
}

// monitorRef names a monitor by its place: "Web / Homepage".
func (ix *index) monitorRef(id int64) string {
	if p, ok := ix.paths[id]; ok {
		return escape(strings.Join(p, PathSep))
	}
	ix.warnings = append(ix.warnings, fmt.Sprintf("a reference to monitor %d, which does not exist, was left out", id))
	return "#" + strconv.FormatInt(id, 10)
}

func (ix *index) pageSlug(id int64) string {
	for _, p := range ix.st.StatusPages {
		if p.ID() == id {
			return p.Str("slug")
		}
	}
	return "#" + strconv.FormatInt(id, 10)
}

func (ix *index) notificationNames(m kuma.Object) []any {
	on, _ := m["notificationIDList"].(map[string]any)
	var out []any
	for _, n := range ix.st.Notifications {
		if v, ok := on[strconv.FormatInt(n.ID(), 10)]; ok && v == true {
			out = append(out, escape(n.Str("name")))
		}
	}
	return out
}

func (ix *index) tagRefs(m kuma.Object) []any {
	ts, _ := m["tags"].([]any)
	var out []any
	for _, t := range ts {
		to, _ := t.(map[string]any)
		name, _ := to["name"].(string)
		val, _ := to["value"].(string)
		if val == "" {
			out = append(out, escape(name))
		} else {
			out = append(out, Obj{{"name", escape(name)}, {"value", escape(val)}})
		}
	}
	return out
}

func (ix *index) proxyRef(v any) string {
	id := kuma.Int(v)
	for _, p := range ix.st.Proxies {
		if id != 0 && p.ID() == id {
			return proxyKey(p)
		}
	}
	return ""
}

func (ix *index) dockerHostName(v any) string {
	id := kuma.Int(v)
	for _, h := range ix.st.DockerHosts {
		if id != 0 && h.ID() == id {
			return escape(h.Str("name"))
		}
	}
	return ""
}

func (ix *index) browserName(v any) string {
	id := kuma.Int(v)
	for _, b := range ix.st.RemoteBrowsers {
		if id != 0 && b.ID() == id {
			return escape(b.Str("name"))
		}
	}
	return ""
}

// MonitorKeys maps each monitor's id to its identity in a file (its place,
// lower case) for a state.
func MonitorKeys(st *kuma.State) map[int64]string {
	ix := newIndex(st)
	out := map[int64]string{}
	for id, p := range ix.paths {
		out[id] = strings.ToLower(strings.Join(p, PathSep))
	}
	return out
}

// ProxyKey is a proxy's identity in a file.
func ProxyKey(p kuma.Object) string { return strings.ToLower(proxyKey(p)) }

// DefaultFor is the baseline of a field (see defaults.go).
func DefaultFor(kind, typ, field string) (any, bool) { return kindDefault(kind, typ, field) }

// BaselineFields are the monitor fields the baseline knows, for a type.
func BaselineFields(typ string) map[string]any {
	out := map[string]any{}
	for k, v := range baseline {
		out[k] = v
	}
	for k, v := range typeBaseline[typ] {
		out[k] = v
	}
	return out
}
