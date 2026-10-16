package config

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is a kind of object in the file, in the order they are applied.
type Kind struct {
	Key   string // the file's key: "monitors"
	One   string // "monitor"
	ID    []string
	Label func(f map[string]any) string
}

// Kinds lists the file's sections, in dependency order (what others refer
// to comes first).
var Kinds = []Kind{
	{Key: "tags", One: "tag", ID: []string{"name"}},
	{Key: "notifications", One: "notification", ID: []string{"name"}},
	{Key: "proxies", One: "proxy", ID: []string{"protocol", "host", "port"}},
	{Key: "docker_hosts", One: "Docker host", ID: []string{"name"}},
	{Key: "remote_browsers", One: "remote browser", ID: []string{"name"}},
	{Key: "monitors", One: "monitor", ID: []string{"name"}},
	{Key: "status_pages", One: "status page", ID: []string{"slug"}},
	{Key: "maintenance", One: "maintenance window", ID: []string{"title"}},
}

// KindOf finds a kind by its file key.
func KindOf(key string) Kind {
	for _, k := range Kinds {
		if k.Key == key {
			return k
		}
	}
	return Kind{}
}

// Item is one object of a file: its identity and fields (references by
// name, values as written, ${NAME} expanded).
type Item struct {
	Kind   string
	Key    string // identity, lower case: what matches it to the instance
	Label  string // how it is shown
	Fields map[string]any
	// Secret lists fields whose value came from ${NAME} or that hold
	// credentials, so they are never printed.
	Secret map[string]bool
}

// Doc is a loaded file.
type Doc struct {
	Items map[string][]*Item // by kind key, in file order
}

// Get finds an item by kind and key.
func (d *Doc) Get(kind, key string) *Item {
	for _, it := range d.Items[kind] {
		if it.Key == key {
			return it
		}
	}
	return nil
}

var ref = regexp.MustCompile(`\$\$\{|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Lookup finds the value of a ${NAME} reference.
type Lookup func(name string) (string, bool)

// Load reads a file. Lookup gives ${NAME} values; a missing one is an
// error.
func Load(data []byte, lookup Lookup) (*Doc, error) {
	var raw map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("reading the file: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	var problems []string
	if v, ok := raw["version"]; ok && !same(v, 1) {
		problems = append(problems, fmt.Sprintf("version %v is not supported (this kumafile reads version 1)", v))
	}
	missing := map[string]bool{}
	d := &Doc{Items: map[string][]*Item{}}
	for key := range raw {
		if key != "version" && KindOf(key).Key == "" {
			problems = append(problems, fmt.Sprintf("unknown section %q", key))
		}
	}
	for _, k := range Kinds {
		v, ok := raw[k.Key]
		if !ok || v == nil {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: expected a list", k.Key))
			continue
		}
		seen := map[string]bool{}
		for i, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("%s item %d: expected fields (name: value)", k.Key, i+1))
				continue
			}
			it := &Item{Kind: k.Key, Fields: map[string]any{}, Secret: map[string]bool{}}
			for f, fv := range m {
				if sm, ok := fv.(map[string]any); ok && k.Key == "notifications" && f == "settings" {
					// Settings are secret one by one.
					out := map[string]any{}
					for sk, sv := range sm {
						x, secret := expand(sv, lookup, missing)
						out[sk] = x
						if secret {
							it.Secret["settings."+sk] = true
						}
					}
					it.Fields[f] = out
					continue
				}
				x, secret := expand(fv, lookup, missing)
				it.Fields[f] = x
				if secret || isSecretField(k.Key, f, x) {
					it.Secret[f] = true
				}
			}
			key, label, err := identity(k, it.Fields)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s item %d: %v", k.Key, i+1, err))
				continue
			}
			it.Key, it.Label = key, label
			if seen[key] {
				problems = append(problems, fmt.Sprintf("%s: %s appears twice", k.Key, label))
				continue
			}
			seen[key] = true
			d.Items[k.Key] = append(d.Items[k.Key], it)
		}
	}
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		problems = append(problems, fmt.Sprintf("no value for %s: set them in the environment or the secrets file", strings.Join(names, ", ")))
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return d, nil
}

// expand fills ${NAME} references in every text; $${ stands for ${.
func expand(v any, lookup Lookup, missing map[string]bool) (any, bool) {
	switch x := v.(type) {
	case string:
		secret := false
		out := ref.ReplaceAllStringFunc(x, func(m string) string {
			if m == "$${" {
				return "${"
			}
			name := m[2 : len(m)-1]
			secret = true
			val, ok := lookup(name)
			if !ok {
				missing[name] = true
			}
			return val
		})
		return out, secret
	case []any:
		out := make([]any, len(x))
		secret := false
		for i, e := range x {
			var s bool
			out[i], s = expand(e, lookup, missing)
			secret = secret || s
		}
		return out, secret
	case map[string]any:
		out := map[string]any{}
		secret := false
		for k, e := range x {
			var s bool
			out[k], s = expand(e, lookup, missing)
			secret = secret || s
		}
		return out, secret
	}
	return v, false
}

// isSecretField reports fields that hold credentials whatever their value.
func isSecretField(kind, field string, v any) bool {
	switch kind {
	case "monitors":
		if monitorSecrets[field] {
			return true
		}
		if s, ok := v.(string); ok && ((field == "headers" && secretHeader.MatchString(s)) || (field == "url" && userinfo.MatchString(s))) {
			return true
		}
	case "proxies":
		return field == "password"
	case "remote_browsers":
		return field == "url"
	}
	return false
}

func identity(k Kind, f map[string]any) (key, label string, err error) {
	str := func(name string) (string, error) {
		v, ok := f[name]
		if !ok || isEmpty(v) {
			return "", fmt.Errorf("%s is missing", name)
		}
		switch x := v.(type) {
		case string:
			return x, nil
		case int, int64, float64, uint64:
			return fmt.Sprint(x), nil
		}
		return "", fmt.Errorf("%s must be text", name)
	}
	switch k.Key {
	case "proxies":
		proto, err := str("protocol")
		if err != nil {
			return "", "", err
		}
		host, err := str("host")
		if err != nil {
			return "", "", err
		}
		port, err := str("port")
		if err != nil {
			return "", "", err
		}
		label = fmt.Sprintf("%s://%s:%s", proto, host, port)
		return strings.ToLower(label), label, nil
	case "monitors":
		name, err := str("name")
		if err != nil {
			return "", "", err
		}
		if strings.Contains(name, PathSep) {
			return "", "", fmt.Errorf("monitor %q: names cannot contain %q (it separates groups)", name, PathSep)
		}
		if _, err := str("type"); err != nil {
			return "", "", fmt.Errorf("monitor %q: %v", name, err)
		}
		label = name
		if g, ok := f["group"]; ok && !isEmpty(g) {
			gs, ok := g.(string)
			if !ok {
				return "", "", fmt.Errorf("monitor %q: group must be text such as \"Web\" or \"Web / API\"", name)
			}
			label = gs + PathSep + name
		}
		return strings.ToLower(label), label, nil
	}
	v, err := str(k.ID[0])
	if err != nil {
		return "", "", err
	}
	if k.Key == "status_pages" {
		return strings.ToLower(v), v, nil
	}
	return strings.ToLower(v), v, nil
}
