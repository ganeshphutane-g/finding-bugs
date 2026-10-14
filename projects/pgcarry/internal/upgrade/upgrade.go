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

	User          string          `json:"user"`
	RunID         string          `json:"run_id"`
	Step          string          `json:"step,omitempty"` // the step under way, while running
	Created       bool            `json:"created_new_data,omitempty"`
	OldSysID      string          `json:"old_system_identifier,omitempty"`
	NewSysID      string          `json:"new_system_identifier,omitempty"`
	NewCheckpoint string          `json:"new_checkpoint,omitempty"`     // latest checkpoint of the copy when the run ended
	T1Settings    [][2]string     `json:"template1_settings,omitempty"` // role ("" for all), name=value
	T1Comment     string          `json:"template1_comment,omitempty"`
	Compose       compose.Options `json:"compose"`
	Files         []ComposeFile   `json:"compose_files,omitempty"`
	Switched      bool            `json:"switched"`
	RolledBack    bool            `json:"rolled_back,omitempty"`
	CleanedUp     bool            `json:"cleaned_up,omitempty"`
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
	oldCtr  string // the old server, restarted without a network for the dump
	created bool   // the new volume or folder was made by this run
	edited  bool   // the Compose files were changed by this run
	unlock  func() // releases the backup folder's lock
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
	u.oldCtr = fmt.Sprintf("pgcarry-%s-%s-old", r.Project, r.Service)
	u.m = Manifest{Project: r.Project, Service: r.Service, OldImage: r.Image, NewImage: r.TargetImage,
		OldVersion: r.Cluster.Version, MountType: r.MountType, OldData: r.MountName,
		NewMount: r.TargetMount, Started: time.Now().UTC(), Compose: o.Project.Options, User: u.user, RunID: u.runID}
	switch r.MountType {
	case "volume":
		u.m.NewData = plan.NewVolumeName(r.MountName, r.Target.Major())
	case "bind":
		u.m.NewData = plan.NewBindPath(r.MountName, r.Target.Major())
	}
	u.old = &pg.Server{CLI: u.cli, Container: u.oldCtr, User: u.user}

	if err := u.prepare(ctx); err != nil {
		return nil, err
	}
	// From here on the manifest says what is under way, so an interrupted
	// run can be recovered with pgcarry rollback.
	if err := u.writeManifest(); err != nil {
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
		u.m.Step = ""
		if werr := u.writeManifest(); werr != nil && err == nil {
			err = werr
		}
		if err == nil {
			m = &u.m
		}
		u.unlock()
	}()

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"stopping the apps that use the database", u.stopApps},
		{"stopping the database", u.stopOld},
		{"starting the old database without a network, so nothing else can write to it", u.startIsolated},
		{"recording what the database holds", u.recordBefore},
		{"dumping roles and databases", u.dump},
		{"stopping the old database again", u.stopIsolated},
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
		u.m.Step = s.name
		u.m.Created = u.created
		u.writeManifest()
		err := s.fn(ctx)
		if err == nil && o.hook != nil {
			err = o.hook(s.name)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("interrupted while %s: %w", s.name, context.Canceled)
			}
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
	for _, c := range []string{u.newCtr, u.oldCtr} {
		if _, err := u.cli.Run(ctx, "inspect", "--type", "container", c); err == nil {
			return fmt.Errorf("container %s is left over from an earlier run; remove it (docker rm -f %s) and try again", c, c)
		}
	}
	switch u.r.MountType {
	case "volume":
		if out, err := u.cli.Run(ctx, "volume", "inspect", "-f", `{{index .Labels "pgcarry.rehearsal"}}`, u.m.NewData); err == nil && strings.TrimSpace(out) == "1" {
			// A copy from an earlier rehearsal: replace it only if no server
			// has run on it since (same cluster, same last checkpoint).
			if u.untouchedRehearsal(ctx) {
				u.log("removing the copy left by an earlier rehearsal (%s); nothing has run on it since", u.m.NewData)
				if _, err := u.cli.Run(ctx, "volume", "rm", u.m.NewData); err != nil {
					return err
				}
			}
		}
		if _, err := u.cli.Run(ctx, "volume", "inspect", u.m.NewData); err == nil {
			return fmt.Errorf("volume %s already exists, perhaps from an earlier upgrade that was rolled back; pgcarry will not write into it. If you do not need it: docker volume rm %s", u.m.NewData, u.m.NewData)
		}
	case "bind":
		// The folder is on the Docker host, which may be another machine:
		// look through a container.
		parent, base := filepath.Dir(u.m.NewData), filepath.Base(u.m.NewData)
		out, err := u.cli.Run(ctx, append(u.helperArgs(), "--user", "0", "--entrypoint", "sh", "-v", parent+":/parent",
			u.r.TargetImage, "-c", `[ -e "/parent/$1" ] && echo exists; true`, "sh", base)...)
		if err != nil {
			return fmt.Errorf("checking %s: %w", u.m.NewData, err)
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("%s already exists; pgcarry only writes into a folder it creates (move or remove it first)", u.m.NewData)
		}
	}
	// Everything checked: only now make the backup folder.
	if err := os.MkdirAll(u.o.BackupDir, 0o700); err != nil {
		return err
	}
	unlock, err := lockDir(u.o.BackupDir)
	if err != nil {
		return err
	}
	u.unlock = unlock
	// The folder holds dumps and password hashes; keep it out of git.
	os.WriteFile(filepath.Join(u.o.BackupDir, ".gitignore"), []byte("*\n"), 0o600)
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
	opt.Name = project
	full := append([]string{"compose"}, opt.Args()...)
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
	if u.m.OldSysID, err = u.old.Query(ctx, "postgres", "select system_identifier from pg_control_system()"); err != nil {
		return err
	}
	if err := u.old.QueryJSON(ctx, "template1", `select coalesce(json_agg(json_build_array(coalesce(r.rolname, ''), cfg)), '[]')
		from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase
		left join pg_roles r on r.oid = s.setrole, unnest(s.setconfig) cfg where d.datname = 'template1'`, &u.m.T1Settings); err != nil {
		return err
	}
	if u.m.T1Comment, err = u.old.Query(ctx, "template1", "select coalesce(shobj_description(oid, 'pg_database'), '') from pg_database where datname = 'template1'"); err != nil {
		return err
	}
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
	args := append(u.helperArgs(), "--network", "container:"+u.oldCtr, "--entrypoint", tool[0])
	if u.pass != "" {
		// From a private file, so it is not on any command line.
		f, done, err := privateFile(u.o.BackupDir, "PGPASSWORD="+u.pass+"\n")
		if err != nil {
			return err
		}
		defer done()
		args = append(args, "--env-file", f)
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
			return u.helper(ctx, w, "pg_dump", "-h", "127.0.0.1", "-U", u.user, "-Fc", "-d", pg.ConnDB(d.Name))
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
		if err := u.serviceStillDown(ctx); err != nil {
			return err
		}
	}
	return nil
}

