package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pgcarry/internal/docker"
)

// LoadManifest reads the manifest of an upgrade from its backup folder.
func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, "manifest.json"), err)
	}
	return &m, nil
}

func saveManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "manifest.json"), append(data, '\n'))
}

// StateDir is where pgcarry keeps backups by default:
// $XDG_STATE_HOME/pgcarry, or ~/.local/state/pgcarry.
func StateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "pgcarry")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "pgcarry")
	}
	return filepath.Join(home, ".local", "state", "pgcarry")
}

// LatestBackup finds the most recent upgrade of a project (and service, if
// given) in the default backup location, by the time it started.
func LatestBackup(project, service string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(StateDir(), project, "*", "manifest.json"))
	best, bestDir := time.Time{}, ""
	for _, f := range matches {
		d := filepath.Dir(f)
		m, err := LoadManifest(d)
		if err != nil || m.Project != project || (service != "" && m.Service != service) {
			continue
		}
		if m.Started.After(best) {
			best, bestDir = m.Started, d
		}
	}
	if bestDir == "" {
		return "", fmt.Errorf("no pgcarry upgrade of project %s found in %s; give its folder with --backup-dir", project, filepath.Join(StateDir(), project))
	}
	return bestDir, nil
}

func majorOf(version string) int {
	n := 0
	fmt.Sscan(strings.SplitN(version, ".", 2)[0], &n)
	return n
}

// CanRollback says why a rollback is not possible (nil if it is).
func CanRollback(m *Manifest) error {
	switch {
	case m.Finished.IsZero() && !m.RolledBack:
		return nil // interrupted for good: recover
	case !m.Switched:
		return errors.New("this upgrade did not switch the service; there is nothing to roll back")
	case m.RolledBack:
		return errors.New("this upgrade was already rolled back")
	case m.CleanedUp:
		return errors.New("the old data was removed by cleanup; it can only be restored from the dumps")
	}
	return nil
}

// CanCleanup says why a cleanup is not possible (nil if it is).
func CanCleanup(m *Manifest) error {
	switch {
	case !m.Switched || m.RolledBack:
		return errors.New("the service does not run on the new data; nothing to clean up")
	case m.CleanedUp:
		return errors.New("already cleaned up")
	}
	return nil
}

// Rollback puts the original Compose files back and starts the service on
// its old data, which pgcarry never changed. Anything written to the new
// server since the switch is not carried back.
func Rollback(ctx context.Context, cli *docker.CLI, dir string) error {
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	if err := CanRollback(m); err != nil {
		return err
	}
	if m.Finished.IsZero() && !m.Switched {
		return recoverRun(ctx, cli, dir, m)
	}
	type restore struct {
		path string
		data []byte
	}
	var todo []restore
	for _, f := range m.Files {
		cur, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(cur)
		if hex.EncodeToString(sum[:]) != f.NewSHA256 {
			return fmt.Errorf("%s was changed after the upgrade; put back %s by hand (it is the original)", f.Path, filepath.Join(dir, f.Backup))
		}
		orig, err := os.ReadFile(filepath.Join(dir, f.Backup))
		if err != nil {
			return err
		}
		todo = append(todo, restore{f.Path, orig})
	}
	for _, r := range todo {
		if err := writeAtomic(r.path, r.data); err != nil {
			return err
		}
	}
	if err := runCompose(ctx, cli, m.Compose, m.Project, "up", "-d", "--no-deps", m.Service); err != nil {
		return err
	}
	if err := serving(ctx, cli, m.Compose, m.Service, m.User, majorOf(m.OldVersion), m.OldSysID); err != nil {
		return err
	}
	m.RolledBack = true
	return saveManifest(dir, m)
}

