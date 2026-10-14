package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/images"
)

func TestPlanBlocksDatabasesWithoutConnections(t *testing.T) {
	p := newProject(t, `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.up("db")
	p.psql("db", "postgres", "postgres", "create database archive")
	p.psql("db", "postgres", "postgres", "alter database archive allow_connections false")
	if r := p.plan("db", 18, ""); !contains(r.Blockers, "database archive does not accept connections") {
		t.Fatalf("blockers %q", r.Blockers)
	}
}

func TestPlanBlocksVolumeDriverOptions(t *testing.T) {
	p := newProject(t, `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
    driver_opts: {type: none, o: bind, device: $DIR/hostdata}
`)
	must(t, os.MkdirAll(filepath.Join(p.Dir, "hostdata"), 0o755))
	p.up("db")
	if r := p.plan("db", 18, ""); !contains(r.Blockers, "cannot yet make a matching new volume") {
		t.Fatalf("blockers %q", r.Blockers)
	}
}

// TestResolveUsesTheFilesTheServiceWasStartedWith starts the project with an
// extra file that moves the data to another volume.
func TestResolveUsesTheFilesTheServiceWasStartedWith(t *testing.T) {
	p := newProject(t, `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	prod := filepath.Join(p.Dir, "prod.yaml")
	must(t, os.WriteFile(prod, []byte("services:\n  db:\n    volumes: [prod:/var/lib/postgresql/data]\nvolumes:\n  prod:\n"), 0o644))
	base := filepath.Join(p.Dir, "compose.yaml")
	p.compose("-f", base, "-f", prod, "up", "-d")
	ctx := context.Background()
	id := strings.TrimSpace(p.compose("-f", base, "-f", prod, "ps", "-q", "db"))
	must(t, images.WaitReady(ctx, p.CLI, id, 2*time.Minute))
	// Only the base file: refused.
	_, err := Resolve(ctx, p.CLI, compose.Options{Name: p.Opt.Name, Files: []string{base}}, "db")
	if err == nil || !strings.Contains(err.Error(), "started with") {
		t.Fatalf("got %v", err)
	}
	// Nothing given: the files from the container are used.
	proj, err := Resolve(ctx, p.CLI, compose.Options{Name: p.Opt.Name}, "db")
	must(t, err)
	r, err := Build(ctx, Input{CLI: p.CLI, Project: proj, Service: "db", To: 18})
	must(t, err)
	if r.MountName != p.Opt.Name+"_prod" || !r.OK() {
		t.Fatalf("mount %s, blockers %q", r.MountName, r.Blockers)
	}
}
