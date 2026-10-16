// Package apply makes an instance match a file, following a plan.
package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/kumafile/internal/config"
	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

// Options say what to do.
type Options struct {
	Client *kuma.Client
	State  *kuma.State // the instance when the plan was made
	Want   *config.Doc
	Plan   *config.Plan
	Log    func(string)
}

type applier struct {
	o   Options
	c   *kuma.Client
	raw map[string]map[string]kuma.Object // kind -> key -> the server's object
	ids map[string]map[string]int64       // kind -> key -> id, including what was created
	// fields are the monitor fields this server stores (from its own
	// monitors), so a new monitor only sends what it knows.
	fields map[string]bool
}

func (a *applier) log(f string, args ...any) {
	if a.o.Log != nil {
		a.o.Log(fmt.Sprintf(f, args...))
	}
}

// Run carries out the plan.
func Run(ctx context.Context, o Options) error {
	if len(o.Plan.Problems) > 0 {
		return fmt.Errorf("the file has problems to solve first: %s", strings.Join(o.Plan.Problems, "; "))
	}
	a := &applier{o: o, c: o.Client, raw: map[string]map[string]kuma.Object{}, ids: map[string]map[string]int64{}}
	a.index()
	// A probe an interrupted run left behind.
	for _, m := range o.State.Monitors {
		if m.Str("name") == ProbeName && m["active"] != true {
			if _, err := a.c.Do(ctx, "deleteMonitor", m.ID()); err != nil {
				return fmt.Errorf("removing the leftover %q: %w", ProbeName, err)
			}
		}
	}

	// Before writing anything: every monitor field the file sets must be
	// one this server has (an export of a newer Kuma applied to an older
	// one would otherwise lose them).
	if acts := a.actions("monitors", config.Create, config.Update); len(acts) > 0 {
		if err := a.knowFields(ctx); err != nil {
			return err
		}
		var unknown []string
		for _, act := range acts {
			for f, v := range act.Want.Fields {
				if !refFields[f] && !a.fields[f] && f != "active" && !strings.HasPrefix(f, "_") && v != nil {
					unknown = append(unknown, fmt.Sprintf("monitor %s: %s", act.Want.Label, f))
				}
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("this Uptime Kuma (%s) does not have these fields, so nothing was changed:\n  - %s",
				o.State.Info.Version, strings.Join(unknown, "\n  - "))
		}
	}

	// Creates and updates, what others refer to first; groups before what
	// they contain.
	for _, k := range config.Kinds {
		acts := a.actions(k.Key, config.Create, config.Update)
		if k.Key == "monitors" {
			sort.SliceStable(acts, func(i, j int) bool { return depth(acts[i].Want.Key) < depth(acts[j].Want.Key) })
		}
		for _, act := range acts {
			if err := a.do(ctx, act); err != nil {
				return fmt.Errorf("%s %s: %w", k.One, act.Label(), err)
			}
		}
	}
	// Deletes, in the reverse order; what a group contains before it.
	for i := len(config.Kinds) - 1; i >= 0; i-- {
		k := config.Kinds[i]
		acts := a.actions(k.Key, config.Delete)
		if k.Key == "monitors" {
			sort.SliceStable(acts, func(i, j int) bool { return depth(acts[i].Have.Key) > depth(acts[j].Have.Key) })
		}
		for _, act := range acts {
			if err := a.remove(ctx, act); err != nil {
				return fmt.Errorf("deleting %s %s: %w", k.One, act.Label(), err)
			}
			a.log("- %s %s", k.One, act.Label())
		}
	}
	return nil
}

func depth(key string) int { return strings.Count(key, config.PathSep) }

func (a *applier) actions(kind string, ops ...config.Op) []config.Action {
	var out []config.Action
	for _, act := range a.o.Plan.Actions {
		if act.Kind.Key != kind {
			continue
		}
		for _, op := range ops {
			if act.Op == op {
				out = append(out, act)
			}
		}
	}
	return out
}

func (a *applier) index() {
	st := a.o.State
	put := func(kind, key string, o kuma.Object) {
		if a.raw[kind] == nil {
			a.raw[kind] = map[string]kuma.Object{}
			a.ids[kind] = map[string]int64{}
		}
		a.raw[kind][key] = o
		a.ids[kind][key] = o.ID()
	}
	for _, k := range config.Kinds {
		a.raw[k.Key] = map[string]kuma.Object{}
		a.ids[k.Key] = map[string]int64{}
	}
	for _, t := range st.Tags {
		put("tags", strings.ToLower(t.Str("name")), t)
	}
	for _, n := range st.Notifications {
		put("notifications", strings.ToLower(n.Str("name")), n)
	}
	for _, p := range st.Proxies {
		put("proxies", config.ProxyKey(p), p)
	}
	for _, h := range st.DockerHosts {
		put("docker_hosts", strings.ToLower(h.Str("name")), h)
	}
	for _, b := range st.RemoteBrowsers {
		put("remote_browsers", strings.ToLower(b.Str("name")), b)
	}
	keys := config.MonitorKeys(st)
	for _, m := range st.Monitors {
		put("monitors", keys[m.ID()], m)
	}
	for _, mt := range st.Maintenance {
		put("maintenance", strings.ToLower(mt.Str("title")), mt.Object)
	}
	for _, sp := range st.StatusPages {
		put("status_pages", strings.ToLower(sp.Str("slug")), sp.Object)
	}
	if len(st.Monitors) > 0 {
		a.fields = map[string]bool{}
		for k := range st.Monitors[0] {
			a.fields[k] = true
		}
	}
}

// id finds what a name refers to (0 if none).
func (a *applier) id(kind, name string) int64 {
	return a.ids[kind][strings.ToLower(name)]
}

func (a *applier) do(ctx context.Context, act config.Action) error {
	verb := "+"
	if act.Op == config.Update {
		verb = "~"
		// Others in the file refer to it by its new name or place.
		a.ids[act.Kind.Key][act.Want.Key] = a.ids[act.Kind.Key][act.Have.Key]
	}
	var err error
	switch act.Kind.Key {
	case "tags":
		err = a.tag(ctx, act)
	case "notifications":
		err = a.notification(ctx, act)
	case "proxies":
		err = a.proxy(ctx, act)
	case "docker_hosts":
		err = a.saveSimple(ctx, act, "addDockerHost", "name", "dockerType", "dockerDaemon")
	case "remote_browsers":
		err = a.saveSimple(ctx, act, "addRemoteBrowser", "name", "url")
	case "monitors":
		err = a.monitor(ctx, act)
	case "status_pages":
		err = a.statusPage(ctx, act)
	case "maintenance":
		err = a.maintenance(ctx, act)
	}
	if err == nil {
		a.log("%s %s %s", verb, act.Kind.One, act.Label())
	}
	return err
}

func (a *applier) existing(act config.Action) (int64, bool) {
	if act.Op != config.Update {
		return 0, false
	}
	return a.ids[act.Kind.Key][act.Have.Key], true
}

func (a *applier) created(act config.Action, id int64) {
	a.ids[act.Kind.Key][act.Want.Key] = id
}

func field(it *config.Item, kind, typ, f string) any {
	if v, ok := it.Fields[f]; ok {
		return v
	}
	v, _ := config.DefaultFor(kind, typ, f)
	return v
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case nil:
		return false
	case string:
		return x != "" && x != "0" && x != "false"
	}
	return kuma.Int(v) != 0
}

