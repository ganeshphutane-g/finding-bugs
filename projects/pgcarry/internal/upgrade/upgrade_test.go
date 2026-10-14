package upgrade

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/images"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
	"github.com/ganeshphutane-g/pgcarry/internal/plan"
	"github.com/ganeshphutane-g/pgcarry/internal/testkit"
)

// rehearse plans and copies, returning the manifest (or the error).
func rehearse(t *testing.T, p *testkit.Project, service string, to int, image string, hook func(string) error) (*Manifest, *plan.Report, error) {
	t.Helper()
	proj := p.Load()
	r, err := plan.Build(context.Background(), plan.Input{CLI: p.CLI, Project: proj, Service: service, To: to, Image: image})
	testkit.Must(t, err)
	if !r.OK() {
		t.Fatalf("plan blocked: %q", r.Blockers)
	}
	var log strings.Builder
	m, err := Run(context.Background(), Options{CLI: p.CLI, Project: proj, Plan: r,
		BackupDir: filepath.Join(t.TempDir(), "backup"), Log: func(s string) { log.WriteString(s + "\n") }, hook: hook})
	if err != nil {
		t.Logf("log:\n%s", log.String())
	}
	return m, r, err
}

// inspectCopy starts the new image on the copy and returns a server to query.
func inspectCopy(t *testing.T, p *testkit.Project, m *Manifest, r *plan.Report) *pg.Server {
	t.Helper()
	ctx := context.Background()
	name := "pgcarry-check-" + p.Opt.Name
	_, err := p.CLI.Run(ctx, "run", "-d", "--name", name, "-v", m.NewData+":"+m.NewMount,
		"-e", "POSTGRES_PASSWORD=unused", r.TargetImage)
	testkit.Must(t, err)
	t.Cleanup(func() { p.CLI.Run(context.Background(), "rm", "-f", name) })
	testkit.Must(t, images.WaitReady(ctx, p.CLI, name, 2*time.Minute))
	return &pg.Server{CLI: p.CLI, Container: name}
}

func query(t *testing.T, s *pg.Server, user, db, sql string) string {
	t.Helper()
	s.User = user
	out, err := s.Query(context.Background(), db, sql)
	testkit.Must(t, err)
	return out
}

// leftovers lists containers pgcarry may have left running.
func leftovers(t *testing.T, p *testkit.Project) string {
	out, _ := p.CLI.Run(context.Background(), "ps", "-aq", "--filter", "label=pgcarry.run")
	return strings.TrimSpace(out)
}

func states(p *testkit.Project) string {
	return strings.TrimSpace(p.Compose("ps", "--all", "--format", "{{.Service}}={{.State}}"))
}

