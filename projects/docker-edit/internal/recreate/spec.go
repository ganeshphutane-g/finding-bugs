// Package recreate turns a container's description into the request that
// creates the same container again, so it can be changed and swapped.
//
// The configuration is kept as the daemon reports it (generic JSON), not
// as typed structures: settings of newer Docker versions pass through
// untouched. Only what the report and the create request disagree on is
// adjusted.
package recreate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/docker-edit/internal/engine"
)

// Doc is a JSON object.
type Doc = engine.Doc

// Spec is how to create a container again.
type Spec struct {
	Name   string // without the leading slash
	Config Doc    // the create request's top level (Image, Env, Cmd, ...)
	Host   Doc    // HostConfig
	// Networks are the endpoints, by network name, in a stable order. With
	// an API older than 1.44 only the first goes in the create request; the
	// rest are connected afterwards.
	Networks []Endpoint
	// NetworkMode is host, none, container:X, or the primary network.
	NetworkMode string
}

// Endpoint is one network connection.
type Endpoint struct {
	Network string
	Config  Doc // Aliases, IPAMConfig, Links, DriverOpts, GwPriority, MacAddress
}

// Read is the container's description for FromInspect. Since API 1.44 the
// description no longer tells a MAC address that was set (--mac-address)
// from one Docker made up; asking as API 1.43 does, so Read adds it as
// Config.MacAddress. With host networking it leaves out the hostname when it
// is only the host's.
func Read(ctx context.Context, c *engine.Client, name string) (Doc, error) {
	ctr, err := c.InspectContainer(ctx, name)
	if err != nil {
		return nil, err
	}
	if str(asDoc(ctr["HostConfig"])["NetworkMode"]) == "host" {
		// With host networking the hostname is the host's, unless one was
		// set: leave the host's out, or it would stay after leaving the host
		// network.
		info, err := c.Info(ctx)
		if err != nil {
			return nil, err
		}
		if cfg := asDoc(ctr["Config"]); info.Name != "" && str(cfg["Hostname"]) == info.Name {
			delete(cfg, "Hostname")
		}
	}
	api, err := c.APIVersion(ctx)
	if err != nil || engine.CompareVersions(api, "1.44") < 0 {
		return ctr, err
	}
	var old Doc
	err = c.Call(ctx, engine.Request{Method: http.MethodGet, Path: "/containers/" + url.PathEscape(str(ctr["Id"])) + "/json", Version: "1.43"}, &old)
	if err != nil {
		return nil, fmt.Errorf("reading the container's MAC address setting: %w", err)
	}
	if mac := str(asDoc(old["Config"])["MacAddress"]); mac != "" {
		asDoc(ctr["Config"])["MacAddress"] = mac
	}
	return ctr, nil
}

// Body is the create request.
func (s *Spec) Body(apiVersion string) Doc {
	body := Doc{}
	for k, v := range s.Config {
		body[k] = v
	}
	body["HostConfig"] = s.Host
	if len(s.Networks) > 0 {
		eps := Doc{}
		old := engine.CompareVersions(apiVersion, "1.44") < 0
		for i, ep := range s.Networks {
			if i > 0 && old {
				break
			}
			conf := ep.Config
			if mac := str(conf["MacAddress"]); mac != "" && old {
				// Older APIs take the MAC address at the top level.
				conf = Doc{}
				for k, v := range ep.Config {
					conf[k] = v
				}
				delete(conf, "MacAddress")
				body["MacAddress"] = mac
			}
			eps[ep.Network] = conf
		}
		body["NetworkingConfig"] = Doc{"EndpointsConfig": eps}
	}
	return body
}

// Later are the networks to connect after creating (older APIs).
func (s *Spec) Later(apiVersion string) []Endpoint {
	if engine.CompareVersions(apiVersion, "1.44") >= 0 || len(s.Networks) < 2 {
		return nil
	}
	return s.Networks[1:]
}

