// Package pg reads facts about a PostgreSQL server running in a container,
// by running psql inside it.
package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/docker"
)

// Server is a PostgreSQL server in a running container, reached as User
// over the container's local socket.
type Server struct {
	CLI       *docker.CLI
	Container string
	User      string
}

// Query runs one SQL statement in database db and returns psql's unaligned,
// tuples-only output.
func (s *Server) Query(ctx context.Context, db, sql string) (string, error) {
	out, err := s.CLI.Run(ctx, "exec", s.Container, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1",
		"-At", "-U", s.User, "-d", ConnDB(db), "-c", sql)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// QueryJSON runs a statement returning one JSON value and decodes it.
func (s *Server) QueryJSON(ctx context.Context, db, sql string, v any) error {
	return s.queryJSON(ctx, db, sql, v)
}

// queryJSON runs a statement returning one JSON value and decodes it.
func (s *Server) queryJSON(ctx context.Context, db, sql string, v any) error {
	out, err := s.Query(ctx, db, sql)
	if err != nil {
		return err
	}
	if out == "" {
		out = "null"
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("reading query result: %w", err)
	}
	return nil
}

// Cluster is what pgcarry needs to know about a server.
type Cluster struct {
	VersionNum     int        `json:"version_num"` // e.g. 140024
	Version        string     `json:"version"`
	DataChecksums  bool       `json:"data_checksums"`
	InRecovery     bool       `json:"in_recovery"`
	PreloadLibs    string     `json:"shared_preload_libraries"`
	PasswordEnc    string     `json:"password_encryption"`
	DataDirectory  string     `json:"data_directory"`
	Databases      []Database `json:"databases"`
	Roles          []Role     `json:"roles"`
	Tablespaces    []string   `json:"tablespaces,omitempty"`
	ReplSlots      []string   `json:"replication_slots,omitempty"`
	MD5Passwords   int        `json:"md5_passwords"`
	TotalSizeBytes int64      `json:"total_size_bytes"`
}

// Major is the major version (14 for 14.24).
func (c *Cluster) Major() int { return c.VersionNum / 10000 }

// Database is one database.
type Database struct {
	Name           string      `json:"name"`
	Encoding       string      `json:"encoding"`
	Collate        string      `json:"collate"`
	Ctype          string      `json:"ctype"`
	LocaleProvider string      `json:"locale_provider"`  // libc, icu, builtin
	Locale         string      `json:"locale,omitempty"` // ICU or builtin locale
	AllowConn      bool        `json:"allow_connections"`
	IsTemplate     bool        `json:"is_template"`
	SizeBytes      int64       `json:"size_bytes"`
	Extensions     []Extension `json:"extensions"`
}

// Extension is an installed extension.
type Extension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Role is a database role.
type Role struct {
	Name      string `json:"name"`
	Superuser bool   `json:"superuser"`
	Login     bool   `json:"login"`
}

// Inspect reads the cluster's facts.
func (s *Server) Inspect(ctx context.Context, db string) (*Cluster, error) {
	var c Cluster
	err := s.queryJSON(ctx, db, `select json_build_object(
		'version_num', current_setting('server_version_num')::int,
		'version', current_setting('server_version'),
		'data_checksums', current_setting('data_checksums') = 'on',
		'in_recovery', pg_is_in_recovery(),
		'shared_preload_libraries', current_setting('shared_preload_libraries'),
		'password_encryption', current_setting('password_encryption'),
		'data_directory', current_setting('data_directory'))`, &c)
	if err != nil {
		return nil, err
	}
	provider, locale := `'libc'`, `null`
	switch {
	case c.Major() >= 17:
		provider = `case datlocprovider when 'i' then 'icu' when 'b' then 'builtin' else 'libc' end`
		locale = `datlocale`
	case c.Major() >= 15:
		provider = `case datlocprovider when 'i' then 'icu' else 'libc' end`
		locale = `daticulocale`
	}
	err = s.queryJSON(ctx, db, `select json_agg(json_build_object(
		'name', datname, 'encoding', pg_encoding_to_char(encoding),
		'collate', datcollate, 'ctype', datctype,
		'locale_provider', `+provider+`, 'locale', `+locale+`,
		'allow_connections', datallowconn, 'is_template', datistemplate,
		'size_bytes', pg_database_size(oid)) order by datname) from pg_database`, &c.Databases)
	if err != nil {
		return nil, err
	}
	err = s.queryJSON(ctx, db, `select json_agg(json_build_object(
		'name', rolname, 'superuser', rolsuper, 'login', rolcanlogin) order by rolname)
		from pg_roles where rolname !~ '^pg_'`, &c.Roles)
	if err != nil {
		return nil, err
	}
	if err := s.queryJSON(ctx, db, `select coalesce(json_agg(spcname order by spcname), '[]')
		from pg_tablespace where spcname not in ('pg_default', 'pg_global')`, &c.Tablespaces); err != nil {
		return nil, err
	}
	if err := s.queryJSON(ctx, db, `select coalesce(json_agg(slot_name order by slot_name), '[]')
		from pg_replication_slots`, &c.ReplSlots); err != nil {
		return nil, err
	}
	// Reading pg_authid needs a superuser; without one this stays 0.
	if out, err := s.Query(ctx, db, `select count(*) from pg_authid where rolpassword like 'md5%'`); err == nil {
		fmt.Sscan(out, &c.MD5Passwords)
	}
	for i := range c.Databases {
		d := &c.Databases[i]
		c.TotalSizeBytes += d.SizeBytes
		if !d.AllowConn {
			continue
		}
		if err := s.queryJSON(ctx, d.Name, `select coalesce(json_agg(json_build_object(
			'name', extname, 'version', extversion) order by extname), '[]') from pg_extension`, &d.Extensions); err != nil {
			return nil, fmt.Errorf("listing extensions in %s: %w", d.Name, err)
		}
	}
	return &c, nil
}

// AvailableExtensions lists the extensions a server can install, with
// their default versions.
func (s *Server) AvailableExtensions(ctx context.Context, db string) (map[string]string, error) {
	var list []struct {
		Name    string `json:"name"`
		Default string `json:"default_version"`
	}
	if err := s.queryJSON(ctx, db, `select coalesce(json_agg(json_build_object(
		'name', name, 'default_version', default_version)), '[]') from pg_available_extensions`, &list); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, e := range list {
		m[e.Name] = e.Default
	}
	return m, nil
}

// ConnDB is a -d argument naming exactly database name: PostgreSQL tools
// read a -d value containing "=" (or a URI) as connection settings, so the
// name goes inside a connection string.
func ConnDB(name string) string {
	return "dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(name) + "'"
}
