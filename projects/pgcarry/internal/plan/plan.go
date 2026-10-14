// Package plan works out what upgrading a Compose project's Postgres service
// would involve, and what stands in the way, without changing anything.
package plan

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/edit"
	"github.com/ganeshphutane-g/pgcarry/internal/images"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
)

// Input selects what to plan.
type Input struct {
	CLI     *docker.CLI
	Project *compose.Project
	Service string // "" to find it
	To      int    // target major version (0: from Image)
	Image   string // target image ("" to work it out)
}

// Report is the plan.
type Report struct {
	Project   string `json:"project"`
	Service   string `json:"service"`
	Image     string `json:"image"`
	Container string `json:"container,omitempty"`
	Running   bool   `json:"running"`

	DataPath   string `json:"data_path"`           // PGDATA inside the container
	MountType  string `json:"mount_type"`          // volume or bind
	MountName  string `json:"mount_name"`          // volume name or host path
	MountPoint string `json:"mount_point"`         // mount target in the container
	MountKey   string `json:"mount_key,omitempty"` // the volume's key in the Compose file
	FileMajor  int    `json:"file_major"`          // from PG_VERSION

	Cluster *pg.Cluster `json:"cluster,omitempty"`

	TargetImage string        `json:"target_image"`
	Target      *images.Probe `json:"target,omitempty"`
	TargetData  string        `json:"target_data_path"`
	TargetMount string        `json:"target_mount_point"`

	FreeBytes  int64    `json:"free_bytes"` // on the data volume's filesystem
	Dependents []string `json:"stops_services"`

	CustomSettings []pg.Setting `json:"custom_settings,omitempty"`
	CustomHBA      []string     `json:"custom_hba_rules,omitempty"`

	ComposeFiles []string    `json:"compose_files,omitempty"`
	Edits        []edit.File `json:"-"`
	EditDiff     string      `json:"compose_diff,omitempty"`
	// SwitchProblem says why the Compose file cannot be edited
	// automatically (a rehearsal still works).
	SwitchProblem string `json:"switch_problem,omitempty"`

	Blockers []string `json:"blockers"`
	Warnings []string `json:"warnings"`
	Changes  []string `json:"changes"`
}

// OK reports whether nothing blocks the upgrade.
func (r *Report) OK() bool { return len(r.Blockers) == 0 }

func (r *Report) block(f string, a ...any) { r.Blockers = append(r.Blockers, fmt.Sprintf(f, a...)) }
func (r *Report) warn(f string, a ...any)  { r.Warnings = append(r.Warnings, fmt.Sprintf(f, a...)) }
func (r *Report) change(f string, a ...any) {
	r.Changes = append(r.Changes, fmt.Sprintf(f, a...))
}

var numericUser = regexp.MustCompile(`^\d+(:\d+)?$`)

var pgImage = regexp.MustCompile(`(?i)(^|/)(postgres|postgresql|postgis|pgvector|timescaledb[\w-]*)(:|@|$)`)

// FindService picks the Postgres service of a project.
func FindService(p *compose.Project, name string) (compose.Service, error) {
	if name != "" {
		s, ok := p.Services[name]
		if !ok {
			return s, fmt.Errorf("the project has no service %q (services: %s)", name, strings.Join(p.ServiceNames(), ", "))
		}
		return s, nil
	}
	// A service keeping data where Postgres does is the database; others
	// may use a Postgres image only for its tools (backups, migrations).
	var withData, byImage []string
	for _, n := range p.ServiceNames() {
		s := p.Services[n]
		hasData := false
		for _, m := range s.Volumes {
			hasData = hasData || strings.HasPrefix(m.Target, "/var/lib/postgresql")
		}
		switch {
		case hasData:
			withData = append(withData, n)
		case pgImage.MatchString(s.Image):
			byImage = append(byImage, n)
		}
	}
	found := withData
	if len(found) == 0 {
		found = byImage
	}
	switch len(found) {
	case 0:
		return compose.Service{}, errors.New("no Postgres service found in the project; name the service")
	case 1:
		return p.Services[found[0]], nil
	}
	return compose.Service{}, fmt.Errorf("several services look like Postgres (%s); name the one to upgrade", strings.Join(found, ", "))
}