var anonymousName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FromInspect builds the spec from `docker inspect` output.
func FromInspect(ctr Doc) (*Spec, error) {
	cfg, ok := deep(ctr["Config"]).(Doc)
	if !ok {
		return nil, fmt.Errorf("the container description has no Config")
	}
	host, _ := deep(ctr["HostConfig"]).(Doc)
	if host == nil {
		host = Doc{}
	}
	id, _ := ctr["Id"].(string)
	name := strings.TrimPrefix(str(ctr["Name"]), "/")
	s := &Spec{Name: name, Config: cfg, Host: host, NetworkMode: str(host["NetworkMode"])}

	// An automatic hostname is the old container's short ID: leave it out
	// so the new container gets its own.
	if h := str(cfg["Hostname"]); len(id) >= 12 && h == id[:12] {
		delete(cfg, "Hostname")
	}

	// Anonymous volumes are not in the host config; name them, so their
	// data stays.
	anonymous := map[string]string{} // destination -> volume name
	for _, m := range list(ctr["Mounts"]) {
		if md, _ := m.(Doc); str(md["Type"]) == "volume" && anonymousName.MatchString(str(md["Name"])) {
			anonymous[str(md["Destination"])] = str(md["Name"])
		}
	}
	// A volume mount without a source (--mount type=volume,dst=/x) made an
	// anonymous volume: name it, or the new container would get an empty one.
	for _, m := range list(host["Mounts"]) {
		if md, ok := m.(Doc); ok && str(md["Type"]) == "volume" && str(md["Source"]) == "" {
			if name := anonymous[str(md["Target"])]; name != "" {
				md["Source"] = name
			}
		}
	}
	taken := map[string]bool{}
	for _, b := range list(host["Binds"]) {
		if parts := strings.Split(str(b), ":"); len(parts) >= 2 {
			taken[parts[1]] = true
		}
	}
	for _, m := range list(host["Mounts"]) {
		if md, ok := m.(Doc); ok {
			taken[str(md["Target"])] = true
		}
	}
	for t := range asDoc(host["Tmpfs"]) {
		taken[t] = true
	}
	var mounts []any
	if existing := list(host["Mounts"]); existing != nil {
		mounts = existing
	}
	for _, m := range list(ctr["Mounts"]) {
		md, _ := m.(Doc)
		dst := str(md["Destination"])
		if str(md["Type"]) != "volume" || taken[dst] || !anonymousName.MatchString(str(md["Name"])) {
			continue
		}
		mount := Doc{"Type": "volume", "Source": md["Name"], "Target": dst}
		if rw, ok := md["RW"].(bool); ok && !rw {
			mount["ReadOnly"] = true
		}
		mounts = append(mounts, mount)
		taken[dst] = true
	}
	if len(mounts) > 0 {
		host["Mounts"] = mounts
	}

	// Legacy links are reported as "/other:/this/alias"; created as
	// "other:alias".
	if links := list(host["Links"]); len(links) > 0 {
		var out []any
		for _, l := range links {
			parts := strings.SplitN(str(l), ":", 2)
			if len(parts) == 2 {
				alias := parts[1]
				if i := strings.LastIndex(alias, "/"); i >= 0 {
					alias = alias[i+1:]
				}
				out = append(out, strings.TrimPrefix(parts[0], "/")+":"+alias)
			}
		}
		host["Links"] = out
	}

	// A MAC address that was set goes with the primary network.
	mac := str(cfg["MacAddress"])
	delete(cfg, "MacAddress")

	// Sharing another container's network: the hostname and ports are that
	// container's, and the create request must not name them.
	if strings.HasPrefix(s.NetworkMode, "container:") {
		for _, k := range []string{"Hostname", "Domainname", "ExposedPorts"} {
			delete(cfg, k)
		}
	}

	// Networks.
	mode := s.NetworkMode
	if mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:") {
		nets := asDoc(asDoc(ctr["NetworkSettings"])["Networks"])
		var names []string
		for n := range nets {
			names = append(names, n)
		}
		// The primary network first, then the rest by name.
		primary := mode
		if primary == "default" {
			primary = "bridge"
		}
		sort.Slice(names, func(i, j int) bool {
			if (names[i] == primary) != (names[j] == primary) {
				return names[i] == primary
			}
			return names[i] < names[j]
		})
		for _, n := range names {
			ep := asDoc(nets[n])
			conf := Doc{}
			var aliases []any
			for _, a := range list(ep["Aliases"]) {
				// Older daemons list the container's short ID and name as
				// aliases; they are added automatically.
				if as := str(a); as != "" && (len(id) < 12 || as != id[:12]) && as != name {
					aliases = append(aliases, as)
				}
			}
			if len(aliases) > 0 && n != "bridge" {
				conf["Aliases"] = aliases
			}
			if ipam := asDoc(ep["IPAMConfig"]); len(nonEmpty(ipam)) > 0 {
				conf["IPAMConfig"] = nonEmpty(ipam)
			}
			if l := list(ep["Links"]); len(l) > 0 {
				conf["Links"] = l
			}
			if d := asDoc(ep["DriverOpts"]); len(d) > 0 {
				conf["DriverOpts"] = d
			}
			if p, ok := ep["GwPriority"].(json.Number); ok && p.String() != "0" {
				conf["GwPriority"] = p
			}
			if len(s.Networks) == 0 {
				if mac != "" {
					conf["MacAddress"] = mac
				}
			} else if m := str(ep["MacAddress"]); m != "" {
				// For further networks the description does not tell a set
				// MAC address from an automatic one: keep it either way.
				conf["MacAddress"] = m
			}
			s.Networks = append(s.Networks, Endpoint{Network: n, Config: conf})
		}
	}
	return s, nil
}

