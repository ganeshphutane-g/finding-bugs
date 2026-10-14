package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/pgcarry/internal/plan"
	"github.com/ganeshphutane-g/pgcarry/internal/testkit"
)

func upgradeNow(t *testing.T, p *testkit.Project, service string, to int, hook func(string) error) (*Manifest, string, error) {
	t.Helper()
	proj := p.Load()
	r, err := plan.Build(context.Background(), plan.Input{CLI: p.CLI, Project: proj, Service: service, To: to})
	testkit.Must(t, err)
	if !r.OK() {
		t.Fatalf("plan blocked: %q", r.Blockers)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	var log strings.Builder
	m, err := Run(context.Background(), Options{CLI: p.CLI, Project: proj, Plan: r, BackupDir: backup,
		Switch: true, Log: func(s string) { log.WriteString(s + "\n") }, hook: hook})
	if err != nil {
		t.Logf("log:\n%s", log.String())
	}
	return m, backup, err
}

func scalar(t *testing.T, p *testkit.Project, service, user, db, sql string) string {
	t.Helper()
	return strings.TrimSpace(p.Compose("exec", "-T", service, "psql", "-X", "-At", "-U", user, "-d", db, "-c", sql))
}

func TestSwitchRollbackAndCleanup(t *testing.T) {
	p := testkit.New(t, `# the app
services:
  web:
    image: postgres:14
    entrypoint: ["sleep", "infinity"]
    stop_grace_period: 1s
    depends_on: [db]
  db:
    image: postgres:14 # keep me
    environment: {POSTGRES_PASSWORD: pw}
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select g from generate_series(1, 777) g")
	file := filepath.Join(p.Dir, "compose.yaml")
	orig, _ := os.ReadFile(file)

	m, backup, err := upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	if !m.Switched {
		t.Fatal("not switched")
	}
	now, _ := os.ReadFile(file)
	if !strings.Contains(string(now), "image: postgres:18 # keep me") || !strings.Contains(string(now), "- pgdata_pg18:/var/lib/postgresql\n") {
		t.Fatalf("compose file:\n%s", now)
	}
	if v := scalar(t, p, "db", "postgres", "postgres", "show server_version_num"); !strings.HasPrefix(v, "18") {
		t.Fatalf("serving %s", v)
	}
	if n := scalar(t, p, "db", "postgres", "postgres", "select count(*) from t"); n != "777" {
		t.Fatalf("rows %s", n)
	}
	if got := states(p); !strings.Contains(got, "web=running") {
		t.Fatalf("web not started again: %s", got)
	}
	// A write after the switch is not carried back by a rollback.
	p.SQL("db", "postgres", "postgres", "insert into t values (0)")

	testkit.Must(t, Rollback(context.Background(), p.CLI, backup))
	if back, _ := os.ReadFile(file); string(back) != string(orig) {
		t.Fatalf("compose file not restored:\n%s", back)
	}
	if v := scalar(t, p, "db", "postgres", "postgres", "show server_version_num"); !strings.HasPrefix(v, "14") {
		t.Fatalf("serving %s after rollback", v)
	}
	if n := scalar(t, p, "db", "postgres", "postgres", "select count(*) from t"); n != "777" {
		t.Fatalf("rows %s after rollback", n)
	}
	if err := Rollback(context.Background(), p.CLI, backup); err == nil {
		t.Fatal("a second rollback should be refused")
	}
	if err := Cleanup(context.Background(), p.CLI, backup); err == nil {
		t.Fatal("cleanup after a rollback should be refused")
	}

	// Upgrade again (the earlier copy has to go first), then clean up.
	if _, _, err := upgradeNow(t, p, "db", 18, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected the leftover volume to be refused, got %v", err)
	}
	p.CLI.Run(context.Background(), "volume", "rm", p.Opt.Name+"_pgdata_pg18")
	_, backup, err = upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	testkit.Must(t, Cleanup(context.Background(), p.CLI, backup))
	if _, err := p.CLI.Run(context.Background(), "volume", "inspect", p.Opt.Name+"_pgdata"); err == nil {
		t.Fatal("the old volume is still there after cleanup")
	}
	if n := scalar(t, p, "db", "postgres", "postgres", "select count(*) from t"); n != "777" {
		t.Fatalf("rows %s after cleanup", n)
	}
	if err := Rollback(context.Background(), p.CLI, backup); err == nil {
		t.Fatal("rollback after cleanup should be refused")
	}
}

func TestSwitchBindMountWithOverrideFile(t *testing.T) {
	p := testkit.New(t, `services:
  db:
    image: postgres:14
    environment: {POSTGRES_PASSWORD: pw}
    volumes: ["./pgdata:/var/lib/postgresql/data"]
`)
	testkit.Must(t, os.WriteFile(filepath.Join(p.Dir, "compose.override.yaml"), []byte("services:\n  db:\n    image: postgres:14-alpine\n"), 0o644))
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t (x text)", "insert into t values ('kept')")
	m, _, err := upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	if m.NewData != filepath.Join(p.Dir, "pgdata-pg18") {
		t.Fatalf("new data %s", m.NewData)
	}
	base, _ := os.ReadFile(filepath.Join(p.Dir, "compose.yaml"))
	over, _ := os.ReadFile(filepath.Join(p.Dir, "compose.override.yaml"))
	if !strings.Contains(string(base), `"./pgdata-pg18:/var/lib/postgresql"`) || !strings.Contains(string(base), "image: postgres:14\n") || !strings.Contains(string(over), "image: postgres:18-alpine") {
		t.Fatalf("files:\n%s\n%s", base, over)
	}
	if v := scalar(t, p, "db", "postgres", "postgres", "select x from t"); v != "kept" {
		t.Fatalf("got %q", v)
	}
}

// TestFailedSwitchPutsTheFileBack fails right after the switch: the original
// Compose file and the old service must come back.
func TestFailedSwitchPutsTheFileBack(t *testing.T) {
	p := testkit.New(t, `services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select 1 x")
	file := filepath.Join(p.Dir, "compose.yaml")
	orig, _ := os.ReadFile(file)
	_, _, err := upgradeNow(t, p, "db", 18, func(s string) error {
		if strings.HasPrefix(s, "switching") {
			return errors.New("injected failure")
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the injected failure")
	}
	if back, _ := os.ReadFile(file); string(back) != string(orig) {
		t.Fatalf("compose file not restored:\n%s", back)
	}
	if v := scalar(t, p, "db", "postgres", "postgres", "show server_version_num"); !strings.HasPrefix(v, "16") {
		t.Fatalf("serving %s", v)
	}
	if _, err := p.CLI.Run(context.Background(), "volume", "inspect", p.Opt.Name+"_data_pg18"); err == nil {
		t.Fatal("the new volume should be removed after a failed switch")
	}
}

func TestImageFromVariableNeedsManualSwitch(t *testing.T) {
	p := testkit.New(t, `services:
  db:
    image: postgres:${PG:-16}
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.Up("db")
	proj := p.Load()
	r, err := plan.Build(context.Background(), plan.Input{CLI: p.CLI, Project: proj, Service: "db", To: 18})
	testkit.Must(t, err)
	if !strings.Contains(r.SwitchProblem, "variable") {
		t.Fatalf("switch problem %q", r.SwitchProblem)
	}
	_, err = Run(context.Background(), Options{CLI: p.CLI, Project: proj, Plan: r, BackupDir: filepath.Join(t.TempDir(), "b"), Switch: true})
	if err == nil || !strings.Contains(err.Error(), "cannot be changed automatically") {
		t.Fatalf("got %v", err)
	}
	if got := states(p); !strings.Contains(got, "db=running") {
		t.Fatalf("db should not have been touched: %s", got)
	}
}
