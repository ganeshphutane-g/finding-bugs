package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/composehost/internal/protocol"
	"github.com/ganeshphutane-g/composehost/internal/steps"
)

// ComposeConfig runs "docker compose [-f FILE]... config --format json"
// (a variable for tests).
var ComposeConfig = func(files []string) ([]byte, error) {
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "config", "--format", "json")
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

type model struct {
	Name     string
	Services map[string]struct {
		Provider *struct {
			Type    string
			Options map[string][]string
		}
		DependsOn map[string]any `json:"depends_on"`
	}
}

// isComposehost says whether a provider type names composehost (on PATH,
// as a path, or as the Docker CLI plugin docker-composehost).
func isComposehost(typ string) bool {
	b := filepath.Base(typ)
	return b == "composehost" || b == "docker-composehost"
}

// Check shows each composehost service's steps, catches options Compose
// would drop without a word (it passes only the declared ones), and looks
// once at the waits.
func Check(args []string, stdout, stderr io.Writer, pending func(dir string, w steps.Waits) ([]string, error)) int {
	if pending == nil {
		pending = func(dir string, w steps.Waits) ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return steps.Pending(ctx, dir, w)
		}
	}
	var files []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-f" || a == "--file") && i+1 < len(args):
			files = append(files, args[i+1])
			i++
		case strings.HasPrefix(a, "--file="):
			files = append(files, strings.TrimPrefix(a, "--file="))
		default:
			fmt.Fprintf(stderr, "composehost check: unexpected %q (use -f FILE)\n", a)
			return 2
		}
	}
	dir, err := projectDir(files)
	if err != nil {
		fmt.Fprintln(stderr, "composehost:", err)
		return 1
	}
	b, err := ComposeConfig(files)
	if err != nil {
		fmt.Fprintln(stderr, "composehost:", err)
		return 1
	}
	var m model
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Fprintln(stderr, "composehost: reading docker compose config:", err)
		return 1
	}
	var names []string
	for name, s := range m.Services {
		if s.Provider != nil && isComposehost(s.Provider.Type) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintf(stdout, "project %s: no service has provider type composehost\n", m.Name)
		return 1
	}
	code := 0
	known := Metadata.Known()
	for _, name := range names {
		s := m.Services[name]
		fmt.Fprintf(stdout, "%s:\n", name)
		var users []string
		for other, o := range m.Services {
			if _, ok := o.DependsOn[name]; ok {
				users = append(users, other)
			}
		}
		sort.Strings(users)
		if len(users) == 0 {
			fmt.Fprintf(stdout, "  note:      no service depends on %s, so nothing waits for its steps\n", name)
		} else {
			fmt.Fprintf(stdout, "  before:    %s\n", strings.Join(users, ", "))
		}
		// Options no command declares are dropped by Compose: say so.
		var unknown []string
		for opt := range s.Provider.Options {
			if !known["up"][opt] && !known["down"][opt] && !known["stop"][opt] {
				unknown = append(unknown, opt)
			}
		}
		sort.Strings(unknown)
		for _, u := range unknown {
			fmt.Fprintf(stdout, "  error:     unknown option %q (Compose would drop it silently)\n", u)
			code = 1
		}
		for _, cmd := range []string{"up", "down", "stop"} {
			call := protocol.Call{Project: m.Name, Command: cmd, Service: name, Options: map[string][]string{}}
			for opt, v := range s.Provider.Options {
				if known[cmd][opt] {
					call.Options[opt] = v
				}
			}
			p, err := planOf(call)
			if err != nil {
				fmt.Fprintf(stdout, "  error:     %s: %v\n", cmd, err)
				code = 1
				continue
			}
			for _, l := range describe(cmd, p) {
				fmt.Fprintf(stdout, "  %-10s %s\n", l[0], l[1])
			}
			if cmd == "up" && !p.waits.Empty() {
				left, err := pending(dir, p.waits)
				switch {
				case err != nil:
					fmt.Fprintf(stdout, "  error:     %v\n", err)
					code = 1
				case len(left) > 0:
					fmt.Fprintf(stdout, "  now:       up would wait for %s\n", strings.Join(left, ", "))
					if code == 0 {
						code = 3
					}
				default:
					fmt.Fprintf(stdout, "  now:       the waits are met\n")
				}
			}
		}
	}
	return code
}

// composeFiles are the names Compose looks for, in its order.
var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// projectDir is where Compose runs the provider: the first file's
// directory (from -f, else COMPOSE_FILE), else the nearest directory, from
// here up, holding a compose file.
func projectDir(files []string) (string, error) {
	if len(files) == 0 && os.Getenv("COMPOSE_FILE") != "" {
		sep := os.Getenv("COMPOSE_PATH_SEPARATOR")
		if sep == "" {
			sep = string(os.PathListSeparator)
		}
		files = strings.Split(os.Getenv("COMPOSE_FILE"), sep)
	}
	if len(files) > 0 {
		if files[0] == "-" {
			return os.Getwd()
		}
		return filepath.Abs(filepath.Dir(files[0]))
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		for _, n := range composeFiles {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return d, nil
			}
		}
		if filepath.Dir(d) == d {
			// Compose itself will say there is no file.
			return wd, nil
		}
	}
}

// describe is a plan's steps, as label and text.
func describe(cmd string, p plan) [][2]string {
	var out [][2]string
	add := func(l, t string) { out = append(out, [2]string{l, t}) }
	switch cmd {
	case "down":
		if p.down != "" {
			add("on down:", p.down)
		}
		return out
	case "stop":
		if p.stop != "" {
			add("on stop:", p.stop)
		}
		return out
	}
	if len(p.mkdir) > 0 {
		t := "mkdir " + strings.Join(p.mkdir, " ")
		var with []string
		if p.owner != nil {
			with = append(with, fmt.Sprintf("owner %d:%d", p.owner.UID, p.owner.GID))
		}
		if p.mode != nil {
			with = append(with, fmt.Sprintf("mode %04o", uint32(*p.mode)))
		}
		if len(with) > 0 {
			t += " (" + strings.Join(with, ", ") + ")"
		}
		add("on up:", t)
	}
	var waits []string
	for _, m := range p.waits.Mounts {
		waits = append(waits, m+" mounted")
	}
	for _, x := range p.waits.Paths {
		waits = append(waits, x+" exists")
	}
	for _, a := range p.waits.TCP {
		waits = append(waits, a+" accepts connections")
	}
	if len(waits) > 0 {
		add("on up:", fmt.Sprintf("wait until %s (at most %s)", strings.Join(waits, ", "), p.waits.Timeout))
	}
	if p.run != "" {
		add("on up:", "run "+p.run)
	}
	if p.env != "" {
		add("on up:", "env from "+p.env)
	}
	return out
}
