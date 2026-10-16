package config

import (
	"encoding/json"
	"strconv"
	"strings"
)

// isEmpty reports a value that means "nothing": null, "", [] or {}.
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if !isEmpty(e) {
				return false
			}
		}
		return true
	case Obj:
		return len(x) == 0
	}
	return false
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// same compares two values the way the server treats them: the empty
// values are all alike, numbers compare by value (also when one is written
// as text, as the web interface sends ports), and booleans equal 0 and 1.
func same(a, b any) bool {
	if isEmpty(a) || isEmpty(b) {
		return isEmpty(a) && isEmpty(b)
	}
	if na, ok := number(a); ok {
		if nb, ok := number(b); ok {
			return na == nb
		}
		if s, ok := b.(string); ok {
			nb, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			return err == nil && na == nb
		}
		if bb, ok := b.(bool); ok {
			return (na != 0) == bb && (na == 0 || na == 1)
		}
		return false
	}
	if _, ok := number(b); ok {
		return same(b, a)
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !same(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range x {
			if !same(v, y[k]) {
				return false
			}
		}
		for k, v := range y {
			if _, seen := x[k]; !seen && !isEmpty(v) {
				return false
			}
		}
		return true
	}
	return false
}