func (a *applier) tag(ctx context.Context, act config.Action) error {
	w := act.Want.Fields
	payload := map[string]any{"name": w["name"], "color": w["color"]}
	if id, ok := a.existing(act); ok {
		payload["id"] = id
		_, err := a.c.Do(ctx, "editTag", payload)
		return err
	}
	r, err := a.c.Do(ctx, "addTag", payload)
	if err != nil {
		return err
	}
	tag, _ := r["tag"].(map[string]any)
	a.created(act, kuma.Int(tag["id"]))
	return nil
}

// idArg is the id to save over, or null for something new.
func idArg(id int64, ok bool) any {
	if !ok {
		return nil
	}
	return id
}

func (a *applier) notification(ctx context.Context, act config.Action) error {
	w := act.Want.Fields
	payload := map[string]any{}
	if s, ok := w["settings"].(map[string]any); ok {
		for k, v := range s {
			payload[k] = v
		}
	}
	payload["name"] = w["name"]
	payload["type"] = w["type"]
	payload["isDefault"] = truthy(field(act.Want, "notifications", "", "default"))
	payload["applyExisting"] = false
	id, ok := a.existing(act)
	r, err := a.c.Do(ctx, "addNotification", payload, idArg(id, ok))
	if err != nil {
		return err
	}
	if !ok {
		a.created(act, kuma.Int(r["id"]))
	}
	return nil
}

