package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Inventory is what a database holds, for comparing a copy with its
// original. Objects that belong to extensions are left out: a newer
// extension version may legitimately create different ones.
type Inventory struct {
	Rows      map[string]int64 `json:"rows"`      // table -> row count
	Sequences map[string]int64 `json:"sequences"` // sequence -> last value (0 if never used)
	Objects   map[string]int   `json:"objects"`   // kind -> count
}

const notExtension = `not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
	and d.objid = c.oid and d.deptype = 'e')`

// Inventory counts the rows of every table, reads every sequence and counts
// schemas, tables, views, functions and other objects in database db.
func (s *Server) Inventory(ctx context.Context, db string) (*Inventory, error) {
	inv := &Inventory{}
	err := s.queryJSON(ctx, db, `select coalesce(json_object_agg(format('%I.%I', n.nspname, c.relname),
		(xpath('/row/n/text()', query_to_xml(format('select count(*) as n from only %I.%I', n.nspname, c.relname), false, true, '')))[1]::text::bigint), '{}')
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where c.relkind in ('r', 'm') and n.nspname not in ('pg_catalog', 'information_schema')
		and n.nspname !~ '^pg_toast' and `+notExtension, &inv.Rows)
	if err != nil {
		return nil, fmt.Errorf("counting rows in %s: %w", db, err)
	}
	err = s.queryJSON(ctx, db, `select coalesce(json_object_agg(format('%I.%I', schemaname, sequencename),
		coalesce(last_value, 0)), '{}') from pg_sequences s
		join pg_namespace n on n.nspname = s.schemaname
		join pg_class c on c.relnamespace = n.oid and c.relname = s.sequencename
		where `+notExtension, &inv.Sequences)
	if err != nil {
		return nil, fmt.Errorf("reading sequences in %s: %w", db, err)
	}
	err = s.queryJSON(ctx, db, `select coalesce(json_object_agg(kind, n), '{}') from (
		select case c.relkind when 'r' then 'tables' when 'p' then 'partitioned tables'
			when 'v' then 'views' when 'm' then 'materialized views' when 'S' then 'sequences'
			when 'i' then 'indexes' when 'I' then 'partitioned indexes' when 'f' then 'foreign tables'
			when 'c' then 'composite types' else 'other relations' end as kind, count(*) as n
		from pg_class c join pg_namespace ns on ns.oid = c.relnamespace
		where ns.nspname not in ('pg_catalog', 'information_schema') and ns.nspname !~ '^pg_toast'
		and `+notExtension+`
		group by 1
		union all
		select 'functions', count(*) from pg_proc p join pg_namespace ns on ns.oid = p.pronamespace
		where ns.nspname not in ('pg_catalog', 'information_schema') and not exists (select 1 from pg_depend d
			where d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e')
		union all
		select 'schemas', count(*) from pg_namespace ns where ns.nspname not in ('pg_catalog', 'information_schema')
		and ns.nspname !~ '^pg_(toast|temp)' and not exists (select 1 from pg_depend d
			where d.classid = 'pg_namespace'::regclass and d.objid = ns.oid and d.deptype = 'e')
		union all
		select 'triggers', count(*) from pg_trigger where not tgisinternal
		union all
		-- NOT NULL constraints are catalogued from PostgreSQL 18 on; leave
		-- them out so versions compare.
		select 'constraints', count(*) from pg_constraint co join pg_namespace ns on ns.oid = co.connamespace
		where ns.nspname not in ('pg_catalog', 'information_schema') and co.contype <> 'n'
	) k where n > 0`, &inv.Objects)
	if err != nil {
		return nil, fmt.Errorf("counting objects in %s: %w", db, err)
	}
	return inv, nil
}

// Diff lists the differences between an original and its copy.
func (a *Inventory) Diff(b *Inventory) []string {
	var out []string
	cmp := func(what string, x, y map[string]int64) {
		for _, k := range unionKeys(x, y) {
			vx, okx := x[k]
			vy, oky := y[k]
			switch {
			case !oky:
				out = append(out, fmt.Sprintf("%s %s is missing in the copy", what, k))
			case !okx:
				out = append(out, fmt.Sprintf("%s %s is new in the copy", what, k))
			case vx != vy:
				out = append(out, fmt.Sprintf("%s %s: %d before, %d after", what, k, vx, vy))
			}
		}
	}
	cmp("table", a.Rows, b.Rows)
	cmp("sequence", a.Sequences, b.Sequences)
	objA, objB := map[string]int64{}, map[string]int64{}
	for k, v := range a.Objects {
		objA[k] = int64(v)
	}
	for k, v := range b.Objects {
		objB[k] = int64(v)
	}
	for _, k := range unionKeys(objA, objB) {
		if objA[k] != objB[k] {
			out = append(out, fmt.Sprintf("%s: %d before, %d after", k, objA[k], objB[k]))
		}
	}
	return out
}

// TotalRows is the number of rows in all tables.
func (a *Inventory) TotalRows() int64 {
	var n int64
	for _, v := range a.Rows {
		n += v
	}
	return n
}

func unionKeys(a, b map[string]int64) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	var keys []string
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RoleNames lists role names other than the built-in pg_ ones.
func (s *Server) RoleNames(ctx context.Context) ([]string, error) {
	out, err := s.Query(ctx, "postgres", `select rolname from pg_roles where rolname !~ '^pg_' order by 1`)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// RoleSignatures describes each role's attributes, memberships and
// password (as a hash of the stored hash), to check a copy carries them.
func (s *Server) RoleSignatures(ctx context.Context) (map[string]string, error) {
	sigs := map[string]string{}
	err := s.queryJSON(ctx, "postgres", `select coalesce(json_object_agg(rolname, concat_ws(' ',
		case when rolsuper then 'superuser' end, case when rolinherit then 'inherit' end,
		case when rolcreaterole then 'createrole' end, case when rolcreatedb then 'createdb' end,
		case when rolcanlogin then 'login' end, case when rolreplication then 'replication' end,
		case when rolbypassrls then 'bypassrls' end, 'connlimit=' || rolconnlimit,
		'valid=' || coalesce(rolvaliduntil::text, '-'), 'password=' || coalesce(md5(rolpassword), '-'),
		'member_of=' || coalesce((select string_agg(r.rolname, ',' order by r.rolname)
			from pg_auth_members m join pg_roles r on r.oid = m.roleid where m.member = a.oid), '-'))), '{}')
		from pg_authid a where rolname !~ '^pg_'`, &sigs)
	return sigs, err
}
