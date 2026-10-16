// Package config is the kumafile file: what an instance's configuration
// looks like on disk, made from (and later compared with) what the server
// reports.
package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// KV is one field of an ordered object.
type KV struct {
	Key   string
	Value any
}

// Obj is an object whose fields keep their order in the file.
type Obj []KV

// Get returns a field's value.
func (o Obj) Get(k string) (any, bool) {
	for _, kv := range o {
		if kv.Key == k {
			return kv.Value, true
		}
	}
	return nil, false
}

// node turns a value into YAML.
func node(v any) *yaml.Node {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case Obj:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, kv := range x {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv.Key}, node(kv.Value))
		}
		return n
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := Obj{}
		for _, k := range keys {
			o = append(o, KV{k, x[k]})
		}
		return node(o)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			n.Content = append(n.Content, node(e))
		}
		if len(x) > 0 && allScalar(x) {
			n.Style = yaml.FlowStyle
		}
		return n
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		return node(a)
	case []Obj:
		a := make([]any, len(x))
		for i, o := range x {
			a[i] = o
		}
		return node(a)
	case string:
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
		if strings.Contains(x, "\n") {
			n.Style = yaml.LiteralStyle
		}
		return n
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: x.String()}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: x.String()}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(x)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(x, 10)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(x, 'g', -1, 64)}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(v)}
}

func allScalar(a []any) bool {
	for _, e := range a {
		switch e.(type) {
		case string, bool, json.Number, int, int64, float64:
			if s, ok := e.(string); ok && (len(s) > 40 || strings.Contains(s, "\n")) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