func (a *applier) proxy(ctx context.Context, act config.Action) error {
	w := act.Want.Fields
	payload := map[string]any{
		"protocol": w["protocol"], "host": w["host"], "port": kuma.Int(w["port"]),
		"auth":     truthy(field(act.Want, "proxies", "", "auth")),
		"username": w["username"], "password": w["password"],
		"default": truthy(field(act.Want, "proxies", "", "default")), "applyExisting": false,
	}
	id, ok := a.existing(act)
	r, err := a.c.Do(ctx, "addProxy", payload, idArg(id, ok))
	if err != nil {
		return err
	}
	if !ok {
		a.created(act, kuma.Int(r["id"]))
	}
	return nil
}

func (a *applier) saveSimple(ctx context.Context, act config.Action, event string, fields ...string) error {
	payload := map[string]any{}
	for _, f := range fields {
		payload[f] = act.Want.Fields[f]
	}
	id, ok := a.existing(act)
	r, err := a.c.Do(ctx, event, payload, idArg(id, ok))
	if err != nil {
		return err
	}
	if !ok {
		a.created(act, kuma.Int(r["id"]))
	}
	return nil
}

// refFields are written by name in the file and as ids on the server.
var refFields = map[string]bool{"group": true, "notifications": true, "tags": true, "proxy": true, "docker_host": true, "remote_browser": true}

// derived are fields of the server's monitor objects that are not settings.
var derived = map[string]bool{"id": true, "path": true, "pathName": true, "childrenIDs": true, "maintenance": true,
	"tags": true, "includeSensitiveData": true, "forceInactive": true, "screenshot": true}

func (a *applier) monitor(ctx context.Context, act config.Action) error {
	w := act.Want
	typ := fmt.Sprint(w.Fields["type"])
	payload := map[string]any{}
	id, update := a.existing(act)
	if update {
		// Everything the server has, so nothing kumafile does not manage is
		// lost (the server sets every field it is sent).
		raw, _ := json.Marshal(a.raw["monitors"][act.Have.Key])
		json.Unmarshal(raw, &payload)
	}
	for k, v := range config.BaselineFields(typ) {
		if _, set := w.Fields[k]; !set {
			payload[k] = v
		}
	}
	for k, v := range w.Fields {
		if !refFields[k] {
			payload[k] = v
		}
	}
	for k := range derived {
		delete(payload, k)
	}
	// References.
	payload["parent"] = nil
	if g, _ := w.Fields["group"].(string); g != "" {
		pid := a.id("monitors", g)
		if pid == 0 {
			return fmt.Errorf("group %q was not found", g)
		}
		payload["parent"] = pid
	}
	notes := map[string]any{}
	for _, n := range list(w.Fields["notifications"]) {
		notes[strconv.FormatInt(a.id("notifications", fmt.Sprint(n)), 10)] = true
	}
	payload["notificationIDList"] = notes
	payload["proxyId"] = nullID(a.id("proxies", str(w.Fields["proxy"])))
	payload["docker_host"] = nullID(a.id("docker_hosts", str(w.Fields["docker_host"])))
	payload["remote_browser"] = nullID(a.id("remote_browsers", str(w.Fields["remote_browser"])))
	// Shapes the server insists on.
	codes := []string{}
	for _, c := range list(payload["accepted_statuscodes"]) {
		codes = append(codes, fmt.Sprint(c))
	}
	payload["accepted_statuscodes"] = codes
	if s, ok := payload["kafkaProducerSaslOptions"].(string); ok && strings.HasPrefix(strings.TrimSpace(s), "{") {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			payload["kafkaProducerSaslOptions"] = v
		}
	}
	if payload["conditions"] == nil {
		payload["conditions"] = []any{}
	}
	// Monitors made through the API can have a retry interval of 0, which
	// the server refuses when it is sent; left out, the server keeps (or
	// stores) exactly that.
	for _, f := range []string{"interval", "retryInterval"} {
		if v, ok := payload[f]; ok && v != nil && kuma.Int(v) < 1 {
			delete(payload, f)
		}
	}
	active := truthy(field(w, "monitors", typ, "active"))

	if update {
		payload["id"] = id
		if _, err := a.c.Do(ctx, "editMonitor", payload); err != nil {
			return err
		}
		live := a.raw["monitors"][act.Have.Key]
		// Under a paused group the server reports the monitor as paused
		// whatever its own setting, which it does not show: leave it.
		if was := truthy(live["active"]); was != active && !truthy(live["forceInactive"]) {
			event := "pauseMonitor"
			if active {
				event = "resumeMonitor"
			}
			if _, err := a.c.Do(ctx, event, id); err != nil {
				return err
			}
		}
	} else {
		if err := a.knowFields(ctx); err != nil {
			return err
		}
		for k := range payload {
			if !a.fields[k] {
				delete(payload, k) // a field this server does not have
			}
		}
		payload["active"] = active
		r, err := a.c.Do(ctx, "add", payload)
		if err != nil {
			return err
		}
		id = kuma.Int(r["monitorID"])
		a.created(act, id)
	}
	return a.monitorTags(ctx, act, id)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// ProbeName names the paused monitor made for a moment to learn the
