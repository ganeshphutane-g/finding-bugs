// Package upgrade copies a Postgres service's data into a new major version:
// dump with the new version's tools, initialise a new volume, restore,
// verify. The old data is never written; on any failure the old service and
// the apps that use it are started again.
package upgrade

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/images"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
	"github.com/ganeshphutane-g/pgcarry/internal/plan"
)

// Options configure an upgrade.
type Options struct {
	CLI       *docker.CLI
	Project   *compose.Project
	Plan      *plan.Report
	BackupDir string       // where dumps and the manifest go
	Log       func(string) // progress lines
	// Switch edits the Compose file and starts the service on the new data;
	// without it the run is a rehearsal and the old service starts again.
	Switch bool

	// hook, in tests, runs after each step and can fail it.
	hook func(step string) error
}

// Manifest records an upgrade; it is written to the backup folder.
type Manifest struct {
	Project     string            `json:"project"`
	Service     string            `json:"service"`
	OldImage    string            `json:"old_image"`
	NewImage    string            `json:"new_image"`
	OldVersion  string            `json:"old_version"`
	NewVersion  string            `json:"new_version,omitempty"`
	MountType   string            `json:"mount_type"`
	OldData     string            `json:"old_data"` // volume name or host folder
	NewData     string            `json:"new_data"`
	NewMount    string            `json:"new_mount"`
	Stopped     []string          `json:"stopped_services"`
	Globals     File              `json:"globals"`
	Databases   []DumpedDB        `json:"databases"`
	Roles       []string          `json:"roles"`
	RoleSigs    map[string]string `json:"-"`
	Started     time.Time         `json:"started"`
	Finished    time.Time         `json:"finished,omitzero"`
	Verified    bool              `json:"verified"`
	ConfigFiles []string          `json:"config_files,omitempty"`
	Settings    []pg.Setting      `json:"settings,omitempty"`

	User       string          `json:"user"`
	Compose    compose.Options `json:"compose"`
	Files      []ComposeFile   `json:"compose_files,omitempty"`
	Switched   bool            `json:"switched"`
	RolledBack bool            `json:"rolled_back,omitempty"`
	CleanedUp  bool            `json:"cleaned_up,omitempty"`
}

// ComposeFile is a Compose file pgcarry changed, and its saved original.
type ComposeFile struct {
	Path      string `json:"path"`
	Backup    string `json:"backup"`     // file in the backup folder
	NewSHA256 string `json:"new_sha256"` // what pgcarry wrote
}