// dataMount finds the mount holding dataPath (the deepest one).
func dataMount(s compose.Service, dataPath string) (compose.Mount, bool) {
	var best compose.Mount
	found := false
	for _, m := range s.Volumes {
		t := strings.TrimSuffix(m.Target, "/")
		if dataPath == t || strings.HasPrefix(dataPath, t+"/") {
			if !found || len(t) > len(best.Target) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// targetLayout returns where the target image keeps its data and what to
// mount: from 18 the image keeps data in a versioned folder below one mount.
func targetLayout(info *images.Info, major int) (data, mount string) {
	data = info.PGData
	if data == "" {
		data = "/var/lib/postgresql/data"
	}
	mount = data
	if len(info.Volumes) == 1 {
		mount = info.Volumes[0]
	}
	if major >= 18 && strings.HasPrefix(data, "/var/lib/postgresql/") && mount == data {
		mount = "/var/lib/postgresql"
	}
	return data, mount
}

// Build makes the plan. An error means the project could not be read; what
// stands in the way of the upgrade is in Report.Blockers.
func Build(ctx context.Context, in Input) (*Report, error) {
	svc, err := FindService(in.Project, in.Service)
	if err != nil {
		return nil, err
	}
	r := &Report{Project: in.Project.Name, Service: svc.Name, Image: svc.Image}
	cli := in.CLI

	info, err := images.Inspect(ctx, cli, svc.Image, false)
	if err != nil {
		return nil, fmt.Errorf("reading image %s: %w", svc.Image, err)
	}
	r.DataPath = svc.Env("PGDATA")
	if r.DataPath == "" {
		r.DataPath = info.PGData
	}
	if r.DataPath == "" {
		r.DataPath = "/var/lib/postgresql/data"
	}
	m, ok := dataMount(svc, r.DataPath)
	switch {
	case !ok:
		r.block("the data folder %s is not in a named volume or bind mount of the service (an anonymous volume is lost when the container is recreated); add a volume first", r.DataPath)
	case m.Type == "volume":
		r.MountType, r.MountName, r.MountPoint, r.MountKey = "volume", in.Project.VolumeName(m), m.Target, m.Source
	case m.Type == "bind":
		r.MountType, r.MountName, r.MountPoint = "bind", m.Source, m.Target
	default:
		r.block("the data folder is on a %s mount, which does not keep data", m.Type)
	}

	// Leftovers of an interrupted run come first: starting the service by
	// hand now could put two servers on one data folder.
	leftover := false
	for _, c := range []string{"old", "new"} {
		name := fmt.Sprintf("pgcarry-%s-%s-%s", in.Project.Name, svc.Name, c)
		if _, err := cli.Run(ctx, "inspect", "--type", "container", name); err == nil && !leftover {
			leftover = true
			r.block("an earlier pgcarry run was interrupted (container %s is left); run pgcarry rollback first, and do not start %s by hand until then", name, svc.Name)
		}
	}

	// The running container, if any.
	cs, err := compose.Containers(ctx, cli, in.Project.Options)
	if err != nil {
		return nil, fmt.Errorf("listing the project's containers: %w", err)
	}
	var running []compose.Container
	for _, c := range cs {
		if c.Service == svc.Name && c.State == "running" {
			running = append(running, c)
		}
	}
	switch len(running) {
	case 0:
		if !leftover {
			r.block("the %s service is not running; start it so pgcarry can read the database", svc.Name)
		}
	case 1:
		r.Container, r.Running = running[0].Name, true
	default:
		r.block("the %s service has %d running containers; pgcarry upgrades a single server", svc.Name, len(running))
	}

	// A volume on another driver, or with options (a bind to a chosen disk,
	// NFS), needs the same for the new volume; v1 does not make those.
	if r.MountType == "volume" {
		out, err := cli.Run(ctx, "volume", "inspect", "-f", "{{.Driver}}|{{json .Options}}", r.MountName)
		if err == nil {
			driver, opts, _ := strings.Cut(strings.TrimSpace(out), "|")
			if driver != "local" || (opts != "null" && opts != "{}" && opts != "") {
				r.block("volume %s uses driver %s with options %s; pgcarry cannot yet make a matching new volume, so it does not upgrade this one (move the data to a plain named volume first, or upgrade by hand)", r.MountName, driver, opts)
			}
		}
	}

	// The container must really use the data the Compose files describe.
	if r.Running && r.MountType != "" {
		kind, src, err := actualMount(ctx, cli, r.Container, r.MountPoint)
		switch {
		case err != nil:
			return nil, err
		case kind != r.MountType || filepath.Clean(src) != filepath.Clean(r.MountName):
			r.block("the running container has %s %q at %s, but the Compose files say %s %q; recreate the service from these files first (docker compose up -d %s)", kind, src, r.MountPoint, r.MountType, r.MountName, svc.Name)
		}
	}

	user := svc.Env("POSTGRES_USER")
	if user == "" {
		user = "postgres"
	}
	var srv *pg.Server
	if r.Running {
		srv = &pg.Server{CLI: cli, Container: r.Container, User: user}
		if v, err := srv.CLI.Run(ctx, "exec", r.Container, "cat", path.Join(r.DataPath, "PG_VERSION")); err == nil {
			r.FileMajor, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		r.Cluster, err = srv.Inspect(ctx, "postgres")
		if err != nil {
			// The "postgres" database may have been dropped.
			r.Cluster, err = srv.Inspect(ctx, "template1")
		}
		if err != nil {
			return nil, fmt.Errorf("reading the database as %s: %w", user, err)
		}
		if df, err := cli.Run(ctx, "exec", r.Container, "df", "-Pk", r.DataPath); err == nil {
			r.FreeBytes = parseDF(df)
		}
	}

	// The target.
	to := in.To
	r.TargetImage = in.Image
	if r.TargetImage == "" {
		if to == 0 {
			return nil, errors.New("say which version to move to (--to 18) or give the image (--image)")
		}
		if r.TargetImage, err = images.Target(svc.Image, to); err != nil {
			r.block("%v", err)
		}
	}
	if r.TargetImage != "" {
		tinfo, err := images.Inspect(ctx, cli, r.TargetImage, true)
		if err != nil {
			r.block("cannot get the new image: %v", err)
		} else if r.Target, err = images.ProbeImage(ctx, cli, r.TargetImage, tinfo, localeTries(r)); err != nil {
			r.block("cannot test the new image: %v", err)
		} else {
			if to != 0 && r.Target.Major() != to {
				r.block("%s runs PostgreSQL %d, not %d", r.TargetImage, r.Target.Major(), to)
			}
			r.TargetData, r.TargetMount = targetLayout(tinfo, r.Target.Major())
			if svc.Env("PGDATA") != "" {
				// The service sets PGDATA itself; it keeps the same layout.
				r.TargetData, r.TargetMount = r.DataPath, r.MountPoint
			}
		}
	}
	r.Dependents = in.Project.Dependents(svc.Name)

	if r.Cluster != nil {
		check(ctx, r, srv, in)
	}
	if r.Running && r.Target != nil && r.MountType != "" {
		prepareEdit(ctx, r, in)
	}
	return r, nil
}

// composeFiles are the files Compose read for the running service, from
// the container's labels (or the options given).
func composeFiles(ctx context.Context, cli *docker.CLI, container string, opt compose.Options) []string {
	out, err := cli.Run(ctx, "inspect", "-f", `{{index .Config.Labels "com.docker.compose.project.config_files"}}`, container)
	if err == nil {
		if v := strings.TrimSpace(out); v != "" && v != "<no value>" {
			return strings.Split(v, ",")
		}
	}
	return nil
}

func prepareEdit(ctx context.Context, r *Report, in Input) {
	r.ComposeFiles = composeFiles(ctx, in.CLI, r.Container, in.Project.Options)
	if len(r.ComposeFiles) == 0 {
		r.SwitchProblem = "pgcarry could not tell which Compose files the service was started from"
		return
	}
	major := r.Target.Major()
	req := edit.Request{Files: r.ComposeFiles, Service: r.Service, Image: r.TargetImage,
		MountTarget: r.MountPoint, NewTarget: r.TargetMount, Kind: r.MountType,
		VolumeKey: r.MountKey, NewKey: fmt.Sprintf("%s_pg%d", r.MountKey, major),
		NewName: NewVolumeName(r.MountName, major), BindSuffix: fmt.Sprintf("-pg%d", major)}
	files, err := edit.Prepare(req)
	if err != nil {
		r.SwitchProblem = err.Error()
		return
	}
	r.Edits = files
	var b strings.Builder
	for _, f := range files {
		b.WriteString(edit.Diff(f))
	}
	r.EditDiff = b.String()
}

func check(ctx context.Context, r *Report, srv *pg.Server, in Input) {
	c := r.Cluster
	for _, d := range c.Databases {
		if !d.AllowConn && d.Name != "template0" {
			r.block("database %s does not accept connections, so it cannot be dumped; allow them first (ALTER DATABASE \"%s\" ALLOW_CONNECTIONS true) or drop it if it is not needed", d.Name, strings.ReplaceAll(d.Name, `"`, `""`))
		}
	}
	if u := in.Project.Services[r.Service].User; u != "" && !numericUser.MatchString(u) {
		r.block("the service runs as user %q; pgcarry needs a numeric user (uid or uid:gid) to give the new data the same owner", u)
	}
	if c.InRecovery {
		r.block("this server is a replica (in recovery); upgrade the primary instead")
	}
	for _, ts := range c.Tablespaces {
		r.block("tablespace %q lives outside the data folder; pgcarry v1 does not move tablespaces", ts)
	}
	if r.Target != nil && r.Target.Major() <= c.Major() {
		r.block("%s runs PostgreSQL %d, which is not newer than the current %d", r.TargetImage, r.Target.Major(), c.Major())
	}
	if len(c.ReplSlots) > 0 {
		r.warn("replication slots (%s) are not carried over; recreate them on the new server", strings.Join(c.ReplSlots, ", "))
	}
	if c.MD5Passwords > 0 {
		r.warn("%d role(s) use md5 passwords, which PostgreSQL 18 deprecates; they still work, but consider resetting them with password_encryption = scram-sha-256", c.MD5Passwords)
	}
	if c.PreloadLibs != "" {
		r.warn("shared_preload_libraries = %q must also be set for the new server (pgcarry carries settings from the data folder; a setting in the service's command stays as it is)", c.PreloadLibs)
	}

	// Extensions the new image lacks.
	if r.Target != nil {
		missing := map[string][]string{}
		for _, d := range c.Databases {
			for _, e := range d.Extensions {
				if _, ok := r.Target.Extensions[e.Name]; !ok {
					missing[e.Name] = append(missing[e.Name], d.Name)
				} else if def := r.Target.Extensions[e.Name]; def != e.Version && e.Name != "plpgsql" {
					r.change("extension %s in %s goes from %s to %s", e.Name, d.Name, e.Version, def)
				}
			}
		}
		for _, name := range sortedKeys(missing) {
			r.block("extension %s (used in %s) is not available in %s; pick an image that has it (--image)", name, strings.Join(missing[name], ", "), r.TargetImage)
		}
		if c.DataChecksums != r.Target.DataChecksums {
			if r.Target.DataChecksums {
				r.change("data checksums are turned on (the new image's default)")
			} else {
				r.change("data checksums stay off (the new image's default); the current cluster has them on")
			}
		}
	}

	// Settings kept in files inside the data folder.
	if settings, err := srv.FileSettings(ctx, "postgres", c.DataDirectory); err == nil {
		for _, s := range settings {
			if !initdbDefault(s.Name) && !strings.HasSuffix(s.SourceFile, "/postgresql.auto.conf") {
				r.CustomSettings = append(r.CustomSettings, s)
			}
		}
	}
	if len(r.CustomSettings) > 0 {
		var names []string
		for _, s := range r.CustomSettings {
			names = append(names, s.Name+"="+s.Value)
		}
		r.change("settings from the data folder's config files are carried over: %s", strings.Join(names, ", "))
	}
	if auto := pg.RulesOf(srv.File(ctx, path.Join(c.DataDirectory, "postgresql.auto.conf"))); len(auto) > 0 {
		r.change("postgresql.auto.conf (ALTER SYSTEM settings) is carried over: %s", strings.Join(auto, "; "))
	}
	if hba := pg.RulesOf(srv.File(ctx, path.Join(c.DataDirectory, "pg_hba.conf"))); len(hba) > 0 {
		r.CustomHBA = customHBA(hba)
		if len(r.CustomHBA) > 0 {
			r.change("%d custom pg_hba.conf rule(s) are carried over", len(r.CustomHBA))
		}
	}

	// Disk: the new cluster needs about the current size, and the dumps
	// (written to the host) up to the same again.
	if r.FreeBytes > 0 && c.TotalSizeBytes > 0 && r.FreeBytes < c.TotalSizeBytes*11/10 {
		r.block("the disk holding the data has %s free; the new copy needs about %s", human(r.FreeBytes), human(c.TotalSizeBytes*11/10))
	}

	if r.Target != nil {
		checkLocales(r)
	}

	if r.MountType != "" && r.Target != nil {
		r.change("the service's image becomes %s", r.TargetImage)
		switch r.MountType {
		case "volume":
			r.change("data moves to a new volume %s mounted at %s; the old volume %s is kept unchanged for rollback", NewVolumeName(r.MountName, r.Target.Major()), r.TargetMount, r.MountName)
		case "bind":
			r.change("data moves to a new folder %s mounted at %s; the old folder %s is kept unchanged for rollback", NewBindPath(r.MountName, r.Target.Major()), r.TargetMount, r.MountName)
		}
	}
	if len(r.Dependents) > 0 {
		r.change("services that depend on %s are stopped during the upgrade and started again: %s", r.Service, strings.Join(r.Dependents, ", "))
	}
}

// locale is one combination of locale settings used by databases.
type locale struct{ provider, collate, ctype, locale string }

// locales lists the databases' locale combinations, in a fixed order.
func locales(c *pg.Cluster) ([]locale, map[locale][]string) {
	seen := map[locale][]string{}
	for _, d := range c.Databases {
		if d.Name == "template0" {
			continue
		}
		l := locale{d.LocaleProvider, d.Collate, d.Ctype, d.Locale}
		seen[l] = append(seen[l], d.Name)
	}
	var order []locale
	for l := range seen {
		order = append(order, l)
	}
	sort.Slice(order, func(i, j int) bool { return fmt.Sprint(order[i]) < fmt.Sprint(order[j]) })
	return order, seen
}

// localeTries are statements creating a database with each locale
// combination, run on the new image during the probe: a locale missing there
// would make the restore fail.
func localeTries(r *Report) []string {
	if r.Cluster == nil {
		return nil
	}
	order, _ := locales(r.Cluster)
	var stmts []string
	for i, l := range order {
		stmt := fmt.Sprintf("CREATE DATABASE pgcarry_locale_%d TEMPLATE template0 LC_COLLATE %s LC_CTYPE %s", i, quote(l.collate), quote(l.ctype))
		switch l.provider {
		case "icu":
			// ICU_LOCALE works on 15 and later (LOCALE is the newer spelling).
			stmt += " LOCALE_PROVIDER icu ICU_LOCALE " + quote(l.locale)
		case "builtin":
			stmt += " LOCALE_PROVIDER builtin BUILTIN_LOCALE " + quote(l.locale)
		}
		stmts = append(stmts, stmt)
	}
	return stmts
}

// checkLocales reports locale combinations the new image could not create.
func checkLocales(r *Report) {
	order, seen := locales(r.Cluster)
	for i, l := range order {
		if i < len(r.Target.TryErrors) && r.Target.TryErrors[i] != "" {
			what := l.collate
			if l.ctype != l.collate {
				what += "/" + l.ctype
			}
			if l.provider != "libc" {
				what = l.provider + " " + l.locale
			}
			r.block("database(s) %s use locale %s, which %s cannot create: %s", strings.Join(seen[l], ", "), what, r.TargetImage, r.Target.TryErrors[i])
		}
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// initdbDefault reports settings that initdb (or the official image) writes
// into postgresql.conf on every new cluster; they are not the user's own.
func initdbDefault(name string) bool {
	switch strings.ToLower(name) {
	case "listen_addresses", "max_connections", "shared_buffers", "dynamic_shared_memory_type",
		"max_wal_size", "min_wal_size", "log_timezone", "timezone", "datestyle", "lc_messages",
		"lc_monetary", "lc_numeric", "lc_time", "default_text_search_config":
		return true
	}
	return false
}

// customHBA returns the rules a fresh official-image cluster would not have.
func customHBA(rules []string) []string {
	var out []string
	for _, r := range rules {
		f := strings.Fields(r)
		std := false
		switch {
		case len(f) == 4 && f[0] == "local" && (f[1] == "all" || f[1] == "replication") && f[2] == "all":
			std = true
		case len(f) == 5 && f[0] == "host" && (f[1] == "all" || f[1] == "replication") && f[2] == "all" &&
			(f[3] == "127.0.0.1/32" || f[3] == "::1/128"):
			std = true
		case len(f) == 5 && f[0] == "host" && f[1] == "all" && f[2] == "all" && f[3] == "all":
			std = true // added by the image from POSTGRES_HOST_AUTH_METHOD
		}
		if !std {
			out = append(out, r)
		}
	}
	return out
}

// NewVolumeName is the volume pgcarry creates for the upgraded data.
func NewVolumeName(old string, major int) string {
	return fmt.Sprintf("%s_pg%d", old, major)
}

// NewBindPath is the folder pgcarry creates for upgraded data kept in a
// bind mount.
func NewBindPath(old string, major int) string {
	return fmt.Sprintf("%s-pg%d", strings.TrimSuffix(old, "/"), major)
}

func parseDF(out string) int64 {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0
	}
	kb, _ := strconv.ParseInt(f[3], 10, 64)
	return kb * 1024
}

func sortedKeys[V any](m map[string]V) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
