package upgrade

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/testkit"
)

func TestOddNamesAndTemplate1Settings(t *testing.T) {
	p := testkit.New(t, `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres",
		`create database app`, `create database "dbname=app"`,
		`alter database template1 set work_mem = '7MB'`,
		`comment on database template1 is 'base for new databases'`)
	p.SQL("db", "postgres", "app", "create table t as select 1 as x")
	p.SQL("db", "postgres", "dbname='dbname=app'", "create table u as select g from generate_series(1, 5) g")
	// A list setting kept in postgresql.conf itself.
	p.Compose("exec", "-T", "db", "sh", "-c", `echo "search_path = '\"\$user\", public, extra'" >> "$PGDATA/postgresql.conf"`)
	p.Compose("restart", "db")
	p.Up("db")
	m, _, err := upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	_ = m
	checks := map[[2]string]string{
		{"dbname='dbname=app'", "select count(*) from u"}: "5",
		{"app", "select x from t"}:                        "1",
		{"postgres", "show search_path"}:                  `"$user", public, extra`,
		{"postgres", "select setconfig[1] from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase where d.datname = 'template1'"}: "work_mem=7MB",
		{"postgres", "select shobj_description(oid, 'pg_database') from pg_database where datname = 'template1'"}:                               "base for new databases",
	}
	for q, want := range checks {
		if got := scalar(t, p, "db", "postgres", q[0], q[1]); got != want {
			t.Errorf("%s: %s = %q, want %q", q[0], q[1], got, want)
		}
	}
}

func TestServiceWithItsOwnUser(t *testing.T) {
	p := testkit.New(t, `
services:
  db:
    image: postgres:16
    user: "1000:1000"
    environment: {POSTGRES_PASSWORD: pw}
    volumes: ["./pgdata:/var/lib/postgresql/data"]
`)
	// The folder must belong to the user before the first start.
	_, err := p.CLI.Run(t.Context(), "run", "--rm", "-v", p.Dir+":/d", "--entrypoint", "sh", "postgres:16", "-c", "mkdir /d/pgdata && chown 1000:1000 /d/pgdata")
	testkit.Must(t, err)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select 1 as x")
	_, _, err = upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	if got := scalar(t, p, "db", "postgres", "postgres", "select x from t"); got != "1" {
		t.Fatalf("got %q", got)
	}
}

func TestLatestBackupByTime(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	write := func(dir, project, service string, at time.Time) {
		d := filepath.Join(state, "pgcarry", project, dir)
		testkit.Must(t, os.MkdirAll(d, 0o700))
		testkit.Must(t, saveManifest(d, &Manifest{Project: project, Service: service, Started: at}))
	}
	write("db-20250101-000000", "shop", "db", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	write("auth-db-20261014-000000", "shop", "auth-db", time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC))
	write("db-20261013-000000", "other", "db", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	got, err := LatestBackup("shop", "")
	testkit.Must(t, err)
	if filepath.Base(got) != "auth-db-20261014-000000" {
		t.Fatalf("latest %s", got)
	}
	got, err = LatestBackup("shop", "db")
	testkit.Must(t, err)
	if filepath.Base(got) != "db-20250101-000000" {
		t.Fatalf("latest db %s", got)
	}
}