// File is a file in the backup folder.
type File struct {
	Name   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// DumpedDB is one database's dump and what it held.
type DumpedDB struct {
	Name      string        `json:"name"`
	Dump      File          `json:"dump"`
	Entries   int           `json:"toc_entries"`
	Inventory *pg.Inventory `json:"inventory"`
}

type run struct {
	o       Options
	cli     *docker.CLI
	r       *plan.Report
	svc     compose.Service
	m       Manifest
	user    string
	pass    string // the old server's password, if known
	runID   string // labels every container this run starts
	helpers int    // helper containers started so far
	newCtr  string // name of the container running the new server
	created bool   // the new volume or folder was made by this run
	edited  bool   // the Compose files were changed by this run
	old     *pg.Server
	new     *pg.Server
}

func (u *run) log(f string, a ...any) {
	if u.o.Log != nil {
		u.o.Log(fmt.Sprintf(f, a...))
	}
}

// Run performs the copy and leaves the new data ready but unused: the old
// service and its apps are started again (a rehearsal). It returns the
// manifest.
func Run(ctx context.Context, o Options) (m *Manifest, err error) {
	r := o.Plan
	if !r.OK() {
		return nil, fmt.Errorf("the plan has problems to solve first: %s", strings.Join(r.Blockers, "; "))
	}
	if o.Switch && (r.SwitchProblem != "" || len(r.Edits) == 0) {
		return nil, fmt.Errorf("the Compose file cannot be changed automatically (%s); rehearse with --no-switch and edit it by hand", r.SwitchProblem)
	}
	u := &run{o: o, cli: o.CLI, r: r, svc: o.Project.Services[r.Service]}
	u.user = u.svc.Env("POSTGRES_USER")
	if u.user == "" {
		u.user = "postgres"
	}
	id := make([]byte, 6)
	rand.Read(id)
	u.runID = hex.EncodeToString(id)
	u.newCtr = fmt.Sprintf("pgcarry-%s-%s-new", r.Project, r.Service)
	u.m = Manifest{Project: r.Project, Service: r.Service, OldImage: r.Image, NewImage: r.TargetImage,
		OldVersion: r.Cluster.Version, MountType: r.MountType, OldData: r.MountName,
		NewMount: r.TargetMount, Started: time.Now().UTC(), Compose: o.Project.Options, User: u.user}
	switch r.MountType {
	case "volume":
		u.m.NewData = plan.NewVolumeName(r.MountName, r.Target.Major())
	case "bind":
		u.m.NewData = plan.NewBindPath(r.MountName, r.Target.Major())
	}
	u.old = &pg.Server{CLI: u.cli, Container: r.Container, User: u.user}

	if err := u.prepare(ctx); err != nil {
		return nil, err
	}
	defer func() {
		// Whatever happened, put things back: the old service and its apps
		// run again, helpers are gone, and a half-made copy is removed.
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		u.removeContainers(bg)
		if err != nil && u.edited {
			if rerr := u.restoreCompose(bg); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
		if err != nil && u.created {
			u.removeNewData(bg)
		}
		if serr := u.startAgain(bg); serr != nil {
			err = errors.Join(err, serr)
		}
		u.m.Finished = time.Now().UTC()
		if werr := u.writeManifest(); werr != nil && err == nil {
			err = werr
		}
		if err == nil {
			m = &u.m
		}
	}()

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"stopping the apps that use the database", u.stopApps},
		{"recording what the database holds", u.recordBefore},
		{"dumping roles and databases", u.dump},
		{"checking nothing changed during the dump", u.checkUnchanged},
		{"stopping the old database", u.stopOld},
		{"creating the new data " + r.MountType, u.makeNewData},
		{"starting PostgreSQL " + fmt.Sprint(r.Target.Major()) + " on it", u.startNew},
		{"restoring roles and databases", u.restore},
		{"verifying the copy", u.verify},
		{"stopping the new database", u.stopNew},
	}
	if o.Switch {
		steps = append(steps, struct {
			name string
			fn   func(context.Context) error
		}{"switching the service to the new data", u.switchOver})
	}
	for _, s := range steps {
		start := time.Now()
		u.log("%s ...", s.name)
		err := s.fn(ctx)
		if err == nil && o.hook != nil {
			err = o.hook(s.name)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		u.log("  done in %s", time.Since(start).Round(100*time.Millisecond))
	}
	u.m.Verified = true
	return nil, nil // the deferred function fills in m
}

// prepare checks the backup folder and leftovers before anything is stopped.
func (u *run) prepare(ctx context.Context) error {
	if entries, err := os.ReadDir(u.o.BackupDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("the backup folder %s is not empty", u.o.BackupDir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(existingParent(u.o.BackupDir), &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		if free < u.r.Cluster.TotalSizeBytes {
			return fmt.Errorf("the backup folder %s has %d MiB free; the dumps may need up to %d MiB", u.o.BackupDir, free>>20, u.r.Cluster.TotalSizeBytes>>20)
		}
	}
	if _, err := u.cli.Run(ctx, "inspect", "--type", "container", u.newCtr); err == nil {
		return fmt.Errorf("container %s is left over from an earlier run; remove it (docker rm -f %s) and try again", u.newCtr, u.newCtr)
	}
	switch u.r.MountType {
	case "volume":
		if _, err := u.cli.Run(ctx, "volume", "inspect", u.m.NewData); err == nil {
			return fmt.Errorf("volume %s already exists, perhaps from an earlier upgrade that was rolled back; pgcarry will not write into it. If you do not need it: docker volume rm %s", u.m.NewData, u.m.NewData)
		}
	case "bind":
		if entries, err := os.ReadDir(u.m.NewData); err == nil && len(entries) > 0 {
			return fmt.Errorf("folder %s already exists and is not empty", u.m.NewData)
		}
	}
	// Everything checked: only now make the backup folder.
	if err := os.MkdirAll(u.o.BackupDir, 0o700); err != nil {
		return err
	}
	// The password, for servers that ask for one over TCP.
	u.pass = u.svc.Env("POSTGRES_PASSWORD")
	if f := u.svc.Env("POSTGRES_PASSWORD_FILE"); u.pass == "" && f != "" {
		if out, err := u.cli.Run(ctx, "exec", u.r.Container, "cat", f); err == nil {
			u.pass = strings.TrimRight(out, "\r\n")
		}
	}
	return nil
}

func (u *run) compose(ctx context.Context, args ...string) error {
	return runCompose(ctx, u.cli, u.o.Project.Options, u.r.Project, args...)
}

func runCompose(ctx context.Context, cli *docker.CLI, opt compose.Options, project string, args ...string) error {
	full := []string{"compose"}
	if opt.Dir != "" {
		full = append(full, "--project-directory", opt.Dir)
	}
	for _, f := range opt.Files {
		full = append(full, "-f", f)
	}
	full = append(full, "-p", project)
	_, err := cli.Run(ctx, append(full, args...)...)
	return err
}

func (u *run) stopApps(ctx context.Context) error {
	cs, err := compose.Containers(ctx, u.cli, u.o.Project.Options)
	if err != nil {
		return err
	}
	running := map[string]bool{}
	for _, c := range cs {
		if c.State == "running" {
			running[c.Service] = true
		}
	}
	var stop []string
	for _, d := range u.r.Dependents {
		if running[d] {
			stop = append(stop, d)
		}
	}
	if len(stop) == 0 {
		return nil
	}
	u.log("  stopping %s", strings.Join(stop, ", "))
	u.m.Stopped = stop
	return u.compose(ctx, append([]string{"stop"}, stop...)...)
}

// databases are the ones dumped: all that accept connections but template0.
func (u *run) databases() []pg.Database {
	var out []pg.Database
	for _, d := range u.r.Cluster.Databases {
		if d.AllowConn && d.Name != "template0" {
			out = append(out, d)
		}
	}
	return out
}

func (u *run) recordBefore(ctx context.Context) error {
	for _, d := range u.databases() {
		inv, err := u.old.Inventory(ctx, d.Name)
		if err != nil {
			return err
		}
		u.m.Databases = append(u.m.Databases, DumpedDB{Name: d.Name, Inventory: inv})
	}
	roles, err := u.old.RoleNames(ctx)
	if err != nil {
		return err
	}
	u.m.Roles = roles
	if u.m.RoleSigs, err = u.old.RoleSignatures(ctx); err != nil {
		return fmt.Errorf("reading roles (pgcarry needs to connect as a superuser): %w", err)
	}
	// Config files that a dump does not carry.
	// postgresql.auto.conf holds ALTER SYSTEM settings, including ones not
	// yet in effect.
	for _, f := range []string{"pg_hba.conf", "pg_ident.conf", "postgresql.auto.conf"} {
		content := u.old.File(ctx, path.Join(u.r.Cluster.DataDirectory, f))
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(u.o.BackupDir, f), []byte(content), 0o600); err != nil {
			return err
		}
		u.m.ConfigFiles = append(u.m.ConfigFiles, f)
	}
	// Settings from postgresql.conf itself are set again with ALTER SYSTEM;
	// those from postgresql.auto.conf come with the file.
	for _, st := range u.r.CustomSettings {
		if !strings.HasSuffix(st.SourceFile, "/postgresql.auto.conf") {
			u.m.Settings = append(u.m.Settings, st)
		}
	}
	return nil
}

// helperArgs starts a "docker run" of a short-lived helper container that
// this run can find and remove again.
func (u *run) helperArgs() []string {
	u.helpers++
	return []string{"run", "--rm", "--name", fmt.Sprintf("pgcarry-%s-%d", u.runID, u.helpers),
		"--label", "pgcarry.run=" + u.runID}
}

// removeContainers removes every container this run started.
func (u *run) removeContainers(ctx context.Context) {
	out, _ := u.cli.Run(ctx, "ps", "-aq", "--filter", "label=pgcarry.run="+u.runID)
	ids := strings.Fields(out)
	if len(ids) > 0 {
		u.cli.Run(ctx, append([]string{"rm", "-f"}, ids...)...)
	}
}

// helper runs a client tool of the new image inside the old server's
// network namespace, so it reaches the server at 127.0.0.1.
func (u *run) helper(ctx context.Context, stdout io.Writer, tool ...string) error {
	args := append(u.helperArgs(), "--network", "container:"+u.r.Container, "--entrypoint", tool[0])
	if u.pass != "" {
		args = append(args, "-e", "PGPASSWORD="+u.pass)
	}
	args = append(args, u.r.TargetImage)
	args = append(args, tool[1:]...)
	return u.cli.Stream(ctx, nil, stdout, args...)
}

// writeFile streams into a new backup file and records its size and hash.
func (u *run) writeFile(name string, fill func(io.Writer) error) (File, error) {
	f, err := os.OpenFile(filepath.Join(u.o.BackupDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	cw := &countWriter{}
	err = fill(io.MultiWriter(f, h, cw))
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return File{Name: name, Bytes: cw.n, SHA256: hex.EncodeToString(h.Sum(nil))}, err
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func (u *run) dump(ctx context.Context) error {
	var err error
	u.m.Globals, err = u.writeFile("globals.sql", func(w io.Writer) error {
		return u.helper(ctx, w, "pg_dumpall", "-h", "127.0.0.1", "-U", u.user, "--globals-only")
	})
	if err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	for i := range u.m.Databases {
		d := &u.m.Databases[i]
		name := fmt.Sprintf("db-%02d.dump", i+1)
		d.Dump, err = u.writeFile(name, func(w io.Writer) error {
			return u.helper(ctx, w, "pg_dump", "-h", "127.0.0.1", "-U", u.user, "-Fc", "-d", d.Name)
		})
		if err != nil {
			return fmt.Errorf("database %s: %w", d.Name, err)
		}
		// The dump must be a complete archive pg_restore can read.
		f, err := os.Open(filepath.Join(u.o.BackupDir, name))
		if err != nil {
			return err
		}
		var toc strings.Builder
		err = u.cli.Stream(ctx, f, &toc, append(u.helperArgs(), "-i", "--entrypoint", "pg_restore", u.r.TargetImage, "--list")...)
		f.Close()
		if err != nil {
			return fmt.Errorf("the dump of %s cannot be read back: %w", d.Name, err)
		}
		for _, line := range strings.Split(toc.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, ";") {
				d.Entries++
			}
		}
		u.log("  %s: %d MiB, %d entries", d.Name, d.Dump.Bytes>>20, d.Entries)
	}
	return nil
}

// checkUnchanged counts again: a change means something still writes to
// the database (an app pgcarry did not stop), and the dump may miss it.
func (u *run) checkUnchanged(ctx context.Context) error {
	for _, d := range u.m.Databases {
		now, err := u.old.Inventory(ctx, d.Name)
		if err != nil {
			return err
		}
		if diff := d.Inventory.Diff(now); len(diff) > 0 {
			return fmt.Errorf("database %s changed while it was dumped (%s); stop whatever writes to it and try again", d.Name, diff[0])
		}
	}
	return nil
}

func (u *run) stopOld(ctx context.Context) error {
	return u.compose(ctx, "stop", u.r.Service)
}

func (u *run) makeNewData(ctx context.Context) error {
	switch u.r.MountType {
	case "volume":
		key := u.r.MountKey + fmt.Sprintf("_pg%d", u.r.Target.Major())
		args := []string{"volume", "create", "--label", "pgcarry.created=" + u.m.Started.Format(time.RFC3339)}
		if !u.o.Project.Volumes[u.r.MountKey].External {
			// Compose adopts a volume carrying its labels as its own.
			args = append(args, "--label", "com.docker.compose.project="+u.r.Project,
				"--label", "com.docker.compose.volume="+key)
		}
		if _, err := u.cli.Run(ctx, append(args, u.m.NewData)...); err != nil {
			return err
		}
	case "bind":
		// The image sets up its own data folder inside; the postgres user
		// must be able to reach it.
		if err := os.MkdirAll(u.m.NewData, 0o755); err != nil {
			return err
		}
	}
	u.created = true
	return nil
}

func (u *run) removeNewData(ctx context.Context) {
	switch u.r.MountType {
	case "volume":
		u.cli.Run(ctx, "volume", "rm", u.m.NewData)
	case "bind":
		// The files belong to the container's postgres user; remove them
		// from inside a container.
		parent, base := filepath.Dir(u.m.NewData), filepath.Base(u.m.NewData)
		u.cli.Run(ctx, append(u.helperArgs(), "--entrypoint", "rm", "-v", parent+":/parent", u.r.TargetImage, "-rf", "/parent/"+base)...)
	}
}

func (u *run) mountArg() string { return u.m.NewData + ":" + u.r.TargetMount }

// initdbArgs gives the new cluster template1's encoding and locale, and the
// data checksum setting.
func (u *run) initdbArgs() string {
	var t1 *pg.Database
	for i, d := range u.r.Cluster.Databases {
		if d.Name == "template1" {
			t1 = &u.r.Cluster.Databases[i]
		}
	}
	var a []string
	if t1 != nil {
		a = append(a, "--encoding="+t1.Encoding, "--lc-collate="+t1.Collate, "--lc-ctype="+t1.Ctype)
		switch t1.LocaleProvider {
		case "icu":
			a = append(a, "--locale-provider=icu", "--icu-locale="+t1.Locale)
		case "builtin":
			a = append(a, "--locale-provider=builtin", "--builtin-locale="+t1.Locale)
		}
	}
	// Keep checksums as they were when the new image's default differs and
	// it can be chosen; turning them on is the better default from 18.
	if u.r.Cluster.DataChecksums && !u.r.Target.DataChecksums {
		a = append(a, "--data-checksums")
	}
	return strings.Join(a, " ")
}

func (u *run) startNew(ctx context.Context) error {
	tmp := make([]byte, 16)
	rand.Read(tmp)
	args := []string{"run", "-d", "--name", u.newCtr, "--label", "pgcarry.run=" + u.runID,
		"-v", u.mountArg(),
		"-e", "POSTGRES_USER=" + u.user,
		"-e", "POSTGRES_PASSWORD=" + hex.EncodeToString(tmp),
		"-e", "POSTGRES_DB=postgres",
		"-e", "POSTGRES_INITDB_ARGS=" + u.initdbArgs()}
	if v := u.svc.Env("PGDATA"); v != "" {
		args = append(args, "-e", "PGDATA="+v)
	}
	if v := u.svc.Env("POSTGRES_HOST_AUTH_METHOD"); v != "" {
		args = append(args, "-e", "POSTGRES_HOST_AUTH_METHOD="+v)
	}
	if v := u.svc.Env("POSTGRES_INITDB_WALDIR"); v != "" {
		return fmt.Errorf("POSTGRES_INITDB_WALDIR is set; pgcarry v1 does not move a separate WAL folder")
	}
	args = append(args, u.r.TargetImage)
	args = append(args, commandArgs(u.svc.Command)...)
	if _, err := u.cli.Run(ctx, args...); err != nil {
		return err
	}
	if err := images.WaitReady(ctx, u.cli, u.newCtr, 5*time.Minute); err != nil {
		return err
	}
	u.new = &pg.Server{CLI: u.cli, Container: u.newCtr, User: u.user}
	cl, err := u.new.Inspect(ctx, "postgres")
	if err != nil {
		return err
	}
	u.m.NewVersion = cl.Version

	// Carry the config files and settings a dump does not include, then
	// restart so settings such as shared_preload_libraries apply before the
	// restore needs them.
	changed := false
	for _, f := range u.m.ConfigFiles {
		content, err := os.ReadFile(filepath.Join(u.o.BackupDir, f))
		if err != nil {
			return err
		}
		target := path.Join(cl.DataDirectory, f)
		if err := u.cli.Stream(ctx, strings.NewReader(string(content)), io.Discard,
			"exec", "-i", "-u", "postgres", u.newCtr, "sh", "-c", `cat > "$1"`, "sh", target); err != nil {
			return fmt.Errorf("writing %s: %w", f, err)
		}
		changed = true
	}
	for _, s := range u.m.Settings {
		if !settingName.MatchString(s.Name) {
			return fmt.Errorf("unexpected setting name %q", s.Name)
		}
		if _, err := u.new.Query(ctx, "postgres", fmt.Sprintf("ALTER SYSTEM SET %s = %s", s.Name, quoteLiteral(s.Value))); err != nil {
			return fmt.Errorf("carrying setting %s: %w", s.Name, err)
		}
		changed = true
	}
	if changed {
		if _, err := u.cli.Run(ctx, "restart", "-t", "30", u.newCtr); err != nil {
			return err
		}
		if err := images.WaitReady(ctx, u.cli, u.newCtr, 5*time.Minute); err != nil {
			return fmt.Errorf("the new server did not start with the carried settings: %w", err)
		}
	}
	return nil
}

// commandArgs turns a Compose command into arguments.
func commandArgs(cmd any) []string {
	switch c := cmd.(type) {
	case []any:
		var out []string
		for _, x := range c {
			out = append(out, fmt.Sprint(x))
		}
		return out
	case string:
		return strings.Fields(c)
	}
	return nil
}

func (u *run) restore(ctx context.Context) error {
	// Roles. The bootstrap role already exists in the new cluster.
	globals, err := os.ReadFile(filepath.Join(u.o.BackupDir, u.m.Globals.Name))
	if err != nil {
		return err
	}
	who := regexp.QuoteMeta(u.user) + `|` + regexp.QuoteMeta(quoteIdent(u.user))
	create := regexp.MustCompile(`(?m)^CREATE ROLE (` + who + `);\n`)
	g := create.ReplaceAllString(string(globals), "")
	if err := u.cli.Stream(ctx, strings.NewReader(g), io.Discard, "exec", "-i", u.newCtr,
		"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", u.user, "-d", "postgres"); err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	if !regexp.MustCompile(`(?m)^ALTER ROLE (` + who + `) .*PASSWORD `).MatchString(g) {
		// It had no password before; drop the temporary one.
		if _, err := u.new.Query(ctx, "postgres", "ALTER ROLE "+quoteIdent(u.user)+" PASSWORD NULL"); err != nil {
			return err
		}
	}
	for _, d := range u.m.Databases {
		u.log("  %s", d.Name)
		f, err := os.Open(filepath.Join(u.o.BackupDir, d.Dump.Name))
		if err != nil {
			return err
		}
		var args []string
		switch d.Name {
		case "template1":
			// It exists already; restore what it holds into it.
			args = []string{"pg_restore", "--exit-on-error", "-U", u.user, "-d", "template1"}
		case "postgres":
			if _, err := u.new.Query(ctx, "template1", "DROP DATABASE postgres"); err != nil {
				f.Close()
				return err
			}
			args = []string{"pg_restore", "--exit-on-error", "--create", "-U", u.user, "-d", "template1"}
		default:
			args = []string{"pg_restore", "--exit-on-error", "--create", "-U", u.user, "-d", "postgres"}
		}
		err = u.cli.Stream(ctx, f, io.Discard, append([]string{"exec", "-i", u.newCtr}, args...)...)
		f.Close()
		if err != nil {
			return fmt.Errorf("database %s: %w", d.Name, err)
		}
	}
	// Planner statistics are not in dumps.
	if _, err := u.cli.Run(ctx, "exec", u.newCtr, "vacuumdb", "-U", u.user, "--all", "--analyze-only", "-q"); err != nil {
		return fmt.Errorf("analyzing: %w", err)
	}
	return nil
}

func (u *run) verify(ctx context.Context) error {
	var problems []string
	for _, d := range u.m.Databases {
		after, err := u.new.Inventory(ctx, d.Name)
		if err != nil {
			return err
		}
		for _, p := range d.Inventory.Diff(after) {
			problems = append(problems, d.Name+": "+p)
		}
		u.log("  %s: %d tables, %d rows match", d.Name, len(after.Rows), after.TotalRows())
	}
	sigs, err := u.new.RoleSignatures(ctx)
	if err != nil {
		return err
	}
	for name, before := range u.m.RoleSigs {
		switch after, ok := sigs[name]; {
		case !ok:
			problems = append(problems, "role "+name+" is missing")
		case after != before:
			problems = append(problems, fmt.Sprintf("role %s differs: %q before, %q after", name, before, after))
		}
	}
	for name := range sigs {
		if _, ok := u.m.RoleSigs[name]; !ok {
			problems = append(problems, "role "+name+" is new in the copy")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the copy differs from the original:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func (u *run) stopNew(ctx context.Context) error {
	if _, err := u.cli.Run(ctx, "stop", "-t", "60", u.newCtr); err != nil {
		return err
	}
	_, err := u.cli.Run(ctx, "rm", u.newCtr)
	return err
}

// startAgain starts the old service and the apps pgcarry stopped.
func (u *run) startAgain(ctx context.Context) error {
	svcs := append([]string{u.r.Service}, u.m.Stopped...)
	if err := u.compose(ctx, append([]string{"start"}, svcs...)...); err != nil {
		return fmt.Errorf("starting %s again: %w (start them with docker compose start)", strings.Join(svcs, ", "), err)
	}
	return nil
}

func (u *run) writeManifest() error {
	data, err := json.MarshalIndent(u.m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(u.o.BackupDir, "manifest.json"), append(data, '\n'), 0o600)
}

var settingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// quoteIdent always quotes, which is valid for any name (keywords too).
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// existingParent is path or its nearest existing parent.
func existingParent(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
