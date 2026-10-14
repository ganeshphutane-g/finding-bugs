package edit

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func volReq(files ...string) Request {
	return Request{Files: files, Service: "db", Image: "postgres:18", MountTarget: "/var/lib/postgresql/data",
		NewTarget: "/var/lib/postgresql", Kind: "volume", VolumeKey: "pgdata", NewKey: "pgdata_pg18", NewName: "app_pgdata_pg18"}
}

func one(t *testing.T, req Request) string {
	t.Helper()
	files, err := Prepare(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("%d files changed", len(files))
	}
	return string(files[0].New)
}

func TestBlockStyleKeepsCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", `# my app
services:
  db:
    image: postgres:14   # pinned major
    environment:
      POSTGRES_PASSWORD: pw
    volumes:
      - pgdata:/var/lib/postgresql/data # data
      - ./init:/docker-entrypoint-initdb.d:ro
volumes:
  # keep this
  pgdata:
  other:
`)
	got := one(t, volReq(f))
	want := `# my app
services:
  db:
    image: postgres:18   # pinned major
    environment:
      POSTGRES_PASSWORD: pw
    volumes:
      - pgdata_pg18:/var/lib/postgresql # data
      - ./init:/docker-entrypoint-initdb.d:ro
volumes:
  # keep this
  pgdata_pg18:
  pgdata:
  other:
`
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestQuotedAndFlowStyles(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", `services:
  db:
    image: "postgres:14-alpine"
    volumes: ['pgdata:/var/lib/postgresql/data', "./x:/x"]
volumes:
  pgdata: {}
`)
	r := volReq(f)
	r.Image = "postgres:18-alpine"
	got := one(t, r)
	for _, w := range []string{`image: "postgres:18-alpine"`, `volumes: ['pgdata_pg18:/var/lib/postgresql', "./x:/x"]`, "  pgdata_pg18: {}\n  pgdata: {}"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestLongSyntaxAndVolumeOptions(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", `services:
  db:
    image: postgres:16
    volumes:
      - type: volume
        source: pgdata
        target: /var/lib/postgresql/data
volumes:
    pgdata:
        name: shared-pg
        driver: local
        labels:
            team: data
`)
	r := volReq(f)
	r.NewName = "shared-pg_pg18"
	got := one(t, r)
	for _, w := range []string{"        source: pgdata_pg18\n        target: /var/lib/postgresql\n",
		"    pgdata_pg18:\n        name: shared-pg_pg18\n        driver: local\n        labels:\n          team: data\n    pgdata:\n        name: shared-pg"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestExternalVolumeGetsAName(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", `services:
  db:
    image: postgres:16
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
    external: true
`)
	r := volReq(f)
	r.NewName = "pgdata_pg18"
	got := one(t, r)
	if !strings.Contains(got, "  pgdata_pg18:\n    external: true\n    name: pgdata_pg18\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBindMountKeepsRelativePath(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", `services:
  db:
    image: postgres:16
    volumes:
      - ./data/pg/:/var/lib/postgresql/data:Z
`)
	r := volReq(f)
	r.Kind, r.BindSuffix = "bind", "-pg18"
	got := one(t, r)
	if !strings.Contains(got, "- ./data/pg-pg18:/var/lib/postgresql:Z\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestOverrideFileWins(t *testing.T) {
	dir := t.TempDir()
	base := write(t, dir, "compose.yaml", `services:
  db:
    image: postgres:14
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
`)
	over := write(t, dir, "compose.override.yaml", `services:
  db:
    image: postgres:15
`)
	files, err := Prepare(volReq(base, over))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("%d files", len(files))
	}
	for _, f := range files {
		switch filepath.Base(f.Path) {
		case "compose.yaml":
			if !strings.Contains(string(f.New), "image: postgres:14") || !strings.Contains(string(f.New), "pgdata_pg18:/var/lib/postgresql]") {
				t.Errorf("base:\n%s", f.New)
			}
		case "compose.override.yaml":
			if !strings.Contains(string(f.New), "image: postgres:18") {
				t.Errorf("override:\n%s", f.New)
			}
		}
	}
}

func TestVariablesNeedAManualEdit(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{
		"services:\n  db:\n    image: postgres:${PG_VERSION}\n    volumes: [pgdata:/var/lib/postgresql/data]\nvolumes:\n  pgdata:\n",
		"services:\n  db:\n    image: postgres:14\n    volumes: [\"${DATA}:/var/lib/postgresql/data\"]\n",
		"services:\n  db:\n    extends: {file: base.yaml, service: pg}\n    image: postgres:14\n",
		"services:\n  db:\n    image: >-\n      postgres:14\n    volumes: [pgdata:/var/lib/postgresql/data]\nvolumes:\n  pgdata:\n",
	} {
		f := write(t, dir, "compose.yaml", content)
		r := volReq(f)
		if strings.Contains(content, "DATA") {
			r.Kind = "bind"
		}
		if _, err := Prepare(r); !errors.Is(err, ErrManual) {
			t.Errorf("expected a manual edit for:\n%s\ngot %v", content, err)
		}
	}
}

func TestWindowsLineEndings(t *testing.T) {
	dir := t.TempDir()
	f := write(t, dir, "compose.yaml", "services:\r\n  db:\r\n    image: postgres:14\r\n    volumes:\r\n      - pgdata:/var/lib/postgresql/data\r\nvolumes:\r\n  pgdata:\r\n")
	got := one(t, volReq(f))
	want := "services:\r\n  db:\r\n    image: postgres:18\r\n    volumes:\r\n      - pgdata_pg18:/var/lib/postgresql\r\nvolumes:\r\n  pgdata_pg18:\r\n  pgdata:\r\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestDiff(t *testing.T) {
	d := Diff(File{Path: "c.yaml", Old: []byte("a\nb\nc\nd\n"), New: []byte("a\nB\nc\nd\n")})
	if !strings.Contains(d, "-b\n+B\n") {
		t.Fatal(d)
	}
}
