package edit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/docker-edit/internal/recreate"
)

// Line is one setting that differs.
type Line struct {
	Op       string // "+", "-" or "~"
	Key      string // e.g. "port 80/tcp", "env FOO"
	From, To string
}

func (l Line) String() string {
	from, to := display(l.From), display(l.To)
	switch l.Op {
	case "+":
		return fmt.Sprintf("  + %s: %s", l.Key, to)
	case "-":
		return fmt.Sprintf("  - %s (was %s)", l.Key, from)
	}
	return fmt.Sprintf("  ~ %s: %s -> %s", l.Key, from, to)
}

// secretName matches variable names whose values are not shown.
var secretName = regexp.MustCompile(`(?i)(pass|secret|token|key|auth|credential|cookie|session|private)`)

// Settings is a spec as named settings and short values, for comparing.
func Settings(s *recreate.Spec) map[string]string {
	out := map[string]string{}
	cfg, host := s.Config, s.Host
	out["image"] = str(cfg["Image"])
	for _, e := range list(cfg["Env"]) {
		k, v, _ := strings.Cut(str(e), "=")
		if secretName.MatchString(k) {
			v = hidden + hash(v)
		}
		out["env "+k] = v
	}
	for k, v := range asDoc(cfg["Labels"]) {
		out["label "+k] = fmt.Sprint(v)
	}
	for port, bs := range asDoc(host["PortBindings"]) {
		var parts []string
		for _, b := range list(bs) {
			bd := asDoc(b)
			hp := str(bd["HostPort"])
			if hp == "" {
				hp = "(any)"
			}
			if ip := str(bd["HostIp"]); ip != "" {
				hp = ip + ":" + hp
			}
			parts = append(parts, hp)
		}
		sort.Strings(parts)
		out["port "+port] = strings.Join(parts, ", ")
	}
	for _, b := range list(host["Binds"]) {
		parts := strings.Split(str(b), ":")
		if len(parts) >= 2 {
			desc := parts[0]
			if len(parts) > 2 {
				desc += " (" + strings.Join(parts[2:], ",") + ")"
			}
			out["mount "+parts[1]] = desc
		}
	}
	for _, m := range list(host["Mounts"]) {
		md := asDoc(m)
		desc := str(md["Type"]) + " " + str(md["Source"])
		if md["ReadOnly"] == true {
			desc += " (ro)"
		}
		out["mount "+str(md["Target"])] = strings.TrimSpace(desc)
	}
	for t, o := range asDoc(host["Tmpfs"]) {
		out["mount "+t] = strings.TrimSpace("tmpfs " + fmt.Sprint(o))
	}
	if rp := asDoc(host["RestartPolicy"]); rp != nil {
		p := str(rp["Name"])
		if p == "" {
			p = "no"
		}
		if n := fmt.Sprint(rp["MaximumRetryCount"]); p == "on-failure" && n != "0" && n != "<nil>" {
			p += ":" + n
		}
		out["restart"] = p
	}
	out["network mode"] = s.NetworkMode
	for _, ep := range s.Networks {
		desc := "connected"
		if as := list(ep.Config["Aliases"]); len(as) > 0 {
			var names []string
			for _, a := range as {
				names = append(names, str(a))
			}
			desc = "aliases " + strings.Join(names, ", ")
		}
		if ip := asDoc(ep.Config["IPAMConfig"]); ip != nil {
			if a := str(ip["IPv4Address"]); a != "" {
				desc += ", ip " + a
			}
		}
		out["network "+ep.Network] = desc
	}
	// Everything else, so no change goes unseen.
	for k, v := range cfg {
		switch k {
		case "Image", "Env", "Labels":
			continue
		case "ExposedPorts":
			// A published port is exposed anyway; the port lines show it.
			rest := Doc{}
			for p, x := range asDoc(v) {
				if _, ok := asDoc(host["PortBindings"])[p]; !ok {
					rest[p] = x
				}
			}
			v = rest
		}
		out["config "+k] = jsonText(v)
	}
	for k, v := range host {
		switch k {
		case "PortBindings", "Binds", "Mounts", "Tmpfs", "RestartPolicy", "NetworkMode":
			continue
		}
		out["host "+k] = jsonText(v)
	}
	return out
}

// Diff lists the settings that differ between two specs.
func Diff(before, after *recreate.Spec) []Line {
	a, b := Settings(before), Settings(after)
	var out []Line
	for _, k := range keys(a) {
		bv, ok := b[k]
		switch {
		case !ok && !blank(a[k]):
			out = append(out, Line{Op: "-", Key: k, From: a[k]})
		case ok && bv != a[k]:
			out = append(out, Line{Op: "~", Key: k, From: a[k], To: bv})
		}
	}
	for _, k := range keys(b) {
		if _, ok := a[k]; !ok && !blank(b[k]) {
			out = append(out, Line{Op: "+", Key: k, To: b[k]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func blank(v string) bool { return v == "" || v == "null" || v == "{}" || v == "[]" }

// hidden is shown for a secret value. The value's hash follows it, after a
// NUL, so a change is still noticed; the hash is not shown.
const hidden = "(hidden)\x00"

// hashKey is new on every run: a hash cannot be checked against guesses.
var hashKey = func() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}()

func hash(s string) string {
	m := hmac.New(sha256.New, hashKey)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// display is a value as shown.
func display(v string) string {
	v, _, _ = strings.Cut(v, "\x00")
	return v
}

// Effective is the spec as the new container will have it: what the spec
// leaves to its image is filled from the image (as Docker does).
func Effective(s *recreate.Spec, image Doc) *recreate.Spec {
	cfg := Doc{}
	for k, v := range s.Config {
		cfg[k] = v
	}
	out := *s
	out.Config = cfg
	img := asDoc(image["Config"])
	if img == nil {
		return &out
	}
	// Environment: the image's, with the container's on top.
	set := map[string]bool{}
	var env []any
	for _, e := range list(s.Config["Env"]) {
		k, _, _ := strings.Cut(str(e), "=")
		set[k] = true
		env = append(env, e)
	}
	for _, e := range list(img["Env"]) {
		if k, _, _ := strings.Cut(str(e), "="); !set[k] {
			env = append(env, e)
		}
	}
	cfg["Env"] = env
	labels := Doc{}
	for k, v := range asDoc(img["Labels"]) {
		labels[k] = v
	}
	for k, v := range asDoc(s.Config["Labels"]) {
		labels[k] = v
	}
	cfg["Labels"] = labels
	for _, k := range []string{"Cmd", "Entrypoint", "WorkingDir", "User", "Healthcheck", "StopSignal", "Shell", "OnBuild"} {
		if _, ok := cfg[k]; !ok {
			if v, ok := img[k]; ok {
				cfg[k] = v
			}
		}
	}
	for _, k := range []string{"ExposedPorts", "Volumes"} {
		merged := Doc{}
		for p, v := range asDoc(img[k]) {
			merged[p] = v
		}
		for p, v := range asDoc(s.Config[k]) {
			merged[p] = v
		}
		if len(merged) > 0 {
			cfg[k] = merged
		}
	}
	return &out
}
