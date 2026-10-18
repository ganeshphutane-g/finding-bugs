package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ParseEnv reads NAME=value lines (as docker compose's env_file and
// systemd's EnvironmentFile take them). Blank lines and # comments are
// skipped; a value may be in single or double quotes.
func ParseEnv(data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envName.MatchString(k) {
			return nil, fmt.Errorf("line %d: expected NAME=value", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// UpdateEnv sets variables in an env file, keeping its other lines, and
// writes it with mode 0600 in one step (a new file renamed over the old).
func UpdateEnv(path string, set map[string]string) error {
	// A link is written through, so its target is what changes.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var lines []string
	done := map[string]bool{}
	if len(old) > 0 {
		for _, line := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
			k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
			k = strings.TrimSpace(k)
			if v, mine := set[k]; ok && mine {
				line = k + "=" + v
				done[k] = true
			}
			lines = append(lines, line)
		}
	}
	var names []string
	for k := range set {
		if !done[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		lines = append(lines, k+"="+set[k])
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pushprobe-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	keepOwner(path, tmp.Name())
	return os.Rename(tmp.Name(), path)
}
