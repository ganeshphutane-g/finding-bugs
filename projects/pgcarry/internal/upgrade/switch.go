package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/compose"
	"github.com/ganeshphutane-g/pgcarry/internal/docker"
	"github.com/ganeshphutane-g/pgcarry/internal/images"
	"github.com/ganeshphutane-g/pgcarry/internal/pg"
)

// switchOver edits the Compose files as planned and starts the service on
// the new data.
func (u *run) switchOver(ctx context.Context) error {
	for _, f := range u.r.Edits {
		cur, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		if string(cur) != string(f.Old) {
			return fmt.Errorf("%s changed since the plan was made; run pgcarry again", f.Path)
		}
	}
	for i, f := range u.r.Edits {
		name := fmt.Sprintf("compose-%d-%s", i+1, filepath.Base(f.Path))
		if err := os.WriteFile(filepath.Join(u.o.BackupDir, name), f.Old, 0o600); err != nil {
			return err
		}
		sum := sha256.Sum256(f.New)
		u.m.Files = append(u.m.Files, ComposeFile{Path: f.Path, Backup: name, NewSHA256: hex.EncodeToString(sum[:])})
	}
	if err := u.writeManifest(); err != nil {
		return err
	}
	u.edited = true
	for _, f := range u.r.Edits {
		if err := writeAtomic(f.Path, f.New); err != nil {
			return err
		}
	}
	// Compose must now read exactly the new image and data.
	proj, err := compose.Load(ctx, u.cli, u.o.Project.Options)
	if err != nil {
		return err
	}
	svc := proj.Services[u.r.Service]
	if svc.Image != u.r.TargetImage {
		return fmt.Errorf("after the edit Compose reads image %q, not %q", svc.Image, u.r.TargetImage)
	}
	ok := false
	for _, m := range svc.Volumes {
		if strings.TrimSuffix(m.Target, "/") != u.r.TargetMount {
			continue
		}
		switch u.r.MountType {
		case "volume":
			ok = m.Type == "volume" && proj.VolumeName(m) == u.m.NewData
		case "bind":
			ok = m.Type == "bind" && filepath.Clean(m.Source) == filepath.Clean(u.m.NewData)
		}
	}
	if !ok {
		return fmt.Errorf("after the edit Compose does not mount %s at %s", u.m.NewData, u.r.TargetMount)
	}
	if err := u.compose(ctx, "up", "-d", "--no-deps", u.r.Service); err != nil {
		return err
	}
	if err := serving(ctx, u.cli, u.o.Project.Options, u.r.Service, u.user, u.r.Target.Major()); err != nil {
		return err
	}
	u.m.Switched = true
	return u.writeManifest()
}

// serving waits for the service and checks the major version it runs.
func serving(ctx context.Context, cli *docker.CLI, opt compose.Options, service, user string, major int) error {
	cs, err := compose.Containers(ctx, cli, opt)
	if err != nil {
		return err
	}
	var id string
	for _, c := range cs {
		if c.Service == service && c.State == "running" {
			id = c.ID
		}
	}
	if id == "" {
		return fmt.Errorf("the %s service is not running", service)
	}
	if err := images.WaitReady(ctx, cli, id, 5*time.Minute); err != nil {
		return err
	}
	srv := &pg.Server{CLI: cli, Container: id, User: user}
	v, err := srv.Query(ctx, "postgres", "select current_setting('server_version_num')")
	if err != nil {
		return err
	}
	n := 0
	fmt.Sscan(v, &n)
	if n/10000 != major {
		return fmt.Errorf("the service runs PostgreSQL %d, expected %d", n/10000, major)
	}
	return nil
}

// restoreCompose puts the original Compose files back and recreates the
// service from them.
func (u *run) restoreCompose(ctx context.Context) error {
	var errs []error
	for _, f := range u.r.Edits {
		if err := writeAtomic(f.Path, f.Old); err != nil {
			errs = append(errs, err)
		}
	}
	if err := u.compose(ctx, "up", "-d", "--no-deps", u.r.Service); err != nil {
		errs = append(errs, fmt.Errorf("recreating %s from the original Compose file: %w", u.r.Service, err))
	}
	u.m.Switched = false
	return errors.Join(errs...)
}

// writeAtomic replaces a file, keeping its permissions.
func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".pgcarry-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
