// Package compose reads a Docker Compose project the way Compose itself
// resolves it (docker compose config), and the state of its containers.
package compose

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/docker"
)

// Options select a project, as docker compose's own flags do.
type Options struct {
	Dir      string   `json:"dir,omitempty"`       // --project-directory
	Files    []string `json:"files,omitempty"`     // -f
	Name     string   `json:"name,omitempty"`      // -p
	EnvFiles []string `json:"env_files,omitempty"` // --env-file
}

// Args are the docker compose flags selecting the project.
func (o Options) Args() []string { return o.args() }

func (o Options) args() []string {
	var a []string
	if o.Dir != "" {
		a = append(a, "--project-directory", o.Dir)
	}
	for _, f := range o.Files {
		a = append(a, "-f", f)
	}
	if o.Name != "" {
		a = append(a, "-p", o.Name)
	}
	for _, f := range o.EnvFiles {
		a = append(a, "--env-file", f)
	}
	return a
}

// Project is a resolved Compose project.
type Project struct {
	Name     string             `json:"name"`
	Services map[string]Service `json:"services"`
	Volumes  map[string]Volume  `json:"volumes"`
	Options  Options            `json:"-"`
}

// Service is one resolved service.
type Service struct {
	Name        string                `json:"-"`
	Image       string                `json:"image"`
	Environment map[string]*string    `json:"environment"`
	Volumes     []Mount               `json:"volumes"`
	DependsOn   map[string]Dependency `json:"depends_on"`
	Networks    map[string]any        `json:"networks"`
	NetworkMode string                `json:"network_mode"`
	User        string                `json:"user"`
	Command     any                   `json:"command"`
}

// Dependency is a depends_on entry.
type Dependency struct {
	Condition string `json:"condition"`
}

// Mount is a service volume or bind mount.
type Mount struct {
	Type     string `json:"type"` // volume, bind, tmpfs
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// Volume is a top-level volume.
type Volume struct {
	Name     string `json:"name"`
	External bool   `json:"external"`
	Driver   string `json:"driver"`
}

// Env returns an environment value of the service ("" when unset).
func (s Service) Env(key string) string {
	if v := s.Environment[key]; v != nil {
		return *v
	}
	return ""
}

// Load resolves the project.
func Load(ctx context.Context, cli *docker.CLI, opt Options) (*Project, error) {
	out, err := cli.Run(ctx, append(append([]string{"compose"}, opt.args()...), "config", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("reading the Compose project: %w", err)
	}
	var p Project
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return nil, fmt.Errorf("reading docker compose config output: %w", err)
	}
	for name, s := range p.Services {
		s.Name = name
		p.Services[name] = s
	}
	p.Options = opt
	if opt.Name == "" {
		p.Options.Name = p.Name
	}
	return &p, nil
}

// VolumeName is the Docker volume a service mount of type "volume" uses.
func (p *Project) VolumeName(m Mount) string {
	if v, ok := p.Volumes[m.Source]; ok && v.Name != "" {
		return v.Name
	}
	return m.Source
}

// ServiceNames lists the services, sorted.
func (p *Project) ServiceNames() []string {
	var names []string
	for n := range p.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Dependents lists the services that depend on name, directly or through
// other services, sorted.
func (p *Project) Dependents(name string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(target string) {
		for _, n := range p.ServiceNames() {
			if _, ok := p.Services[n].DependsOn[target]; ok && !seen[n] && n != name {
				seen[n] = true
				walk(n)
			}
		}
	}
	walk(name)
	var out []string
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Container is one container of the project.
type Container struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// Containers lists the project's containers, running or not.
func Containers(ctx context.Context, cli *docker.CLI, opt Options) ([]Container, error) {
	out, err := cli.Run(ctx, append(append([]string{"compose"}, opt.args()...), "ps", "--all", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	var cs []Container
	if strings.HasPrefix(out, "[") {
		err = json.Unmarshal([]byte(out), &cs)
		return cs, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			var c Container
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return nil, fmt.Errorf("reading docker compose ps output: %w", err)
			}
			cs = append(cs, c)
		}
	}
	return cs, sc.Err()
}
