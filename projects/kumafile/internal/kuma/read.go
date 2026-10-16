package kuma

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Object is one item as the server describes it. Numbers stay exact
// (json.Number).
type Object map[string]any

// ID is the object's numeric id (0 if missing).
func (o Object) ID() int64 { return Int(o["id"]) }

// Str is a string field ("" if missing or not a string).
func (o Object) Str(k string) string {
	s, _ := o[k].(string)
	return s
}

// Int reads a number however it was decoded.
func Int(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

func decode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(out)
}

// Maintenance is a maintenance window with what it covers.
type Maintenance struct {
	Object
	Monitors    []int64
	StatusPages []int64
}

// StatusPage is a status page with its groups of monitors.
type StatusPage struct {
	Object
	Groups []Group
}

// Group is a section of a status page.
type Group struct {
	Name     string
	Monitors []GroupMonitor
}

// GroupMonitor is a monitor shown on a status page, with how it is shown.
type GroupMonitor struct {
	ID      int64
	SendURL bool // the monitor's address is shown
	URL     string
}

// State is the configuration of an instance.
type State struct {
	Info           Info
	Monitors       []Object
	Notifications  []Object
	Tags           []Object
	Proxies        []Object
	DockerHosts    []Object
	RemoteBrowsers []Object
	// MonitorTypes are the types the server announced (built-in ones are
	// not announced); nil when it announced none.
	MonitorTypes []string
	Maintenance  []Maintenance
	StatusPages  []StatusPage
}

// Read collects the instance's configuration. The connection must be
// freshly logged in: lists are what the server pushed at login.
func (c *Client) Read(ctx context.Context) (*State, error) {
	st := &State{Info: c.Info()}
	c.mu.Lock()
	st.MonitorTypes = c.types
	c.mu.Unlock()
	var err error
	if st.Monitors, err = c.objects("monitorList"); err != nil {
		return nil, err
	}
	if st.Notifications, err = c.objects("notificationList"); err != nil {
		return nil, err
	}
	// A notification's settings are a JSON text inside it.
	for _, n := range st.Notifications {
		if s, ok := n["config"].(string); ok {
			var cfg map[string]any
			if err := decode([]byte(s), &cfg); err != nil {
				return nil, fmt.Errorf("notification %q: unreadable settings: %w", n.Str("name"), err)
			}
			n["config"] = cfg
		}
	}
	if st.Proxies, err = c.objects("proxyList"); err != nil {
		return nil, err
	}
	if st.DockerHosts, err = c.objects("dockerHostList"); err != nil {
		return nil, err
	}
	if st.RemoteBrowsers, err = c.objects("remoteBrowserList"); err != nil {
		return nil, err
	}
	var tags struct {
		result
		Tags []Object `json:"tags"`
	}
	if err := c.callDecode(ctx, &tags, "getTags"); err != nil {
		return nil, fmt.Errorf("reading tags: %w", err)
	}
	st.Tags = tags.Tags
	sortByID(st.Tags)

	ms, err := c.objects("maintenanceList")
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		var mons struct {
			result
			Monitors []Object `json:"monitors"`
		}
		if err := c.callDecode(ctx, &mons, "getMonitorMaintenance", m.ID()); err != nil {
			return nil, fmt.Errorf("maintenance %q: %w", m.Str("title"), err)
		}
		var pages struct {
			result
			StatusPages []Object `json:"statusPages"`
		}
		if err := c.callDecode(ctx, &pages, "getMaintenanceStatusPage", m.ID()); err != nil {
			return nil, fmt.Errorf("maintenance %q: %w", m.Str("title"), err)
		}
		mt := Maintenance{Object: m}
		for _, x := range mons.Monitors {
			mt.Monitors = append(mt.Monitors, x.ID())
		}
		for _, x := range pages.StatusPages {
			mt.StatusPages = append(mt.StatusPages, x.ID())
		}
		st.Maintenance = append(st.Maintenance, mt)
	}

	pages, err := c.objects("statusPageList")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		sp := StatusPage{Object: p}
		if icon := p.Str("icon"); strings.HasPrefix(icon, "/upload/") {
			// An uploaded logo is a file on this instance: carry the
			// image itself.
			data, ctype, err := c.Fetch(ctx, icon, 5<<20)
			if err != nil {
				return nil, fmt.Errorf("status page %q: its logo: %w", p.Str("slug"), err)
			}
			if strings.HasPrefix(ctype, "image/png") || bytes.HasPrefix(data, []byte("\x89PNG")) {
				p["logo"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
			}
		}
		var data struct {
			PublicGroupList []struct {
				Name        string   `json:"name"`
				MonitorList []Object `json:"monitorList"`
			} `json:"publicGroupList"`
		}
		if err := c.HTTP(ctx, "/api/status-page/"+PathEscape(p.Str("slug")), &data); err != nil {
			return nil, fmt.Errorf("status page %q: %w", p.Str("slug"), err)
		}
		for _, g := range data.PublicGroupList {
			gr := Group{Name: g.Name}
			for _, m := range g.MonitorList {
				gm := GroupMonitor{ID: m.ID(), URL: m.Str("url")}
				gm.SendURL = Int(m["sendUrl"]) != 0
				gr.Monitors = append(gr.Monitors, gm)
			}
			sp.Groups = append(sp.Groups, gr)
		}
		st.StatusPages = append(st.StatusPages, sp)
	}
	return st, nil
}

// callDecode is call with exact numbers.
func (c *Client) callDecode(ctx context.Context, out okResult, event string, args ...any) error {
	res, err := c.conn.Call(ctx, event, args...)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("%s: no answer", event)
	}
	if err := decode(res[0], out); err != nil {
		return fmt.Errorf("%s: unreadable answer: %w", event, err)
	}
	return out.err()
}

// objects reads a pushed list, which is either an array or an object keyed
// by id, sorted by id.
func (c *Client) objects(name string) ([]Object, error) {
	raw := c.list(name)
	if len(raw) == 0 {
		return nil, fmt.Errorf("the server did not send its %s (is this Uptime Kuma 2.x?)", name)
	}
	var out []Object
	if raw[0] == '[' {
		if err := decode(raw, &out); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	} else {
		var m map[string]Object
		if err := decode(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, o := range m {
			out = append(out, o)
		}
	}
	sortByID(out)
	return out, nil
}

func sortByID(os []Object) {
	sort.SliceStable(os, func(i, j int) bool { return os[i].ID() < os[j].ID() })
}
