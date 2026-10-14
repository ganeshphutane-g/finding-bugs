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
	"sort"
	"strings"

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

// LatestBackup finds the newest backup folder of a switched upgrade under
// dir (pgcarry-backup-*).
func LatestBackup(dir string) (string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "pgcarry-backup-*", "manifest.json"))
	sort.Strings(matches) // the names carry the time
	for i := len(matches) - 1; i >= 0; i-- {
		d := filepath.Dir(matches[i])
		if m, err := LoadManifest(d); err == nil && m.Switched && !m.RolledBack && !m.CleanedUp {
			return d, nil
		}
	}
	return "", fmt.Errorf("no backup folder of a switched upgrade in %s; give it with --backup-dir", dir)
}

func majorOf(version string) int {
	n := 0
	fmt.Sscan(strings.SplitN(version, ".", 2)[0], &n)
	return n
}

// CanRollback says why a rollback is not possible (nil if it is).
func CanRollback(m *Manifest) error {
	switch {
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
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	if err := CanRollback(m); err != nil {
		return err
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
	if err := serving(ctx, cli, m.Compose, m.Service, m.User, majorOf(m.OldVersion)); err != nil {
		return err
	}
	m.RolledBack = true
	return saveManifest(dir, m)
}

// Cleanup removes the old data once the service runs on the new data. The
// dumps stay in the backup folder.
func Cleanup(ctx context.Context, cli *docker.CLI, dir string) error {
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	if err := CanCleanup(m); err != nil {
		return err
	}
	// The running service must use the new data, and nothing the old.
	if err := serving(ctx, cli, m.Compose, m.Service, m.User, majorOf(m.NewVersion)); err != nil {
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
