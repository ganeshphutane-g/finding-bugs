// Package store keeps the parity directory: one .par file per repository
// file, at the same path, plus removed/ (parity for files that are gone,
// kept for a while), quarantine/ (damaged files replaced by repair), a lock,
// and state.json.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	marker  = "packparity.json"
	ext     = ".par"
	removed = "removed"
)

// Store is a parity directory.
type Store struct {
	Root string
}

type markerFile struct {
	Format int    `json:"format"`
	Tool   string `json:"tool"`
}

// Open opens the parity directory at root. With create, a missing or empty
// directory is set up; a non-empty directory that is not a parity directory
// is refused.
func Open(root string, create bool) (*Store, error) {
	b, err := os.ReadFile(filepath.Join(root, marker))
	if err == nil {
		var m markerFile
		if json.Unmarshal(b, &m) != nil || m.Tool != "packparity" {
			return nil, fmt.Errorf("%s: %s is not packparity's", root, marker)
		}
		if m.Format != 1 {
			return nil, fmt.Errorf("%s: parity directory format %d is not known to this packparity", root, m.Format)
		}
		return &Store{Root: root}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if !create {
		return nil, fmt.Errorf("%s is not a parity directory (run packparity sync first)", root)
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty and is not a parity directory; use an empty or new directory", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	b, _ = json.Marshal(markerFile{Format: 1, Tool: "packparity"})
	if err := WriteFile(filepath.Join(root, marker), append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	return &Store{Root: root}, nil
}

// Path is where the parity for repository file name lives.
func (s *Store) Path(name string) string {
	return filepath.Join(s.Root, filepath.FromSlash(name)+ext)
}

// List lists the parity files, by repository file name, with their sizes.
func (s *Store) List() (map[string]int64, error) {
	out := map[string]int64{}
	if st, err := os.Stat(s.Path("config")); err == nil {
		out["config"] = st.Size()
	}
	for _, d := range []string{"keys", "snapshots", "index", "data"} {
		base := filepath.Join(s.Root, d)
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if p == base && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
				return nil
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(s.Root, p)
			out[strings.TrimSuffix(filepath.ToSlash(rel), ext)] = info.Size()
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Read reads the parity for name.
func (s *Store) Read(name string) ([]byte, error) { return os.ReadFile(s.Path(name)) }

// Write saves the parity for name.
func (s *Store) Write(name string, b []byte) error {
	p := s.Path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return WriteFile(p, b, 0o600)
}

// Remove moves the parity for name to removed/, stamped with now.
func (s *Store) Remove(name string, now time.Time) error {
	dst := filepath.Join(s.Root, removed, filepath.FromSlash(name)+ext+"."+strconv.FormatInt(now.Unix(), 10))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.Rename(s.Path(name), dst)
}

// Removed is parity kept for a file that is gone.
type Removed struct {
	Name string // the repository file name
	At   time.Time
	Path string
	Size int64
}

// ListRemoved lists removed/, oldest first.
func (s *Store) ListRemoved() ([]Removed, error) {
	var out []Removed
	base := filepath.Join(s.Root, removed)
	err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if p == base && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		i := strings.LastIndex(rel, ext+".")
		if i < 0 {
			return nil
		}
		sec, err := strconv.ParseInt(rel[i+len(ext)+1:], 10, 64)
		if err != nil {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		out = append(out, Removed{Name: rel[:i], At: time.Unix(sec, 0), Path: p, Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, err
}

// Restore puts removed parity back (the file came back: a disk that was
// briefly missing, for example).
func (s *Store) Restore(r Removed) error {
	p := s.Path(r.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.Rename(r.Path, p)
}

// WriteFile writes a file in full under a temporary name, syncs it and
// renames it into place, then syncs the directory.
func WriteFile(path string, b []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// State is what packparity remembers between runs.
type State struct {
	LastSync  time.Time `json:"last_sync,omitzero"`
	LastCheck time.Time `json:"last_check,omitzero"`
	// Verified is when each file was last read and found intact (unix
	// seconds), by repository file name.
	Verified map[string]int64 `json:"verified"`
	// Damaged are files the last sync or check found damaged, for repair.
	Damaged []string `json:"damaged,omitempty"`
}

// LoadState reads state.json (empty when there is none).
func (s *Store) LoadState() (*State, error) {
	st := &State{Verified: map[string]int64{}}
	b, err := os.ReadFile(filepath.Join(s.Root, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %v", filepath.Join(s.Root, "state.json"), err)
	}
	if st.Verified == nil {
		st.Verified = map[string]int64{}
	}
	return st, nil
}

// SaveState writes state.json.
func (s *Store) SaveState(st *State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return WriteFile(filepath.Join(s.Root, "state.json"), b, 0o600)
}
