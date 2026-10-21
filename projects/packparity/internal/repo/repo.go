// Package repo lists the files of a restic repository on disk. Every file
// but config is named by the SHA-256 of its contents and never changes;
// restic and rest-server write under a temporary name and rename, so a file
// with a full name is complete.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Dirs are the directories with content-named files. locks/ is left out:
// locks come and go.
var Dirs = []string{"keys", "snapshots", "index", "data"}

// Config is the one file not named by its hash.
const Config = "config"

// File is one repository file.
type File struct {
	// Name is the path in the repository, with forward slashes:
	// data/ab/abcd..., index/..., config.
	Name    string
	Size    int64
	ModTime time.Time
}

// ID is the file's name without its directory ("" for config).
func (f File) ID() string {
	if f.Name == Config {
		return ""
	}
	return filepath.Base(f.Name)
}

func isID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ErrNotRepo means the path does not look like a restic repository.
var ErrNotRepo = errors.New("not a restic repository")

// Check makes sure root looks like a repository: an unmounted disk is an
// empty folder, and must not read as "every file was deleted".
func Check(root string) error {
	for _, p := range []string{Config, "keys", "data"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: %w (no %s; is the disk mounted?)", root, ErrNotRepo, p)
			}
			return err
		}
	}
	return nil
}

// List lists the repository's files, sorted by name. Temporary files and
// anything else not named like a restic file are left out.
func List(root string) ([]File, error) {
	if err := Check(root); err != nil {
		return nil, err
	}
	var out []File
	st, err := os.Stat(filepath.Join(root, Config))
	if err != nil {
		return nil, err
	}
	out = append(out, File{Name: Config, Size: st.Size(), ModTime: st.ModTime()})
	for _, d := range Dirs {
		base := filepath.Join(root, d)
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if p == base && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if e.IsDir() {
				// data/ has one level of two-hex-digit folders.
				if p != base && (d != "data" || filepath.Dir(p) != base) {
					return fs.SkipDir
				}
				return nil
			}
			if !e.Type().IsRegular() || !isID(e.Name()) {
				return nil
			}
			if d == "data" && filepath.Base(filepath.Dir(p)) != e.Name()[:2] {
				return nil
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil // pruned while listing
			}
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out = append(out, File{Name: filepath.ToSlash(rel), Size: info.Size(), ModTime: info.ModTime()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read reads a file and says whether its contents match its name (always
// true for config, which has no name to match).
func Read(root string, f File) (data []byte, matches bool, err error) {
	data, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Name)))
	if err != nil {
		return nil, false, err
	}
	if f.Name == Config {
		return data, true, nil
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]) == f.ID(), nil
}