// startIsolated runs the old image on the old data with the service's own
// mounts, environment and command, but no network: no app, declared or not,
// can write while the dump is taken, so the dump holds every write the old
// server ever accepted.
func (u *run) startIsolated(ctx context.Context) error {
	args := []string{"run", "-d", "--name", u.oldCtr, "--label", "pgcarry.run=" + u.runID, "--network", "none"}
	if u.svc.User != "" {
		args = append(args, "--user", u.svc.User)
	}
	for _, m := range u.svc.Volumes {
		switch m.Type {
		case "volume":
			src := u.o.Project.VolumeName(m)
			if src == "" {
				continue // anonymous: not data pgcarry is moving
			}
			spec := src + ":" + m.Target
			if m.ReadOnly {
				spec += ":ro"
			}
			args = append(args, "-v", spec)
		case "bind":
			spec := m.Source + ":" + m.Target
			if m.ReadOnly {
				spec += ":ro"
			}
			args = append(args, "-v", spec)
		case "tmpfs":
			args = append(args, "--tmpfs", m.Target)
		}
	}
	envFile, done, err := u.envFile(func(k string) bool { return !strings.HasSuffix(k, "_FILE") })
	if err != nil {
		return err
	}
	defer done()
	args = append(args, "--env-file", envFile, u.r.Image)
	args = append(args, commandArgs(u.svc.Command)...)
	if _, err := u.cli.Run(ctx, args...); err != nil {
		return err
	}
	return images.WaitReady(ctx, u.cli, u.oldCtr, 5*time.Minute)
}

