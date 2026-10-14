// Package testkit starts throwaway Compose projects for tests. Tests using it
// run only with PGCARRY_INTEGRATION=1.
package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/images"
)

// Project is a Compose project in a temporary folder.
type Project struct {
	T   *testing.T
	CLI *docker.CLI
	Opt compose.Options
	Dir string
}

// New writes yaml ($DIR is replaced by the project folder) and removes the
// project and its volumes when the test ends.
func New(t *testing.T, yaml string) *Project {
	t.Helper()
	if os.Getenv("PGCARRY_INTEGRATION") == "" {
		t.Skip("set PGCARRY_INTEGRATION=1 to run tests against Docker")
	}
	b := make([]byte, 4)
	rand.Read(b)
	dir := t.TempDir()
	p := &Project{T: t, CLI: &docker.CLI{}, Dir: dir,
		Opt: compose.Options{Dir: dir, Name: "pgct" + hex.EncodeToString(b)}}
	yaml = strings.ReplaceAll(yaml, "$DIR", dir)
	Must(t, os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(yaml), 0o644))
	t.Cleanup(func() {
		ctx := context.Background()
		p.CLI.Run(ctx, "compose", "--project-directory", dir, "-p", p.Opt.Name, "down", "-v", "--remove-orphans", "-t", "1")
		// Volumes pgcarry made carry the project label.
		out, _ := p.CLI.Run(ctx, "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+p.Opt.Name)
		for _, v := range strings.Fields(out) {
			p.CLI.Run(ctx, "volume", "rm", "-f", v)
		}
		// Bind-mounted data is owned by the container's user; remove it
		// from inside a container so TempDir cleanup works.
		p.CLI.Run(ctx, "run", "--rm", "-v", dir+":/d", "--entrypoint", "sh", "postgres:18-alpine", "-c", "rm -rf /d/*")
	})
	return p
}

// Compose runs docker compose for the project.
func (p *Project) Compose(args ...string) string {
	p.T.Helper()
	full := append([]string{"compose", "--project-directory", p.Opt.Dir, "-p", p.Opt.Name}, args...)
	out, err := p.CLI.Run(context.Background(), full...)
	if err != nil {
		p.T.Fatalf("docker %s: %v", strings.Join(full, " "), err)
	}
	return out
}

// Up starts the project and waits for the database services.
func (p *Project) Up(services ...string) {
	p.T.Helper()
	p.Compose("up", "-d")
	for _, s := range services {
		id := strings.TrimSpace(p.Compose("ps", "-q", s))
		Must(p.T, images.WaitReady(context.Background(), p.CLI, id, 2*time.Minute))
	}
}

// SQL runs statements one by one in a service's database.
func (p *Project) SQL(service, user, db string, stmts ...string) {
	p.T.Helper()
	for _, st := range stmts {
		p.Compose("exec", "-T", service, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", st)
	}
}

// Load reads the project.
func (p *Project) Load() *compose.Project {
	p.T.Helper()
	proj, err := compose.Load(context.Background(), p.CLI, p.Opt)
	Must(p.T, err)
	return proj
}

// Must fails the test on an error.
func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
