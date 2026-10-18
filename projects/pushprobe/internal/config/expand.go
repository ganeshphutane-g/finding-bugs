package config

import (
	"reflect"
	"regexp"
	"strings"
)

// ref is a ${NAME} reference; $${ stands for a literal ${.
var ref = regexp.MustCompile(`\$\$\{|\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// envName is a variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Allowed reports the variables a file may read. A file cannot read any
// other variable, so a change to it cannot send, say, your cloud credentials
// to a server it names.
func Allowed(name string) bool {
	for _, p := range []string{"PUSHPROBE_", "KUMA_", "GATUS_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// expandAll fills references in every text of the file. For ForCheck, where
// results go is left alone, so its variables need not be set.
func expandAll(c *Config, lookup Lookup, mode Mode, missing map[string]bool) {
	fill := func(s string) string {
		return ref.ReplaceAllStringFunc(s, func(m string) string {
			if m == "$${" {
				return "${"
			}
			name := m[2 : len(m)-1]
			if !Allowed(name) {
				missing[name+" (only PUSHPROBE_*, KUMA_* and GATUS_* variables can be used)"] = true
				return ""
			}
			v, ok := lookup(name)
			if !ok {
				missing[name] = true
			}
			return v
		})
	}
	switch mode {
	case ForRun:
		walk(reflect.ValueOf(&c.Targets).Elem(), fill)
	case ForSync:
		if c.Targets.Kuma != nil {
			walk(reflect.ValueOf(c.Targets.Kuma).Elem(), fill)
		}
	}
	for i := range c.Checks {
		ch := &c.Checks[i]
		if ch.Kuma != nil {
			ch.Kuma.Ref = ch.Kuma.PushToken
		}
		kuma, gatus := ch.Kuma, ch.Gatus
		switch mode {
		case ForCheck:
			ch.Kuma, ch.Gatus = nil, nil
		case ForSync:
			ch.Gatus = nil
			// An unset ${NAME} as the whole push_token is for sync to make.
			if v := kuma.tokenVarIfAny(); v != "" && Allowed(v) {
				if _, set := lookup(v); !set {
					kuma.PushToken = ""
				}
			}
		}
		walk(reflect.ValueOf(ch).Elem(), fill)
		ch.Kuma, ch.Gatus = kuma, gatus
	}
}

// walk applies fill to every string in v (struct fields, pointers, slices and
// map values).
func walk(v reflect.Value, fill func(string) string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walk(v.Elem(), fill)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			// (Fields not read from the file, such as Ref, are left alone.)
			if f := v.Type().Field(i); f.IsExported() && f.Tag.Get("yaml") != "-" {
				walk(v.Field(i), fill)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			walk(v.Index(i), fill)
		}
	case reflect.Map:
		if v.Type().Elem().Kind() == reflect.String {
			for _, k := range v.MapKeys() {
				v.SetMapIndex(k, reflect.ValueOf(fill(v.MapIndex(k).String())).Convert(v.Type().Elem()))
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(fill(v.String()))
		}
	}
}

// tokenVarIfAny is TokenVar for a route that may be nil.
func (k *KumaRoute) tokenVarIfAny() string {
	if k == nil {
		return ""
	}
	return k.TokenVar()
}
