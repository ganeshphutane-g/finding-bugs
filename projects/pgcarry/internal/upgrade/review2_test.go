package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/images"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
	"github.com/ganeshphutane-g/pgcarry/internal/plan"
	"github.com/ganeshphutane-g/pgcarry/internal/testkit"
)

const simpleDB = `
services:
  db:
    image: postgres:16
    environment: {POSTGRES_PASSWORD: pw}
    volumes: [data:/var/lib/postgresql/data]
volumes:
  data:
`

// TestServiceStartedDuringTheDumpIsCaught starts the service while pgcarry
// has the old data open in its own container.
func TestServiceStartedDuringTheDumpIsCaught(t *testing.T) {
	p := testkit.New(t, simpleDB)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select 1 x")
	_, _, err := rehearse(t, p, "db", 18, "", func(s string) error {
		if strings.HasPrefix(s, "recording") {
			p.Compose("start", "db")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "was started while pgcarry had its data open") {
		t.Fatalf("got %v", err)
	}
	if l := leftovers(t, p); l != "" {
		t.Fatalf("containers left: %s", l)
	}
}

// TestUsedRehearsalCopyIsKept: a rehearsal copy is replaced by the next run
// only while nothing has run on it.
func TestUsedRehearsalCopyIsKept(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := testkit.New(t, simpleDB)
	p.Up("db")
	p.SQL("db", "postgres", "postgres", "create table t as select 1 x")
	run := func() error {
		proj := p.Load()
		r, err := plan.Build(context.Background(), plan.Input{CLI: p.CLI, Project: proj, Service: "db", To: 18})
		testkit.Must(t, err)
		dir := filepath.Join(StateDir(), proj.Name, "db-"+time.Now().Format("150405.000000"))
		_, err = Run(context.Background(), Options{CLI: p.CLI, Project: proj, Plan: r, BackupDir: dir})
		return err
	}
	testkit.Must(t, run())
	testkit.Must(t, run()) // untouched: replaced
	// Someone uses the copy.
	ctx := context.Background()
	name := "pgcarry-adopt-" + p.Opt.Name
	_, err := p.CLI.Run(ctx, "run", "-d", "--name", name, "-v", p.Opt.Name+"_data_pg18:/var/lib/postgresql", "-e", "POSTGRES_PASSWORD=x", "postgres:18")
	testkit.Must(t, err)
	testkit.Must(t, images.WaitReady(ctx, p.CLI, name, time.Minute))
	s := &pg.Server{CLI: p.CLI, Container: name, User: "postgres"}
	_, err = s.Query(ctx, "postgres", "create table precious as select 42 v")
	testkit.Must(t, err)
	p.CLI.Run(ctx, "rm", "-f", name)
	if err := run(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a used copy must not be replaced; got %v", err)
	}
}

// TestRecoveryKeepsDataStillInUse: recovery must not remove a copy that a
// container still uses.
func TestRecoveryKeepsDataStillInUse(t *testing.T) {
	p := testkit.New(t, simpleDB)
	p.Up("db")
	ctx := context.Background()
	id := strings.TrimSpace(p.Compose("ps", "-q", "db"))
	old := &pg.Server{CLI: p.CLI, Container: id, User: "postgres"}
	sysid, err := old.Query(ctx, "postgres", "select system_identifier from pg_control_system()")
	testkit.Must(t, err)
	inUse := filepath.Join(p.Dir, "copy-pg18")
	testkit.Must(t, os.MkdirAll(inUse, 0o755))
	testkit.Must(t, os.WriteFile(filepath.Join(inUse, "keep"), []byte("x"), 0o644))
	user := "pgcarry-user-" + p.Opt.Name
	_, err = p.CLI.Run(ctx, "run", "-d", "--name", user, "-v", inUse+":/data", "alpine:3.20", "sleep", "300")
	testkit.Must(t, err)
	defer p.CLI.Run(ctx, "rm", "-f", user)
	dir := t.TempDir()
	testkit.Must(t, saveManifest(dir, &Manifest{Project: p.Opt.Name, Service: "db", Compose: p.Opt, User: "postgres",
		OldVersion: "16", OldSysID: sysid, MountType: "bind", NewData: inUse, NewImage: "postgres:18",
		Created: true, RunID: "test", Started: time.Now()}))
	err = Rollback(ctx, p.CLI, dir)
	if err == nil || !strings.Contains(err.Error(), "still used by") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(inUse, "keep")); err != nil {
		t.Fatal("the data in use was removed")
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDir(dir)
	testkit.Must(t, err)
	if _, err := lockDir(dir); err == nil {
		t.Fatal("second lock should fail")
	}
	unlock()
	u2, err := lockDir(dir)
	testkit.Must(t, err)
	u2()
}
