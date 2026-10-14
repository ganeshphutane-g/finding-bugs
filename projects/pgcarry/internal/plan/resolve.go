package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
)

// Resolve loads the project the way the Postgres service was actually
// started: the same folder, files and project name, read from the running
// container's labels. Files given by the user must match them; otherwise
// pgcarry could copy one set of data and switch Compose to another.
func Resolve(ctx context.Context, cli *docker.CLI, opt compose.Options, service string) (*compose.Project, error) {
	// Only a project name: find its folder and files from its containers,
	// so pgcarry works from any folder.
	if opt.Name != "" && opt.Dir == "" && len(opt.Files) == 0 {
		out, err := cli.Run(ctx, "ps", "-q", "--filter", "label=com.docker.compose.project="+opt.Name)
		if err == nil {
			if ids := strings.Fields(out); len(ids) > 0 {
				if started, ok := startedWith(ctx, cli, ids[0]); ok {
					opt = started
				}
			}
		}
	}
	proj, err := compose.Load(ctx, cli, opt)
	if err != nil {
		return nil, err
	}
	svc, err := FindService(proj, service)
	if err != nil {
		return nil, err
	}
	cs, err := compose.Containers(ctx, cli, proj.Options)
	if err != nil {
		return nil, err
	}
	var id string
	for _, c := range cs {
		if c.Service == svc.Name && c.State == "running" {
			id = c.ID
		}
	}
	if id == "" {
		return proj, nil // Build reports that it is not running
	}
	started, ok := startedWith(ctx, cli, id)
	if !ok {
		return proj, nil
	}
	if len(opt.Files) > 0 && !sameFiles(opt.Files, started.Files) {
		return nil, fmt.Errorf("the %s service was started with %s; give the same files with -f (or none) so pgcarry changes what is really running", svc.Name, strings.Join(started.Files, ", "))
	}
	if opt.Dir != "" && filepath.Clean(opt.Dir) != filepath.Clean(started.Dir) {
		return nil, fmt.Errorf("the %s service was started from %s, not %s", svc.Name, started.Dir, opt.Dir)
	}
	return compose.Load(ctx, cli, started)
}

func sameFiles(a, b []string) bool {
	norm := func(x []string) []string {
		var out []string
		for _, f := range x {
			if abs, err := filepath.Abs(f); err == nil {
				f = abs
			}
			if real, err := filepath.EvalSymlinks(f); err == nil {
				f = real
			}
			out = append(out, f)
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(norm(a), "\n") == strings.Join(norm(b), "\n")
}

// actualMount is what the running container really has mounted at target:
// a volume name or a host folder.
func actualMount(ctx context.Context, cli *docker.CLI, container, target string) (kind, source string, err error) {
	out, err := cli.Run(ctx, "inspect", "-f", "{{json .Mounts}}", container)
	if err != nil {
		return "", "", err
	}
	var mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal([]byte(out), &mounts); err != nil {
		return "", "", err
	}
	for _, m := range mounts {
		if filepath.Clean(m.Destination) == filepath.Clean(target) {
			if m.Type == "volume" {
				return "volume", m.Name, nil
			}
			return m.Type, m.Source, nil
		}
	}
	return "", "", nil
}

// startedWith reads the folder, files and project name a container was
// started from, out of its Compose labels.
func startedWith(ctx context.Context, cli *docker.CLI, id string) (compose.Options, bool) {
	out, err := cli.Run(ctx, "inspect", "-f", "{{json .Config.Labels}}", id)
	if err != nil {
		return compose.Options{}, false
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		return compose.Options{}, false
	}
	started := compose.Options{
		Dir:  labels["com.docker.compose.project.working_dir"],
		Name: labels["com.docker.compose.project"],
	}
	if f := labels["com.docker.compose.project.config_files"]; f != "" {
		started.Files = strings.Split(f, ",")
	}
	return started, started.Dir != "" && len(started.Files) > 0
}
