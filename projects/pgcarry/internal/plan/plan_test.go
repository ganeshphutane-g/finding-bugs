package plan

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

// These tests start real Compose projects. They run with
// PGCARRY_INTEGRATION=1 and need Docker with the Compose plugin.

type project struct {
	t   *testing.T
	cli *docker.CLI
	opt compose.Options
	dir string
}

func newProject(t *testing.T, yaml string) *project {
	t.Helper()
	if os.Getenv("PGCARRY_INTEGRATION") == "" {
		t.Skip("set PGCARRY_INTEGRATION=1 to run tests against Docker")
	}
	b := make([]byte, 4)
	rand.Read(b)
	dir := t.TempDir()
	p := &project{t: t, cli: &docker.CLI{}, dir: dir,
		opt: compose.Options{Dir: dir, Name: "pgct" + hex.EncodeToString(b)}}
	yaml = strings.ReplaceAll(yaml, "$DIR", dir)
	must(t, os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(yaml), 0o644))
	t.Cleanup(func() {
		p.compose("down", "-v", "--remove-orphans", "-t", "1")
	})
	return p
}

func (p *project) compose(args ...string) string {
	p.t.Helper()
	full := append([]string{"compose", "--project-directory", p.opt.Dir, "-p", p.opt.Name}, args...)
	out, err := p.cli.Run(context.Background(), full...)
	if err != nil {
		p.t.Fatalf("docker %s: %v", strings.Join(full, " "), err)
	}
	return out
}

// up starts the project and waits for the database service to be ready.
func (p *project) up(service string) {
	p.t.Helper()
	p.compose("up", "-d")
	id := strings.TrimSpace(p.compose("ps", "-q", service))
	must(p.t, images.WaitReady(context.Background(), p.cli, id, 2*time.Minute))
}

func (p *project) psql(service, user, db, sql string) {
	p.t.Helper()
	p.compose("exec", "-T", service, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", sql)
}

func (p *project) plan(service string, to int, image string) *Report {
	p.t.Helper()
	proj, err := compose.Load(context.Background(), p.cli, p.opt)
	must(p.t, err)
	r, err := Build(context.Background(), Input{CLI: p.cli, Project: proj, Service: service, To: to, Image: image})
	must(p.t, err)
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func TestPlanOfficialImage(t *testing.T) {
	p := newProject(t, `
services:
  web:
    image: postgres:14
    entrypoint: ["sleep", "infinity"]
    depends_on: [db]
  db:
    image: postgres:14
    environment: {POSTGRES_USER: app, POSTGRES_PASSWORD: pw, POSTGRES_DB: appdb}
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
`)
	p.up("db")
	p.psql("db", "app", "appdb", "create table t as select generate_series(1, 1000) i")
	p.psql("db", "app", "appdb", "alter system set work_mem = '8MB'")
	p.compose("exec", "-T", "db", "sh", "-c", `echo "hostssl appdb app 10.0.0.0/8 scram-sha-256" >> "$PGDATA/pg_hba.conf"`)
	p.compose("restart", "db")
	p.up("db")
	r := p.plan("", 18, "")
	if !r.OK() {
		t.Fatalf("blocked: %q", r.Blockers)
	}
	if r.Service != "db" || r.Cluster.Major() != 14 || r.FileMajor != 14 || r.Target.Major() != 18 {
		t.Fatalf("report: %+v", r)
	}
	if r.MountType != "volume" || r.MountName != p.opt.Name+"_pgdata" || r.TargetMount != "/var/lib/postgresql" || r.TargetData != "/var/lib/postgresql/18/docker" {
		t.Fatalf("layout: %s %s -> %s %s", r.MountType, r.MountName, r.TargetMount, r.TargetData)
	}
	if len(r.Dependents) != 1 || r.Dependents[0] != "web" {
		t.Fatalf("dependents %q", r.Dependents)
	}
	if !contains(r.Changes, "work_mem=8192kB") || len(r.CustomSettings) != 1 {
		t.Fatalf("custom settings not found: %+v %q", r.CustomSettings, r.Changes)
	}
	if len(r.CustomHBA) != 1 || !strings.HasPrefix(r.CustomHBA[0], "hostssl appdb") {
		t.Fatalf("custom hba: %q", r.CustomHBA)
	}
	if !contains(r.Changes, "checksums are turned on") {
		t.Fatalf("changes: %q", r.Changes)
	}
}

func TestPlanBlocksMissingExtension(t *testing.T) {
	p := newProject(t, `
services:
  db:
    image: pgvector/pgvector:pg16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.up("db")
	p.psql("db", "postgres", "postgres", "create extension vector")
	// The plain image has no pgvector.
	r := p.plan("db", 18, "postgres:18")
	if !contains(r.Blockers, "extension vector") {
		t.Fatalf("missing extension not reported: %q", r.Blockers)
	}
	// The same family's newer image has it.
	r = p.plan("db", 18, "")
	if r.TargetImage != "pgvector/pgvector:pg18" || !r.OK() {
		t.Fatalf("target %s, blockers %q", r.TargetImage, r.Blockers)
	}
	if !contains(r.Changes, "extension vector in postgres goes from") && !contains(r.Changes, "image becomes") {
		t.Fatalf("changes: %q", r.Changes)
	}
}

func TestPlanBlocksLocaleTheNewImageLacks(t *testing.T) {
	p := newProject(t, `
services:
  db:
    image: postgres:14-alpine
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.up("db")
	// musl accepts any locale name; Debian-based images only have a few.
	p.psql("db", "postgres", "postgres", "create database shop template template0 lc_collate 'de_DE.UTF-8' lc_ctype 'de_DE.UTF-8'")
	r := p.plan("db", 18, "postgres:18")
	if !contains(r.Blockers, "shop use locale de_DE.UTF-8") {
		t.Fatalf("locale not reported: %q", r.Blockers)
	}
	r = p.plan("db", 18, "")
	if r.TargetImage != "postgres:18-alpine" || !r.OK() {
		t.Fatalf("alpine target: %s %q", r.TargetImage, r.Blockers)
	}
}

func TestPlanBlocksUnsafeSetups(t *testing.T) {
	p := newProject(t, `
services:
  anon:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
  stopped:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
  bound:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: ["$DIR/pg:/var/lib/postgresql/data"]
volumes:
  data:
`)
	p.compose("up", "-d", "anon", "bound")
	for _, s := range []string{"anon", "bound"} {
		id := strings.TrimSpace(p.compose("ps", "-q", s))
		must(t, images.WaitReady(context.Background(), p.cli, id, 2*time.Minute))
	}
	proj, err := compose.Load(context.Background(), p.cli, p.opt)
	must(t, err)
	if _, err := FindService(proj, ""); err == nil || !strings.Contains(err.Error(), "several services") {
		t.Fatalf("FindService: %v", err)
	}
	if r := p.plan("anon", 18, ""); !contains(r.Blockers, "anonymous volume") {
		t.Fatalf("anon: %q", r.Blockers)
	}
	if r := p.plan("stopped", 18, ""); !contains(r.Blockers, "not running") {
		t.Fatalf("stopped: %q", r.Blockers)
	}
	r := p.plan("bound", 18, "")
	if !r.OK() || r.MountType != "bind" || !contains(r.Changes, filepath.Join(p.dir, "pg-pg18")) {
		t.Fatalf("bound: %s %q %q", r.MountType, r.Blockers, r.Changes)
	}
	if r := p.plan("bound", 16, ""); !contains(r.Blockers, "not newer") {
		t.Fatalf("same version: %q", r.Blockers)
	}
}