// server's fields; one left behind is removed by the next apply.
const ProbeName = "kumafile probe (temporary)"

// knowFields learns which monitor fields the server has: from one of its
// monitors, or from a short-lived paused one when there are none.
func (a *applier) knowFields(ctx context.Context) error {
	if a.fields != nil {
		return nil
	}
	r, err := a.c.Do(ctx, "add", map[string]any{"type": "http", "name": ProbeName, "url": "https://example.com",
		"accepted_statuscodes": []string{"200-299"}, "notificationIDList": map[string]any{}, "conditions": []any{},
		"interval": 60, "retryInterval": 60, "active": false})
	if err != nil {
		return fmt.Errorf("learning the server's monitor fields: %w", err)
	}
	id := kuma.Int(r["monitorID"])
	m, err := a.c.Do(ctx, "getMonitor", id)
	// Remove it even if the context was cancelled meanwhile.
	dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	if _, derr := a.c.Do(dctx, "deleteMonitor", id); derr != nil && err == nil {
		err = fmt.Errorf("%w (remove the paused monitor %q by hand)", derr, ProbeName)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("learning the server's monitor fields: %w", err)
	}
	mon, _ := m["monitor"].(map[string]any)
	a.fields = map[string]bool{"accepted_statuscodes": true, "notificationIDList": true}
	for k := range mon {
		a.fields[k] = true
	}
	return nil
}