// Cleanup removes the old data once the service runs on the new data. The
// dumps stay in the backup folder.
func Cleanup(ctx context.Context, cli *docker.CLI, dir string) error {
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	if err := CanCleanup(m); err != nil {
		return err
	}
	// The running service must use the new data, and nothing the old.
	if err := serving(ctx, cli, m.Compose, m.Service, m.User, majorOf(m.NewVersion), m.NewSysID); err != nil {
		return fmt.Errorf("checking the service: %w", err)
	}
	out, err := cli.Run(ctx, "ps", "-aq", "--filter", "volume="+m.OldData)
	if m.MountType == "volume" && err == nil && strings.TrimSpace(out) != "" {
		return fmt.Errorf("a container still uses volume %s (%s); remove it first", m.OldData, strings.Fields(out)[0])
	}
	switch m.MountType {
	case "volume":
		if _, err := cli.Run(ctx, "volume", "rm", m.OldData); err != nil {
			return err
		}
	case "bind":
		parent, base := filepath.Dir(m.OldData), filepath.Base(m.OldData)
		if _, err := cli.Run(ctx, "run", "--rm", "--entrypoint", "rm", "-v", parent+":/parent", m.NewImage, "-rf", "/parent/"+base); err != nil {
			return err
		}
	}
	m.CleanedUp = true
	return saveManifest(dir, m)
}

// recoverRun puts things back after a run that was killed before it could
// clean up (power loss, kill -9): helpers go, a changed Compose file is put
// back, the half-made copy is removed and the old service and apps start.
func recoverRun(ctx context.Context, cli *docker.CLI, dir string, m *Manifest) error {
	out, _ := cli.Run(ctx, "ps", "-aq", "--filter", "label=pgcarry.run="+m.RunID)
	if ids := strings.Fields(out); len(ids) > 0 {
		if _, err := cli.Run(ctx, append([]string{"rm", "-f"}, ids...)...); err != nil {
			return err
		}
	}
	restored := false
	for _, f := range m.Files {
		cur, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(cur)
		if hex.EncodeToString(sum[:]) != f.NewSHA256 {
			continue // not pgcarry's version: leave it
		}
		orig, err := os.ReadFile(filepath.Join(dir, f.Backup))
		if err != nil {
			return err
		}
		if err := writeAtomic(f.Path, orig); err != nil {
			return err
		}
		restored = true
	}
	if restored {
		if err := runCompose(ctx, cli, m.Compose, m.Project, "up", "-d", "--no-deps", m.Service); err != nil {
			return err
		}
	}
	if err := runCompose(ctx, cli, m.Compose, m.Project, append([]string{"start", m.Service}, m.Stopped...)...); err != nil {
		return err
	}
	if err := serving(ctx, cli, m.Compose, m.Service, m.User, majorOf(m.OldVersion), m.OldSysID); err != nil {
		return fmt.Errorf("%w; the new data %s was left in place", err, m.NewData)
	}
	// The half-made copy goes only when the old service serves again and
	// no container (the service included) uses it.
	if m.Created {
		users, err := containersUsing(ctx, cli, m.MountType, m.NewData)
		switch {
		case err != nil:
			return err
		case len(users) > 0:
			return fmt.Errorf("the new data %s is still used by %s; it was left in place", m.NewData, strings.Join(users, ", "))
		}
		switch m.MountType {
		case "volume":
			if _, err := cli.Run(ctx, "volume", "rm", m.NewData); err != nil {
				return err
			}
		case "bind":
			parent, base := filepath.Dir(m.NewData), filepath.Base(m.NewData)
			if _, err := cli.Run(ctx, "run", "--rm", "--user", "0", "--entrypoint", "rm", "-v", parent+":/parent", m.NewImage, "-rf", "/parent/"+base); err != nil {
				return err
			}
		}
	}
	// Temporary files a killed run left in the folder.
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "pgcarry-env-*")); len(leftovers) > 0 {
		for _, f := range leftovers {
			os.Remove(f)
		}
	}
	m.RolledBack = true
	m.Finished = time.Now().UTC()
	return saveManifest(dir, m)
}

// containersUsing lists the containers (running or not) that mount a
// volume (by name) or a host folder (by path).
func containersUsing(ctx context.Context, cli *docker.CLI, kind, data string) ([]string, error) {
	out, err := cli.Run(ctx, "ps", "-aq")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = cli.Run(ctx, append([]string{"inspect", "-f", `{{.Name}}{{range .Mounts}}|{{.Type}}={{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}{{end}}`}, ids...)...)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "|")
		for _, mnt := range parts[1:] {
			t, src, _ := strings.Cut(mnt, "=")
			if t == kind && filepath.Clean(src) == filepath.Clean(data) {
				users = append(users, strings.TrimPrefix(parts[0], "/"))
			}
		}
	}
	return users, nil
}