// DropImageDefaults removes what the container only has because its image
// set it, so another image's defaults can apply. It is used when the
// image changes.
func (s *Spec) DropImageDefaults(oldImage Doc) {
	s.dropDefaults(oldImage, false)
}

// DropNewImageNames is DropImageDefaults for when the old image is gone, so
// its defaults are unknown: environment variables and labels the new image
// sets are taken to be image defaults and dropped, whatever their value. It
// returns the ones whose value will change.
func (s *Spec) DropNewImageNames(newImage Doc) []string {
	return s.dropDefaults(newImage, true)
}

func (s *Spec) dropDefaults(image Doc, byName bool) []string {
	img := asDoc(image["Config"])
	if img == nil {
		return nil
	}
	var changed []string
	// Environment: entries equal to the image's (or of the same name).
	if env := list(s.Config["Env"]); env != nil {
		defaults := map[string]string{}
		for _, e := range list(img["Env"]) {
			k, _, _ := strings.Cut(str(e), "=")
			defaults[k] = str(e)
		}
		var keep []any
		for _, e := range env {
			k, _, _ := strings.Cut(str(e), "=")
			d, ok := defaults[k]
			switch {
			case ok && d == str(e):
			case ok && byName:
				changed = append(changed, "env "+k)
			default:
				keep = append(keep, e)
			}
		}
		s.Config["Env"] = keep
	}
	// Labels: those equal to the image's (or of the same name).
	if labels := asDoc(s.Config["Labels"]); labels != nil {
		il := asDoc(img["Labels"])
		for k, v := range labels {
			if iv, ok := il[k]; ok && (iv == v || byName) {
				if iv != v {
					changed = append(changed, "label "+k)
				}
				delete(labels, k)
			}
		}
	}
	// Whole settings equal to the image's. Docker takes the image's command
	// only along with its entrypoint, so a command stays with an entrypoint
	// of the container's own.
	ownEntrypoint := s.Config["Entrypoint"] != nil && !same(s.Config["Entrypoint"], img["Entrypoint"])
	for _, k := range []string{"Cmd", "Entrypoint", "WorkingDir", "User", "Healthcheck", "StopSignal", "Shell", "OnBuild", "ExposedPorts", "Volumes"} {
		if k == "Cmd" && ownEntrypoint {
			continue
		}
		if same(s.Config[k], img[k]) {
			delete(s.Config, k)
		}
	}
	sort.Strings(changed)
	return changed
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func asDoc(v any) Doc {
	d, _ := v.(Doc)
	return d
}

func nonEmpty(d Doc) Doc {
	out := Doc{}
	for k, v := range d {
		switch x := v.(type) {
		case nil:
		case string:
			if x != "" {
				out[k] = x
			}
		case []any:
			if len(x) > 0 {
				out[k] = x
			}
		default:
			out[k] = v
		}
	}
	return out
}

// deep copies a JSON value.
func deep(v any) any {
	switch x := v.(type) {
	case Doc:
		out := Doc{}
		for k, e := range x {
			out[k] = deep(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deep(e)
		}
		return out
	}
	return v
}

func same(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
