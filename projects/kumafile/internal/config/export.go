package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

// Secret is a value taken out of the file, to be given back through the
// environment as ${Name}.
type Secret struct {
	Name, Value string
}

// FileHeader and EnvHeader start the files kumafile writes (so it knows
// which files it may replace).
const (
	FileHeader = "# Uptime Kuma configuration, exported by kumafile"
	EnvHeader  = "# Secrets for the kumafile configuration"
)

// Export is a file made from an instance.
type Export struct {
	Doc      *yaml.Node
	Secrets  []Secret // empty with inline secrets
	Warnings []string
}

// ExportOptions control the export.
type ExportOptions struct {
	Source        string // where it came from, for the header
	InlineSecrets bool   // keep secrets in the file
	Now           time.Time
	live          bool // for comparing: marks what the server does not show
}

// parentPaused marks, in the instance's own view, a monitor whose group is
// paused: its active setting is not known and not compared.
const parentPaused = "_parent_paused"

// LiveOptions are the options for an export used to compare.
func LiveOptions() ExportOptions { return ExportOptions{InlineSecrets: true, live: true} }

// monitorDropped are fields the server derives or that are written another
// way in the file.
var monitorDropped = map[string]bool{
	"id": true, "path": true, "pathName": true, "childrenIDs": true, "weight": true, "maintenance": true,
	"forceInactive": true, "dns_last_result": true, "includeSensitiveData": true, "parent": true,
	"notificationIDList": true, "tags": true, "proxyId": true, "docker_host": true, "remote_browser": true,
	"user_id": true, "screenshot": true,
}

// monitorFirst are shown first, in this order.
var monitorFirst = []string{"name", "group", "type", "active", "description", "url", "hostname", "port", "method",
	"keyword", "invertKeyword", "interval", "retryInterval", "maxretries", "resendInterval", "timeout"}

type exporter struct {
	opt      ExportOptions
	secrets  []Secret
	names    map[string]int
	warnings []string
}

func (e *exporter) warn(f string, a ...any) { e.warnings = append(e.warnings, fmt.Sprintf(f, a...)) }

// secret replaces a value with a reference, or keeps it (inline secrets).
func (e *exporter) secret(value string, parts ...string) any {
	if value == "" || e.opt.InlineSecrets {
		return escape(value)
	}
	name := envName(parts...)
	if n := e.names[name]; n > 0 {
		e.names[name] = n + 1
		name = fmt.Sprintf("%s_%d", name, n+1)
	} else {
		e.names[name] = 1
	}
	e.secrets = append(e.secrets, Secret{Name: name, Value: value})
	return "${" + name + "}"
}

var notWord = regexp.MustCompile(`[^A-Z0-9]+`)

func envName(parts ...string) string {
	var out []string
	for _, p := range parts {
		p = strings.Trim(notWord.ReplaceAllString(strings.ToUpper(p), "_"), "_")
		if len(p) > 30 {
			p = strings.TrimRight(p[:30], "_")
		}
		if p != "" {
			out = append(out, p)
		}
	}
	// The prefix marks what kumafile may take from the environment.
	return "KUMA_" + strings.Join(out, "_")
}

// escape protects text that looks like a ${reference}.
func escape(s string) string { return strings.ReplaceAll(s, "${", "$${") }

