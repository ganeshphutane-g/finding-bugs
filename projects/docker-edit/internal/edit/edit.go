// Package edit applies requested changes to a container's spec and
// describes the difference for people.
package edit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ganeshphutane-g/docker-edit/internal/recreate"
)

// Doc is a JSON object.
type Doc = recreate.Doc

// Changes are what to change, as given on the command line.
type Changes struct {
	Publish    []string // [ip:]host:container[/proto] or container[/proto]
	Unpublish  []string // container[/proto]
	Env        []string // K=V
	UnsetEnv   []string // K
	Volumes    []string // SRC:DST[:opts]
	Mounts     []string // --mount syntax: type=bind,source=...,target=...
	Unmount    []string // DST
	Restart    string   // no, always, unless-stopped, on-failure[:N]
	Labels     []string // K=V
	Unlabel    []string // K
	Image      string
	Networks   []string // NAME[:alias,alias]
	Disconnect []string // NAME
}

// Empty reports no change at all.
func (c Changes) Empty() bool {
	return len(c.Publish)+len(c.Unpublish)+len(c.Env)+len(c.UnsetEnv)+len(c.Volumes)+len(c.Mounts)+len(c.Unmount)+
		len(c.Labels)+len(c.Unlabel)+len(c.Networks)+len(c.Disconnect) == 0 && c.Restart == "" && c.Image == ""
}

// OnlyRestart reports a change docker update makes in place.
func (c Changes) OnlyRestart() bool {
	r := c.Restart
	c.Restart = ""
	return r != "" && c.Empty()
}

// Apply changes the spec. It returns an error for a malformed change.
func Apply(s *recreate.Spec, c Changes) error {
	host := s.Host
	cfg := s.Config
	for _, p := range c.Unpublish {
		key, err := portKey(p)
		if err != nil {
			return err
		}
		pb := doc(host, "PortBindings")
		if _, ok := pb[key]; !ok {
			return fmt.Errorf("--unpublish %s: the container does not publish %s", p, key)
		}
		delete(pb, key)
	}
	for _, p := range c.Publish {
		key, binding, err := parsePublish(p)
		if err != nil {
			return err
		}
		pb := doc(host, "PortBindings")
		pb[key] = append(list(pb[key]), binding)
		doc(cfg, "ExposedPorts")[key] = Doc{}
	}
	if len(c.Env) > 0 || len(c.UnsetEnv) > 0 {
		env := list(cfg["Env"])
		for _, k := range c.UnsetEnv {
			found := false
			var keep []any
			for _, e := range env {
				if name, _, _ := strings.Cut(str(e), "="); name == k {
					found = true
					continue
				}
				keep = append(keep, e)
			}
			if !found {
				return fmt.Errorf("--unset-env %s: the container has no variable %s", k, k)
			}
			env = keep
		}
		for _, kv := range c.Env {
			k, _, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return fmt.Errorf("--env %q: expected NAME=value", kv)
			}
			replaced := false
			for i, e := range env {
				if name, _, _ := strings.Cut(str(e), "="); name == k {
					env[i] = kv
					replaced = true
				}
			}
			if !replaced {
				env = append(env, kv)
			}
		}
		cfg["Env"] = env
	}
	for _, dst := range c.Unmount {
		if !removeMount(host, dst) {
			return fmt.Errorf("--unmount %s: nothing is mounted there", dst)
		}
	}
	for _, v := range c.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
			return fmt.Errorf("--volume %q: expected SOURCE:/path[:options]", v)
		}
		removeMount(host, parts[1])
		host["Binds"] = append(list(host["Binds"]), v)
	}
	for _, m := range c.Mounts {
		mount, err := parseMount(m)
		if err != nil {
			return err
		}
		removeMount(host, str(mount["Target"]))
		host["Mounts"] = append(list(host["Mounts"]), mount)
	}
	if c.Restart != "" {
		policy, err := parseRestart(c.Restart)
		if err != nil {
			return err
		}
		if policy["Name"] != "no" && host["AutoRemove"] == true {
			return fmt.Errorf("--restart %s: the container is removed when it stops (--rm), so it cannot restart", c.Restart)
		}
		host["RestartPolicy"] = policy
	}
	for _, k := range c.Unlabel {
		labels := doc(cfg, "Labels")
		if _, ok := labels[k]; !ok {
			return fmt.Errorf("--unlabel %s: the container has no label %s", k, k)
		}
		delete(labels, k)
	}
	for _, kv := range c.Labels {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("--label %q: expected NAME=value", kv)
		}
		doc(cfg, "Labels")[k] = v
	}
	if c.Image != "" {
		cfg["Image"] = c.Image
	}
	for _, n := range c.Disconnect {
		found := false
		var keep []recreate.Endpoint
		for _, ep := range s.Networks {
			if ep.Network == n {
				found = true
				continue
			}
			keep = append(keep, ep)
		}
		if !found {
			return fmt.Errorf("--disconnect %s: the container is not on that network", n)
		}
		s.Networks = keep
		if s.NetworkMode == n {
			s.NetworkMode = "none"
			if len(keep) > 0 {
				s.NetworkMode = keep[0].Network
			}
			host["NetworkMode"] = s.NetworkMode
		}
	}
	for _, n := range c.Networks {
		name, aliases, _ := strings.Cut(n, ":")
		if name == "" {
			return fmt.Errorf("--network %q: expected NAME[:alias,alias]", n)
		}
		switch s.NetworkMode {
		case "host", "none":
			if len(s.Networks) == 0 {
				s.NetworkMode = name
				host["NetworkMode"] = name
			}
		}
		if strings.HasPrefix(s.NetworkMode, "container:") || s.NetworkMode == "host" {
			return fmt.Errorf("--network %s: the container uses the network of %s", name, s.NetworkMode)
		}
		ep := Doc{}
		if aliases != "" {
			var as []any
			for _, a := range strings.Split(aliases, ",") {
				if a = strings.TrimSpace(a); a != "" {
					as = append(as, a)
				}
			}
			ep["Aliases"] = as
		}
		replaced := false
		for i := range s.Networks {
			if s.Networks[i].Network == name {
				s.Networks[i].Config = ep
				replaced = true
			}
		}
		if !replaced {
			s.Networks = append(s.Networks, recreate.Endpoint{Network: name, Config: ep})
		}
	}
	return nil
}

