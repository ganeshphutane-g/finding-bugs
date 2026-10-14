package upgrade

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/pgcarry/internal/testkit"
)

// TestVersionMatrix upgrades across several version pairs, each with a
// different way of setting up the service.
func TestVersionMatrix(t *testing.T) {
	cases := []struct {
		name, from string
		to         int
		env        string // extra environment lines for the db service
		extra      string // extra service keys
		setup      []string
		check      map[string]string // query in postgres db -> expected
	}{
		{
			name: "13 to 17 with C locale", from: "postgres:13", to: 17,
			env:   `      POSTGRES_INITDB_ARGS: "--locale=C --encoding=UTF8"`,
			setup: []string{"create table t as select g from generate_series(1, 100) g"},
			check: map[string]string{
				"select datcollate from pg_database where datname = 'template1'": "C",
				"select count(*) from t": "100",
			},
		},
		{
			name: "15 to 18 with an ICU database and trust auth", from: "postgres:15", to: 18,
			env: `      POSTGRES_HOST_AUTH_METHOD: trust`,
			setup: []string{
				"create database icu template template0 locale_provider icu icu_locale 'de-DE' locale 'en_US.utf8'",
				"create table t (s text)",
				"insert into t values ('b'), ('a'), ('ä')",
			},
			check: map[string]string{
				"select datlocprovider::text || ':' || datlocale from pg_database where datname = 'icu'": "i:de-DE",
				"select string_agg(s, '' order by s) from t":                                             "aäb",
			},
		},
		{
			name: "17 to 18 with a password file", from: "postgres:17", to: 18,
			env:   "      POSTGRES_PASSWORD_FILE: /run/secrets/pg\n      POSTGRES_USER: owner",
			extra: "    secrets: [pg]\n",
			setup: []string{"create table t (x int)", "insert into t values (42)"},
			check: map[string]string{"select x from t": "42"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := c.env
			if !strings.Contains(env, "PASSWORD") && !strings.Contains(env, "trust") {
				env += "\n      POSTGRES_PASSWORD: pw"
			}
			yaml := "services:\n  db:\n    image: " + c.from + "\n    environment:\n" + strings.TrimLeft(env, "\n") +
				"\n    volumes: [data:/var/lib/postgresql/data]\n" + c.extra + "volumes:\n  data:\n"
			if c.extra != "" {
				yaml += "secrets:\n  pg:\n    file: ./pg-password\n"
			}
			p := testkit.New(t, yaml)
			if c.extra != "" {
				testkit.Must(t, os.WriteFile(filepath.Join(p.Dir, "pg-password"), []byte("s3cret\n"), 0o644))
			}
			user := "postgres"
			if strings.Contains(c.env, "POSTGRES_USER: owner") {
				user = "owner"
			}
			p.Up("db")
			p.SQL("db", user, "postgres", c.setup...)
			if c.name == "15 to 18 with an ICU database and trust auth" {
				p.SQL("db", user, "icu", "create table t (s text)", "insert into t values ('b'), ('a'), ('ä')")
			}
			m, _, err := upgradeNow(t, p, "db", c.to, nil)
			testkit.Must(t, err)
			if !strings.HasPrefix(m.NewVersion, strconv.Itoa(c.to)+".") {
				t.Fatalf("version %s", m.NewVersion)
			}
			for q, want := range c.check {
				db := "postgres"
				if strings.Contains(q, "string_agg") {
					db = "icu"
				}
				if got := scalar(t, p, "db", user, db, q); got != want {
					t.Errorf("%s = %q, want %q", q, got, want)
				}
			}
		})
	}
}