// monitorTags makes the monitor's tags (with values) match the file,
// adding them in the file's order.
func (a *applier) monitorTags(ctx context.Context, act config.Action, id int64) error {
	type tv struct {
		tag   int64
		value string
	}
	var want []tv
	wanted := map[tv]bool{}
	for _, t := range list(act.Want.Fields["tags"]) {
		name, value := str(t), ""
		if m, ok := t.(map[string]any); ok {
			name, value = str(m["name"]), fmt.Sprint(nz(m["value"]))
		}
		tid := a.id("tags", name)
		if tid == 0 {
			return fmt.Errorf("tag %q was not found", name)
		}
		if k := (tv{tid, value}); !wanted[k] {
			wanted[k] = true
			want = append(want, k)
		}
	}
	have := map[tv]bool{}
	if act.Op == config.Update {
		for _, t := range list(a.raw["monitors"][act.Have.Key]["tags"]) {
			m, _ := t.(map[string]any)
			k := tv{kuma.Int(m["tag_id"]), fmt.Sprint(nz(m["value"]))}
			have[k] = true
			if !wanted[k] {
				// The stored value as it is (it can be null, which "" does
				// not match).
				if _, err := a.c.Do(ctx, "deleteMonitorTag", k.tag, id, m["value"]); err != nil {
					return err
				}
			}
		}
	}
	for _, k := range want {
		if !have[k] {
			if _, err := a.c.Do(ctx, "addMonitorTag", k.tag, id, k.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func nz(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func (a *applier) statusPage(ctx context.Context, act config.Action) error {
	w := act.Want.Fields
	slug := str(w["slug"])
	cfg := map[string]any{}
	if act.Op == config.Update {
		raw, _ := json.Marshal(a.raw["status_pages"][act.Have.Key])
		json.Unmarshal(raw, &cfg)
		slug = str(cfg["slug"])
	} else {
		title := str(w["title"])
		if _, err := a.c.Do(ctx, "addStatusPage", title, slug); err != nil {
			return err
		}
		r, err := a.c.Do(ctx, "getStatusPage", slug)
		if err != nil {
			return err
		}
		cfg, _ = r["config"].(map[string]any)
		if cfg == nil {
			return fmt.Errorf("the new page %q could not be read back", slug)
		}
		a.created(act, kuma.Int(cfg["id"]))
	}
	for _, f := range config.StatusPageFields() {
		cfg[f] = field(act.Want, "status_pages", "", f)
	}
	for f, v := range w {
		if f != "groups" && f != "logo" {
			cfg[f] = v // also settings of later versions
		}
	}
	if cfg["domainNameList"] == nil {
		cfg["domainNameList"] = []any{}
	}
	if cfg["icon"] == nil {
		cfg["icon"] = "/icon.svg"
	}
	var groups []any
	for _, g := range list(w["groups"]) {
		gm, _ := g.(map[string]any)
		var mons []any
		for _, m := range list(gm["monitors"]) {
			name, show, link := str(m), false, ""
			if mm, ok := m.(map[string]any); ok {
				name, show, link = str(mm["monitor"]), mm["show_url"] == true, str(mm["url"])
			}
			mid := a.id("monitors", name)
			if mid == 0 {
				return fmt.Errorf("monitor %q was not found", name)
			}
			send := 0
			if show {
				send = 1
			}
			entry := map[string]any{"id": mid, "sendUrl": send}
			if link != "" {
				entry["url"] = link
			}
			mons = append(mons, entry)
		}
		if mons == nil {
			mons = []any{}
		}
		groups = append(groups, map[string]any{"name": gm["name"], "monitorList": mons})
	}
	if groups == nil {
		groups = []any{}
	}
	// An uploaded logo travels in the file as a PNG data address; the
	// server stores it as a file again.
	img := cfg["icon"]
	if logo := str(w["logo"]); logo != "" {
		img = logo
	}
	_, err := a.c.Do(ctx, "saveStatusPage", slug, cfg, img, groups)
	return err
}

func (a *applier) maintenance(ctx context.Context, act config.Action) error {
	w := act.Want
	get := func(f string, def any) any {
		if v := field(w, "maintenance", "", f); v != nil {
			return v
		}
		return def
	}
	payload := map[string]any{
		"title": w.Fields["title"], "description": get("description", ""), "strategy": get("strategy", "manual"),
		"active": truthy(get("active", true)), "intervalDay": get("intervalDay", 1),
		"dateRange": get("dateRange", []any{}), "timeRange": get("timeRange", []any{map[string]any{"hours": 0, "minutes": 0}, map[string]any{"hours": 0, "minutes": 0}}),
		"weekdays": get("weekdays", []any{}), "daysOfMonth": get("daysOfMonth", []any{}), "cron": get("cron", ""),
		"durationMinutes": get("durationMinutes", 60), "timezoneOption": get("timezoneOption", "SAME_AS_SERVER"),
	}
	id, update := a.existing(act)
	if update {
		payload["id"] = id
		if _, err := a.c.Do(ctx, "editMaintenance", payload); err != nil {
			return err
		}
	} else {
		r, err := a.c.Do(ctx, "addMaintenance", payload)
		if err != nil {
			return err
		}
		id = kuma.Int(r["maintenanceID"])
		a.created(act, id)
	}
	var mons, pages []any
	for _, m := range list(w.Fields["monitors"]) {
		mid := a.id("monitors", str(m))
		if mid == 0 {
			return fmt.Errorf("monitor %q was not found", str(m))
		}
		mons = append(mons, map[string]any{"id": mid})
	}
	for _, p := range list(w.Fields["status_pages"]) {
		pid := a.id("status_pages", str(p))
		if pid == 0 {
			return fmt.Errorf("status page %q was not found", str(p))
		}
		pages = append(pages, map[string]any{"id": pid})
	}
	if mons == nil {
		mons = []any{}
	}
	if pages == nil {
		pages = []any{}
	}
	if _, err := a.c.Do(ctx, "addMonitorMaintenance", id, mons); err != nil {
		return err
	}
	_, err := a.c.Do(ctx, "addMaintenanceStatusPage", id, pages)
	return err
}

func (a *applier) remove(ctx context.Context, act config.Action) error {
	id := a.ids[act.Kind.Key][act.Have.Key]
	var err error
	switch act.Kind.Key {
	case "tags":
		_, err = a.c.Do(ctx, "deleteTag", id)
	case "notifications":
		_, err = a.c.Do(ctx, "deleteNotification", id)
	case "proxies":
		_, err = a.c.Do(ctx, "deleteProxy", id)
	case "docker_hosts":
		_, err = a.c.Do(ctx, "deleteDockerHost", id)
	case "remote_browsers":
		_, err = a.c.Do(ctx, "deleteRemoteBrowser", id)
	case "monitors":
		// (2.0.0 takes no "delete children" argument; later versions
		// default it to false.)
		_, err = a.c.Do(ctx, "deleteMonitor", id)
	case "maintenance":
		_, err = a.c.Do(ctx, "deleteMaintenance", id)
	case "status_pages":
		_, err = a.c.Do(ctx, "deleteStatusPage", a.raw["status_pages"][act.Have.Key].Str("slug"))
	}
	return err
}