var portRe = regexp.MustCompile(`^(\d+)(?:/(tcp|udp|sctp))?$`)

func portKey(p string) (string, error) {
	m := portRe.FindStringSubmatch(p)
	if m == nil {
		return "", fmt.Errorf("%q: expected a container port such as 80 or 53/udp", p)
	}
	proto := m[2]
	if proto == "" {
		proto = "tcp"
	}
	return m[1] + "/" + proto, nil
}

// parsePublish reads [ip:][host:]container[/proto] (IPv6 hosts in [brackets]).
func parsePublish(p string) (string, Doc, error) {
	ip := ""
	rest := p
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]:")
		if end < 0 {
			return "", nil, fmt.Errorf("--publish %q: unclosed [", p)
		}
		ip, rest = rest[1:end], rest[end+2:]
	}
	parts := strings.Split(rest, ":")
	var host, ctr string
	switch len(parts) {
	case 1:
		ctr = parts[0]
	case 2:
		host, ctr = parts[0], parts[1]
	case 3:
		if ip != "" {
			return "", nil, fmt.Errorf("--publish %q: too many parts", p)
		}
		ip, host, ctr = parts[0], parts[1], parts[2]
	default:
		return "", nil, fmt.Errorf("--publish %q: expected [ip:]host:container[/proto]", p)
	}
	key, err := portKey(ctr)
	if err != nil {
		return "", nil, fmt.Errorf("--publish %s", err)
	}
	if host != "" {
		if _, err := strconv.Atoi(strings.Split(host, "-")[0]); err != nil {
			return "", nil, fmt.Errorf("--publish %q: %q is not a port", p, host)
		}
	}
	return key, Doc{"HostIp": ip, "HostPort": host}, nil
}

// parseMount reads the --mount syntax (type, source, target, readonly and
// a few options).
func parseMount(m string) (Doc, error) {
	out := Doc{"Type": "volume"}
	for _, field := range strings.Split(m, ",") {
		k, v, hasV := strings.Cut(field, "=")
		switch strings.ToLower(k) {
		case "type":
			out["Type"] = v
		case "source", "src":
			out["Source"] = v
		case "target", "destination", "dst":
			out["Target"] = v
		case "readonly", "ro":
			out["ReadOnly"] = !hasV || v == "true" || v == "1"
		case "bind-propagation":
			out["BindOptions"] = Doc{"Propagation": v}
		case "volume-nocopy":
			out["VolumeOptions"] = Doc{"NoCopy": !hasV || v == "true" || v == "1"}
		case "tmpfs-size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("--mount %q: tmpfs-size is a number of bytes", m)
			}
			out["TmpfsOptions"] = Doc{"SizeBytes": n}
		default:
			return nil, fmt.Errorf("--mount %q: unknown option %q", m, k)
		}
	}
	if !strings.HasPrefix(str(out["Target"]), "/") {
		return nil, fmt.Errorf("--mount %q: target must be an absolute path", m)
	}
	return out, nil
}

func parseRestart(r string) (Doc, error) {
	name, max, _ := strings.Cut(r, ":")
	switch name {
	case "no", "always", "unless-stopped":
		if max != "" {
			return nil, fmt.Errorf("--restart %s: only on-failure takes a count", r)
		}
		return Doc{"Name": name, "MaximumRetryCount": 0}, nil
	case "on-failure":
		n := 0
		if max != "" {
			var err error
			if n, err = strconv.Atoi(max); err != nil || n < 0 {
				return nil, fmt.Errorf("--restart %s: the count must be a number", r)
			}
		}
		return Doc{"Name": name, "MaximumRetryCount": n}, nil
	}
	return nil, fmt.Errorf("--restart %q: expected no, always, unless-stopped or on-failure[:N]", r)
}

// removeMount takes away whatever is mounted at dst (bind, mount, tmpfs or
// an anonymous volume). It reports whether there was one.
func removeMount(host Doc, dst string) bool {
	found := false
	var binds []any
	for _, b := range list(host["Binds"]) {
		if parts := strings.Split(str(b), ":"); len(parts) >= 2 && parts[1] == dst {
			found = true
			continue
		}
		binds = append(binds, b)
	}
	if found {
		host["Binds"] = binds
	}
	var mounts []any
	removed := false
	for _, m := range list(host["Mounts"]) {
		if str(asDoc(m)["Target"]) == dst {
			removed = true
			continue
		}
		mounts = append(mounts, m)
	}
	if removed {
		host["Mounts"] = mounts
		found = true
	}
	if t := asDoc(host["Tmpfs"]); t != nil {
		if _, ok := t[dst]; ok {
			delete(t, dst)
			found = true
		}
	}
	return found
}

func doc(parent Doc, key string) Doc {
	d, ok := parent[key].(Doc)
	if !ok || d == nil {
		d = Doc{}
		parent[key] = d
	}
	return d
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asDoc(v any) Doc {
	d, _ := v.(Doc)
	return d
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// keys returns a map's keys in order.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
