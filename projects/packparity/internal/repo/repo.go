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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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

// Listing is a repository's files and what else was found.
type Listing struct {
	Files []File
	// Ignored are entries named like repository files that are not regular
	// files (symlinks, devices): restic never makes them.
	Ignored []string
	// Leftovers are packparity's own temporary files from a killed repair.
	Leftovers []string
}

// TempPrefix starts the names of repair's temporary files.
const TempPrefix = ".packparity-tmp-"

// List lists the repository's files, sorted by name. restic's temporary
// files and anything else not named like a repository file are left out. A
// top-level folder or a data/xx folder may be a symlink (to another disk).
func List(root string) (*Listing, error) {
	if err := Check(root); err != nil {
		return nil, err
	}
	l := &Listing{}
	st, err := os.Lstat(filepath.Join(root, Config))
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: config is not a regular file", root)
	}
	l.Files = append(l.Files, File{Name: Config, Size: st.Size(), ModTime: st.ModTime()})
	for _, d := range Dirs {
		if err := l.dir(filepath.Join(root, d), d, d == "data"); err != nil {
			return nil, err
		}
	}
	sort.Slice(l.Files, func(i, j int) bool { return l.Files[i].Name < l.Files[j].Name })
	return l, nil
}

func (l *Listing) dir(path, rel string, sub bool) error {
	entries, err := os.ReadDir(path) // follows a symlinked folder
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(path, name)
		r := rel + "/" + name
		if sub {
			if len(name) != 2 || !isID(name+strings.Repeat("0", 62)) {
				continue
			}
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				if err := l.dir(p, r, false); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasPrefix(name, TempPrefix) {
			l.Leftovers = append(l.Leftovers, p)
			continue
		}
		if !isID(name) || rel != "data" && strings.HasPrefix(rel, "data/") && name[:2] != filepath.Base(rel) {
			continue
		}
		if !e.Type().IsRegular() {
			l.Ignored = append(l.Ignored, r)
			continue
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned while listing
		}
		if err != nil {
			return err
		}
		l.Files = append(l.Files, File{Name: r, Size: info.Size(), ModTime: info.ModTime()})
	}
	return nil
}

// Path is a repository file's path on disk.
func Path(root, name string) string { return filepath.Join(root, filepath.FromSlash(name)) }

// Read reads a file (without following a symlink in its place) and says
// whether its contents match its name (always true for config, which has
// no name to match).
func Read(root string, f File) (data []byte, matches bool, err error) {
	fh, err := os.OpenFile(Path(root, f.Name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", f.Name)
	}
	if st.Size() > MaxSize {
		return nil, false, fmt.Errorf("%s is %d bytes, more than packparity handles", f.Name, st.Size())
	}
	data, err = io.ReadAll(io.LimitReader(fh, MaxSize+1))
	if err != nil {
		return data, false, err
	}
	if f.Name == Config {
		return data, true, nil
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]) == f.ID(), nil
}

// MaxSize is the largest repository file packparity reads (restic's packs
// are at most 128 MiB).
const MaxSize = 4 << 30