// privateFile writes content to a temporary file only the user can read.
// The file goes in the private backup folder, so a leftover after a crash
// is not in a shared temporary folder (rollback removes it).
func privateFile(dir, content string) (string, func(), error) {
	f, err := os.CreateTemp(dir, "pgcarry-env-*")
	if err != nil {
		return "", nil, err
	}
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// envFile writes the service's environment (the keys keep allows) to a
// private temporary file, so values stay off command lines.
func (u *run) envFile(keep func(string) bool) (string, func(), error) {
	f, err := os.CreateTemp(u.o.BackupDir, "pgcarry-env-*")
	if err != nil {
		return "", nil, err
	}
	for k, v := range u.svc.Environment {
		if v == nil || !keep(k) || strings.ContainsAny(*v, "\n\r") {
			continue
		}
		fmt.Fprintf(f, "%s=%s\n", k, *v)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// serviceStillDown fails if the service was started (by the user, a script
// or a restart policy) while pgcarry has the old data open in its own
// container: two servers on one data folder can corrupt it.
func (u *run) serviceStillDown(ctx context.Context) error {
	cs, err := compose.Containers(ctx, u.cli, u.o.Project.Options)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Service == u.r.Service && c.State == "running" {
			u.cli.Run(context.WithoutCancel(ctx), "stop", "-t", "60", u.oldCtr)
			return fmt.Errorf("the %s service was started while pgcarry had its data open; pgcarry stopped its own copy at once. Do not start the service during an upgrade. Check the database with the old version before trying again", u.r.Service)
		}
	}
	return nil
}

func (u *run) stopIsolated(ctx context.Context) error {
	if err := u.serviceStillDown(ctx); err != nil {
		return err
	}
	if _, err := u.cli.Run(ctx, "stop", "-t", "60", u.oldCtr); err != nil {
		return err
	}
	_, err := u.cli.Run(ctx, "rm", u.oldCtr)
	return err
}

func (u *run) stopOld(ctx context.Context) error {
	return u.compose(ctx, "stop", u.r.Service)
}

func (u *run) makeNewData(ctx context.Context) error {
	switch u.r.MountType {
	case "volume":
		key := u.r.MountKey + fmt.Sprintf("_pg%d", u.r.Target.Major())
		args := []string{"volume", "create", "--label", "pgcarry.created=" + u.m.Started.Format(time.RFC3339)}
		if !u.o.Switch {
			args = append(args, "--label", "pgcarry.rehearsal=1")
		}
		if !u.o.Project.Volumes[u.r.MountKey].External {
			// Compose adopts a volume carrying its labels as its own.
			args = append(args, "--label", "com.docker.compose.project="+u.r.Project,
				"--label", "com.docker.compose.volume="+key)
		}
		if _, err := u.cli.Run(ctx, append(args, u.m.NewData)...); err != nil {
			return err
		}
	case "bind":
		// Made on the Docker host, through a container. The image sets up
		// its own data folder inside; the postgres user must reach it.
		parent, base := filepath.Dir(u.m.NewData), filepath.Base(u.m.NewData)
		if _, err := u.cli.Run(ctx, append(u.helperArgs(), "--user", "0", "--entrypoint", "sh", "-v", parent+":/parent",
			u.r.TargetImage, "-c", `mkdir -p "/parent/$1" && chmod 755 "/parent/$1"`, "sh", base)...); err != nil {
			return fmt.Errorf("creating %s: %w", u.m.NewData, err)
		}
	}
	u.created = true
	u.m.Created = true
	if err := u.writeManifest(); err != nil {
		return err
	}
	// A service running as its own user needs the data to belong to it.
	if u.svc.User != "" {
		if _, err := u.cli.Run(ctx, append(u.helperArgs(), "--user", "0", "--entrypoint", "chown", "-v", u.mountArg(),
			u.r.TargetImage, u.svc.User, u.r.TargetMount)...); err != nil {
			return fmt.Errorf("giving the new data to user %s: %w", u.svc.User, err)
		}
	}
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
	env := "POSTGRES_USER=" + u.user + "\nPOSTGRES_PASSWORD=" + hex.EncodeToString(tmp) +
		"\nPOSTGRES_DB=postgres\nPOSTGRES_INITDB_ARGS=" + u.initdbArgs() + "\n"
	if v := u.svc.Env("PGDATA"); v != "" {
		env += "PGDATA=" + v + "\n"
	}
	if v := u.svc.Env("POSTGRES_HOST_AUTH_METHOD"); v != "" {
		env += "POSTGRES_HOST_AUTH_METHOD=" + v + "\n"
	}
	envFile, done, err := privateFile(u.o.BackupDir, env)
	if err != nil {
		return err
	}
	defer done()
	args := []string{"run", "-d", "--name", u.newCtr, "--label", "pgcarry.run=" + u.runID,
		"-v", u.mountArg(), "--env-file", envFile}
	if u.svc.User != "" {
		args = append(args, "--user", u.svc.User)
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
	if u.m.NewSysID, err = u.new.Query(ctx, "postgres", "select system_identifier from pg_control_system()"); err != nil {
		return err
	}

	// Carry the config files and settings a dump does not include, then
	// restart so settings such as shared_preload_libraries apply before the
	// restore needs them.
	changed := false
	owner := "postgres"
	if u.svc.User != "" {
		owner = u.svc.User
	}
	for _, f := range u.m.ConfigFiles {
		content, err := os.ReadFile(filepath.Join(u.o.BackupDir, f))
		if err != nil {
			return err
		}
		target := path.Join(cl.DataDirectory, f)
		if err := u.cli.Stream(ctx, strings.NewReader(string(content)), io.Discard,
			"exec", "-i", "-u", owner, u.newCtr, "sh", "-c", `cat > "$1"`, "sh", target); err != nil {
			return fmt.Errorf("writing %s: %w", f, err)
		}
		changed = true
	}
	for _, s := range u.m.Settings {
		if !settingName.MatchString(s.Name) {
			return fmt.Errorf("unexpected setting name %q", s.Name)
		}
		if _, err := u.new.Query(ctx, "postgres", fmt.Sprintf("ALTER SYSTEM SET %s = %s", s.Name, settingValue(s.Name, s.Value))); err != nil {
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
			args = []string{"pg_restore", "--exit-on-error", "-U", u.user, "-d", pg.ConnDB("template1")}
		case "postgres":
			if _, err := u.new.Query(ctx, "template1", "DROP DATABASE postgres"); err != nil {
				f.Close()
				return err
			}
			args = []string{"pg_restore", "--exit-on-error", "--create", "-U", u.user, "-d", pg.ConnDB("template1")}
		default:
			args = []string{"pg_restore", "--exit-on-error", "--create", "-U", u.user, "-d", pg.ConnDB("postgres")}
		}
		err = u.cli.Stream(ctx, f, io.Discard, append([]string{"exec", "-i", u.newCtr}, args...)...)
		f.Close()
		if err != nil {
			return fmt.Errorf("database %s: %w", d.Name, err)
		}
	}
	// template1 is restored into the existing database, so its own settings
	// and comment are set again here.
	for _, rs := range u.m.T1Settings {
		name, value, ok := strings.Cut(rs[1], "=")
		if !ok || !settingName.MatchString(name) {
			continue
		}
		stmt := "ALTER DATABASE template1"
		if rs[0] != "" {
			stmt = "ALTER ROLE " + quoteIdent(rs[0]) + " IN DATABASE template1"
		}
		if _, err := u.new.Query(ctx, "postgres", stmt+" SET "+name+" = "+settingValue(name, value)); err != nil {
			return fmt.Errorf("template1 setting %s: %w", name, err)
		}
	}
	if u.m.T1Comment != "" {
		if _, err := u.new.Query(ctx, "postgres", "COMMENT ON DATABASE template1 IS "+quoteLiteral(u.m.T1Comment)); err != nil {
			return err
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
	// Every database must be there (template0 aside).
	want := map[string]bool{}
	for _, d := range u.r.Cluster.Databases {
		if d.Name != "template0" {
			want[d.Name] = true
		}
	}
	cl, err := u.new.Inspect(ctx, "postgres")
	if err != nil {
		return err
	}
	for _, d := range cl.Databases {
		if d.Name == "template0" {
			continue
		}
		if !want[d.Name] {
			problems = append(problems, "database "+d.Name+" is new in the copy")
		}
		delete(want, d.Name)
	}
	for name := range want {
		problems = append(problems, "database "+name+" is missing in the copy")
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
	if _, err := u.cli.Run(ctx, "rm", u.newCtr); err != nil {
		return err
	}
	cd, err := u.controlData(ctx)
	if err != nil {
		return err
	}
	u.m.NewCheckpoint = cd["Latest checkpoint location"]
	return nil
}

// controlData runs pg_controldata on the new data (read-only).
func (u *run) controlData(ctx context.Context) (map[string]string, error) {
	return controlData(ctx, u.cli, u.r.TargetImage, u.m.NewData+":"+u.r.TargetMount+":ro", u.r.TargetData)
}

func controlData(ctx context.Context, cli *docker.CLI, image, mount, pgdata string) (map[string]string, error) {
	out, err := cli.Run(ctx, "run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "pg_controldata",
		"-v", mount, image, pgdata)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m, nil
}

// untouchedRehearsal reports whether the existing new volume is a copy from
// an earlier rehearsal of this service that no server has run on since.
func (u *run) untouchedRehearsal(ctx context.Context) bool {
	matches, _ := filepath.Glob(filepath.Join(StateDir(), u.r.Project, "*", "manifest.json"))
	for _, f := range matches {
		m, err := LoadManifest(filepath.Dir(f))
		if err != nil || m.NewData != u.m.NewData || m.Switched || !m.Verified || m.NewCheckpoint == "" {
			continue
		}
		cd, err := u.controlData(ctx)
		if err != nil {
			return false
		}
		// Any server start marks the cluster "in production" until a clean
		// shutdown, which writes a new checkpoint: both must be as left.
		if cd["Database system identifier"] == m.NewSysID && cd["Latest checkpoint location"] == m.NewCheckpoint &&
			cd["Database cluster state"] == "shut down" {
			if users, _ := u.cli.Run(ctx, "ps", "-aq", "--filter", "volume="+u.m.NewData); strings.TrimSpace(users) == "" {
				return true
			}
		}
	}
	return false
}

// startAgain starts the old service and the apps pgcarry stopped.
func (u *run) startAgain(ctx context.Context) error {
	if len(u.m.Stopped) > 0 {
		// A stop interrupted on pgcarry's side may still be finishing in
		// the daemon; let it end, or the start below would be a no-op.
		u.compose(ctx, append([]string{"stop"}, u.m.Stopped...)...)
	}
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

// listSettings are settings whose value is a list of names (PostgreSQL
// quotes their elements); each element must be its own literal.
var listSettings = map[string]bool{
	"search_path": true, "shared_preload_libraries": true, "session_preload_libraries": true,
	"local_preload_libraries": true, "temp_tablespaces": true, "unix_socket_directories": true,
}

// settingValue writes a setting's value for ALTER SYSTEM.
func settingValue(name, value string) string {
	if !listSettings[strings.ToLower(name)] {
		return quoteLiteral(value)
	}
	var elems []string
	for _, e := range splitList(value) {
		elems = append(elems, quoteLiteral(e))
	}
	if len(elems) == 0 {
		return "''"
	}
	return strings.Join(elems, ", ")
}

// splitList splits a list setting as SHOW prints it: comma separated,
// elements optionally in double quotes ("" inside quotes is one quote).
func splitList(v string) []string {
	var out []string
	var cur strings.Builder
	inQuote, quoted := false, false
	flush := func() {
		e := cur.String()
		if !quoted {
			e = strings.TrimSpace(e)
		}
		if e != "" || quoted {
			out = append(out, e)
		}
		cur.Reset()
		quoted = false
	}
	runes := []rune(v)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case inQuote && c == '"' && i+1 < len(runes) && runes[i+1] == '"':
			cur.WriteRune('"')
			i++
		case c == '"':
			inQuote = !inQuote
			quoted = true
		case c == ',' && !inQuote:
			flush()
		case !inQuote && (c == ' ' || c == '\t') && (quoted || cur.Len() == 0):
			// spaces around a quoted element
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}