func TestRehearsalOfficial14To18(t *testing.T) {
	p := testkit.New(t, `
services:
  web:
    image: postgres:14
    entrypoint: ["sleep", "infinity"]
    stop_grace_period: 1s
    depends_on: [db]
  db:
    image: postgres:14
    environment: {POSTGRES_USER: user, POSTGRES_PASSWORD: pw, POSTGRES_DB: shop}
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
`)
	p.Up("db")
	p.SQL("db", "user", "shop",
		"create table orders as select g as id, md5(g::text) as note, now() as at from generate_series(1, 20000) g",
		"alter table orders add primary key (id)",
		"create index on orders (note)",
		"create sequence invoice_no start 1000",
		"select nextval('invoice_no')",
		"create view recent as select * from orders where id > 19000",
		"create materialized view counts as select count(*) from orders",
		"create function twice(int) returns int language sql as 'select $1 * 2'",
		"create schema audit",
		"create table audit.log (id serial primary key, msg text not null)",
		"insert into audit.log (msg) values ('a'), ('b')",
		"create role reader login password 'readpw' connection limit 5",
		"grant select on orders to reader",
		"create role writers nologin",
		"grant writers to reader",
		"create database reports owner reader",
		"alter system set work_mem = '16MB'",
		"alter database shop set statement_timeout = '30s'",
	)
	p.SQL("db", "user", "reports", "create table r (x int)", "insert into r values (1), (2), (3)")

	m, r, err := rehearse(t, p, "", 18, "", nil)
	testkit.Must(t, err)
	if !m.Verified || !strings.HasPrefix(m.NewVersion, "18") || m.NewData != p.Opt.Name+"_pgdata_pg18" {
		t.Fatalf("manifest: %+v", m)
	}
	if got := states(p); !strings.Contains(got, "db=running") || !strings.Contains(got, "web=running") {
		t.Fatalf("services not started again: %s", got)
	}
	if l := leftovers(t, p); l != "" {
		t.Fatalf("containers left: %s", l)
	}

	s := inspectCopy(t, p, m, r)
	checks := map[string]string{
		"select count(*), sum(id) from orders":                                      "20000|200010000",
		"select last_value from invoice_no":                                         "1000",
		"select count(*) from recent":                                               "1000",
		"select * from counts":                                                      "20000",
		"select twice(21)":                                                          "42",
		"select string_agg(msg, ',' order by id) from audit.log":                    "a,b",
		"show work_mem":                                                             "16MB",
		"show data_checksums":                                                       "on",
		"select rolconnlimit from pg_roles where rolname = 'reader'":                "5",
		"select has_table_privilege('reader', 'orders', 'select')":                  "t",
		"select pg_has_role('reader', 'writers', 'member')":                         "t",
		"select pg_get_userbyid(datdba) from pg_database where datname = 'reports'": "reader",
		"select setconfig[1] from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase where d.datname = 'shop'": "statement_timeout=30s",
		"select count(*) from pg_indexes where tablename = 'orders'":                                                         "2",
	}
	for q, want := range checks {
		if got := query(t, s, "user", "shop", q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if got := query(t, s, "user", "reports", "select sum(x) from r"); got != "6" {
		t.Errorf("reports: %q", got)
	}
	// Passwords are the same hashes, so the old passwords still work.
	s.User = "user"
	if _, err := p.CLI.Run(context.Background(), "exec", "-e", "PGPASSWORD=readpw", s.Container,
		"psql", "-h", "localhost", "-U", "reader", "-d", "shop", "-c", "select 1"); err != nil {
		t.Errorf("reader cannot log in with its old password: %v", err)
	}
	// The old data still answers as before.
	p.SQL("db", "user", "shop", "select 1 from orders limit 1")
}

func TestRehearsalBindMountAlpineLocale(t *testing.T) {
	p := testkit.New(t, `
services:
  db:
    image: postgres:14-alpine
    environment: {POSTGRES_PASSWORD: pw}
    volumes: ["$DIR/pgdata:/var/lib/postgresql/data"]
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres",
		"create database shop template template0 lc_collate 'de_DE.UTF-8' lc_ctype 'de_DE.UTF-8'",
		"create table t as select g from generate_series(1, 500) g")
	p.SQL("db", "postgres", "shop", "create table items (name text)", "insert into items values ('Äpfel'), ('Zucker'), ('apfel')")
	m, r, err := rehearse(t, p, "db", 18, "", nil)
	testkit.Must(t, err)
	if m.NewData != filepath.Join(p.Dir, "pgdata-pg18") || r.TargetImage != "postgres:18-alpine" {
		t.Fatalf("manifest: %+v", m)
	}
	s := inspectCopy(t, p, m, r)
	if got := query(t, s, "postgres", "shop", "select datcollate from pg_database where datname = 'shop'"); got != "de_DE.UTF-8" {
		t.Errorf("collate %q", got)
	}
	if got := query(t, s, "postgres", "postgres", "select count(*) from t"); got != "500" {
		t.Errorf("rows %q", got)
	}
}

func TestRehearsalPgvector(t *testing.T) {
	p := testkit.New(t, `
services:
  db:
    image: pgvector/pgvector:pg16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres",
		"create extension vector",
		"create table items (id int primary key, embedding vector(3))",
		"insert into items select g, array[g, g * 2, g * 3]::vector from generate_series(1, 300) g",
		"create index on items using hnsw (embedding vector_l2_ops)")
	m, r, err := rehearse(t, p, "db", 18, "", nil)
	testkit.Must(t, err)
	s := inspectCopy(t, p, m, r)
	if got := query(t, s, "postgres", "postgres", "select id from items order by embedding <-> '[10,20,30]' limit 1"); got != "10" {
		t.Errorf("nearest %q", got)
	}
}

// TestFailureAtEveryStepPutsThingsBack fails the run after each step in
// turn: the old service and its apps must run again, no helper container may
// stay, and a half-made copy must be gone.
func TestFailureAtEveryStepPutsThingsBack(t *testing.T) {
	p := testkit.New(t, `
services:
  web:
    image: postgres:16
    entrypoint: ["sleep", "infinity"]
    stop_grace_period: 1s
    depends_on: [db]
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select g from generate_series(1, 100) g")
	steps := []string{"stopping the apps", "stopping the database", "starting the old database", "recording",
		"dumping", "stopping the old database again", "creating the new", "starting PostgreSQL", "restoring",
		"verifying", "stopping the new"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			_, _, err := rehearse(t, p, "db", 18, "", func(s string) error {
				if strings.HasPrefix(s, step) {
					return errors.New("injected failure")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("expected the injected failure, got %v", err)
			}
			if got := states(p); !strings.Contains(got, "db=running") || !strings.Contains(got, "web=running") {
				t.Fatalf("not put back: %s", got)
			}
			if l := leftovers(t, p); l != "" {
				t.Fatalf("containers left: %s", l)
			}
			if _, err := p.CLI.Run(context.Background(), "volume", "inspect", p.Opt.Name+"_data_pg18"); err == nil {
				t.Fatal("the half-made volume is still there")
			}
			// The service comes back with its data and accepts work.
			id := strings.TrimSpace(p.Compose("ps", "-q", "db"))
			testkit.Must(t, images.WaitReady(context.Background(), p.CLI, id, time.Minute))
			p.SQL("db", "postgres", "postgres", "select count(*) from t")
		})
	}
}

// TestUndeclaredWriterLosesNothing runs an app that keeps inserting rows
// but is not declared as depending on the database, so pgcarry does not stop
// it. Every row the old server accepted must be in the new one.
func TestUndeclaredWriterLosesNothing(t *testing.T) {
	p := testkit.New(t, `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
  writer:
    image: postgres:16
    environment: {PGPASSWORD: pw}
    stop_grace_period: 1s
    entrypoint: ["sh", "-c", "while true; do psql -q -h db -U postgres -c 'insert into w default values' 2>/dev/null; sleep 0.05; done"]
volumes:
  data:
`)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table w (id serial primary key, at timestamptz default now())")
	time.Sleep(2 * time.Second)
	m, _, err := upgradeNow(t, p, "db", 18, nil)
	testkit.Must(t, err)
	// What the old data holds at the end.
	ctx := context.Background()
	name := "pgcarry-oldcheck-" + p.Opt.Name
	_, err = p.CLI.Run(ctx, "run", "-d", "--name", name, "--network", "none", "-v", m.OldData+":/var/lib/postgresql/data", "postgres:16")
	testkit.Must(t, err)
	defer p.CLI.Run(ctx, "rm", "-f", name)
	testkit.Must(t, images.WaitReady(ctx, p.CLI, name, time.Minute))
	old := &pg.Server{CLI: p.CLI, Container: name, User: "postgres"}
	last, err := old.Query(ctx, "postgres", "select max(id) || ' ' || count(*) from w")
	testkit.Must(t, err)
	var maxID, count int
	fmt.Sscan(last, &maxID, &count)
	if count < 5 {
		t.Fatalf("the writer barely wrote (%s)", last)
	}
	got := scalar(t, p, "db", "postgres", "postgres", fmt.Sprintf("select count(*) from w where id <= %d", maxID))
	if got != fmt.Sprint(count) {
		t.Fatalf("the new server has %s of the %d rows the old one accepted", got, count)
	}
}

func TestInterruptPutsThingsBack(t *testing.T) {
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
	p.SQL("db", "postgres", "postgres", "create table t as select g from generate_series(1, 2000000) g")
	proj := p.Load()
	r, err := plan.Build(context.Background(), plan.Input{CLI: p.CLI, Project: proj, Service: "db", To: 18})
	testkit.Must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Run(ctx, Options{CLI: p.CLI, Project: proj, Plan: r, BackupDir: filepath.Join(t.TempDir(), "b"),
		hook: func(s string) error {
			if strings.HasPrefix(s, "creating the new") {
				cancel() // as Ctrl-C would, in the middle of the run
			}
			return nil
		}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if got := states(p); !strings.Contains(got, "db=running") {
		t.Fatalf("not put back: %s", got)
	}
	if l := leftovers(t, p); l != "" {
		t.Fatalf("containers left: %s", l)
	}
}