// value prepares a plain value for the file: strings are escaped, nested
// values converted.
func value(v any) any {
	switch x := v.(type) {
	case string:
		return escape(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = value(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = value(e)
		}
		return out
	case kuma.Object:
		return value(map[string]any(x))
	}
	return v
}

// secretsIn takes credentials out of nested settings, by key name.
func (e *exporter) secretsIn(v any, parts ...string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable secret names
		for _, k := range keys {
			sub := append(append([]string(nil), parts...), k)
			if s, ok := x[k].(string); ok && secretSetting(k, s) {
				out[k] = e.secret(s, sub...)
				continue
			}
			out[k] = e.secretsIn(x[k], sub...)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e2 := range x {
			out[i] = e.secretsIn(e2, parts...)
		}
		return out
	}
	return value(v)
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// Make builds the file for a state.
func Make(st *kuma.State, opt ExportOptions) (*Export, error) {
	e := &exporter{opt: opt, names: map[string]int{}}
	ix := newIndex(st)
	if err := ix.check(); err != nil {
		return nil, err
	}
	st = ix.st

	doc := Obj{{"version", 1}}

	if len(st.Tags) > 0 {
		var tags []Obj
		for _, t := range st.Tags {
			tags = append(tags, Obj{{"name", value(t.Str("name"))}, {"color", t.Str("color")}})
		}
		doc = append(doc, KV{"tags", tags})
	}

	if len(st.Notifications) > 0 {
		var ns []Obj
		for _, n := range st.Notifications {
			cfg, _ := n["config"].(map[string]any)
			o := Obj{{"name", value(n.Str("name"))}, {"type", fmt.Sprint(cfg["type"])}}
			if kuma.Int(n["isDefault"]) != 0 || cfg["isDefault"] == true {
				o = append(o, KV{"default", true})
			}
			rest := map[string]any{}
			for k, v := range cfg {
				switch k {
				case "name", "type", "isDefault", "applyExisting", "id", "userId", "active":
					continue
				}
				if empty(v) {
					continue
				}
				rest[k] = v
			}
			if len(rest) > 0 {
				o = append(o, KV{"settings", e.secretsIn(rest, "notification", n.Str("name"))})
			}
			ns = append(ns, o)
		}
		doc = append(doc, KV{"notifications", ns})
	}

	if len(st.Proxies) > 0 {
		var ps []Obj
		for _, p := range st.Proxies {
			o := Obj{{"protocol", p.Str("protocol")}, {"host", value(p.Str("host"))}, {"port", p["port"]}}
			if kuma.Int(p["auth"]) != 0 {
				o = append(o, KV{"auth", true}, KV{"username", value(p.Str("username"))},
					KV{"password", e.secret(p.Str("password"), "proxy", p.Str("host"), "password")})
			}
			if kuma.Int(p["default"]) != 0 {
				o = append(o, KV{"default", true})
			}
			ps = append(ps, o)
		}
		doc = append(doc, KV{"proxies", ps})
	}

	if len(st.DockerHosts) > 0 {
		var hs []Obj
		for _, h := range st.DockerHosts {
			hs = append(hs, Obj{{"name", value(h.Str("name"))}, {"dockerType", h.Str("dockerType")}, {"dockerDaemon", value(h.Str("dockerDaemon"))}})
		}
		doc = append(doc, KV{"docker_hosts", hs})
	}

	if len(st.RemoteBrowsers) > 0 {
		var bs []Obj
		for _, b := range st.RemoteBrowsers {
			u := b.Str("url")
			var uv any = value(u)
			if secretField("remote_browsers", "url", u) {
				uv = e.secret(u, "browser", b.Str("name"), "url")
			}
			bs = append(bs, Obj{{"name", value(b.Str("name"))}, {"url", uv}})
		}
		doc = append(doc, KV{"remote_browsers", bs})
	}

	if len(st.Monitors) > 0 {
		var ms []Obj
		for _, m := range ix.monitorsInOrder() {
			ms = append(ms, e.monitor(m, ix))
		}
		doc = append(doc, KV{"monitors", ms})
	}

	if len(st.Maintenance) > 0 {
		var mts []Obj
		for _, mt := range st.Maintenance {
			mts = append(mts, maintenance(mt, ix))
		}
		doc = append(doc, KV{"maintenance", mts})
	}

	if len(st.StatusPages) > 0 {
		var sps []Obj
		for _, sp := range st.StatusPages {
			sps = append(sps, statusPage(sp, ix))
		}
		doc = append(doc, KV{"status_pages", sps})
	}

	root := node(doc)
	when := opt.Now
	if when.IsZero() {
		when = time.Now()
	}
	head := fmt.Sprintf("%s on %s", strings.TrimPrefix(FileHeader, "# "), when.UTC().Format("2006-01-02 15:04 UTC"))
	if opt.Source != "" {
		head += "\nfrom " + opt.Source
		if st.Info.Version != "" {
			head += " (Uptime Kuma " + st.Info.Version + ")"
		}
	}
	if len(e.secrets) > 0 {
		head += "\nSecrets are written as ${NAME}; their values are in the separate environment file."
	}
	root.HeadComment = head
	return &Export{Doc: &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}, Secrets: e.secrets, Warnings: append(ix.warnings, e.warnings...)}, nil
}

func (e *exporter) monitor(m kuma.Object, ix *index) Obj {
	name := m.Str("name")
	fields := map[string]any{}
	for k, v := range m {
		if monitorDropped[k] {
			continue
		}
		fields[k] = v
	}
	typ := m.Str("type")
	if m["forceInactive"] == true {
		// In a paused group the server reports the monitor as paused,
		// whatever its own setting (which it does not show).
		delete(fields, "active")
		if e.opt.live {
			fields[parentPaused] = true
		}
	}
	if u, _ := fields["url"].(string); placeholderURL[u] {
		delete(fields, "url") // the web interface's empty address
	}
	o := Obj{}
	put := func(k string, v any) {
		if k == parentPaused {
			o = append(o, KV{k, true})
			return
		}
		if dv, known := defaultFor(typ, k); known && k != "name" {
			if same(v, dv) {
				return // as the web interface would have it
			}
			if isEmpty(v) {
				// Empty where the default is not.
				if _, list := dv.([]any); list {
					o = append(o, KV{k, []any{}})
				} else {
					o = append(o, KV{k, nil})
				}
				return
			}
		} else if empty(v) {
			return
		}
		if secretField("monitors", k, v) {
			switch s, ok := v.(string); {
			case ok:
				v = e.secret(s, "monitor", name, k)
			case e.opt.InlineSecrets:
				v = value(v)
			default:
				// A structured secret (Kafka SASL options) goes whole, as
				// JSON text.
				raw, _ := json.Marshal(v)
				v = e.secret(string(raw), "monitor", name, k)
			}
		} else {
			v = value(v)
		}
		o = append(o, KV{k, v})
	}
	put("name", name)
	if g := ix.groupPath(m); g != "" {
		o = append(o, KV{"group", escape(g)})
	}
	done := map[string]bool{"name": true}
	for _, k := range monitorFirst[2:] {
		if v, ok := fields[k]; ok {
			put(k, v)
		}
		done[k] = true
	}
	var rest []string
	for k := range fields {
		if !done[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		put(k, fields[k])
	}
	if ns := ix.notificationNames(m); len(ns) > 0 {
		o = append(o, KV{"notifications", ns})
	}
	if ts := ix.tagRefs(m); len(ts) > 0 {
		o = append(o, KV{"tags", ts})
	}
	if p := ix.proxyRef(m["proxyId"]); p != "" {
		o = append(o, KV{"proxy", p})
	}
	if h := ix.dockerHostName(m["docker_host"]); h != "" {
		o = append(o, KV{"docker_host", h})
	}
	if b := ix.browserName(m["remote_browser"]); b != "" {
		o = append(o, KV{"remote_browser", b})
	}
	return o
}

func maintenance(mt kuma.Maintenance, ix *index) Obj {
	o := Obj{{"title", value(mt.Str("title"))}}
	relevant := maintenanceFields(mt.Str("strategy"))
	for _, k := range []string{"description", "strategy", "active", "intervalDay", "dateRange", "timeRange", "weekdays",
		"daysOfMonth", "cron", "durationMinutes", "timezoneOption"} {
		v, ok := mt.Object[k]
		if !ok || !relevant[k] {
			continue
		}
		if k == "timeRange" {
			v = timeRange(v)
		}
		if a, isList := v.([]any); isList && len(a) == 1 && a[0] == nil {
			continue // dateRange [null]: no dates
		}
		if dv, known := kindDefault("maintenance", "", k); known {
			if same(v, dv) {
				continue
			}
			if isEmpty(v) {
				o = append(o, KV{k, nil})
				continue
			}
		}
		if empty(v) {
			continue
		}
		o = append(o, KV{k, value(v)})
	}
	var mons []any
	for _, id := range mt.Monitors {
		mons = append(mons, ix.monitorRef(id))
	}
	if len(mons) > 0 {
		o = append(o, KV{"monitors", mons})
	}
	var pages []any
	for _, id := range mt.StatusPages {
		pages = append(pages, ix.pageSlug(id))
	}
	if len(pages) > 0 {
		o = append(o, KV{"status_pages", pages})
	}
	return o
}

func statusPage(sp kuma.StatusPage, ix *index) Obj {
	o := Obj{{"slug", sp.Str("slug")}, {"title", value(sp.Str("title"))}}
	var rest []string
	for k := range sp.Object {
		switch k {
		case "id", "slug", "title", "published":
			continue // (published cannot be changed through the API)
		case "icon":
			if sp.Str("logo") != "" {
				continue // the uploaded logo goes as "logo"
			}
		}
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		v := sp.Object[k]
		if dv, known := kindDefault("status_pages", "", k); known {
			if same(v, dv) {
				continue
			}
			if isEmpty(v) {
				o = append(o, KV{k, nil})
				continue
			}
		}
		if !empty(v) {
			o = append(o, KV{k, value(v)})
		}
	}
	var groups []Obj
	for _, g := range sp.Groups {
		var mons []any
		for _, gm := range g.Monitors {
			ref := ix.monitorRef(gm.ID)
			if gm.SendURL {
				e := Obj{{"monitor", ref}, {"show_url", true}}
				// The page shows the monitor's own address unless another
				// link was set for it.
				if gm.URL != "" && gm.URL != ix.monitors[gm.ID].Str("url") {
					e = append(e, KV{"url", value(gm.URL)})
				}
				mons = append(mons, e)
			} else {
				mons = append(mons, ref)
			}
		}
		gr := Obj{{"name", value(g.Name)}}
		if len(mons) > 0 {
			gr = append(gr, KV{"monitors", mons})
		}
		groups = append(groups, gr)
	}
	if len(groups) > 0 {
		o = append(o, KV{"groups", groups})
	}
	return o
}
