// Package run is what the commands do: sync, check, repair and status.
package run

import (
	"crypto/sha256"
	"encoding/binary"
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

	"github.com/ganeshphutane-g/packparity/internal/parity"
	"github.com/ganeshphutane-g/packparity/internal/repo"
	"github.com/ganeshphutane-g/packparity/internal/store"
)

// Options are shared by the commands.
type Options struct {
	Repo   string
	Parity string
	// Params are for new parity (sync), and for parity made again when the
	// old file cannot say what it had.
	Params parity.Params
	// Init lets sync start a new parity directory.
	Init bool
	// Wait is how long to wait for another run's lock.
	Wait time.Duration
	// KeepRemoved is how long parity for gone files is kept.
	KeepRemoved time.Duration
	// AllowMassRemoval lets sync retire parity even when much of the
	// repository is gone at once.
	AllowMassRemoval bool
	// AcceptConfig takes a changed config as the new one.
	AcceptConfig bool
	// Young: a file this new whose contents do not match its name may still
	// be being written; it is skipped, not reported.
	Young time.Duration
	Now   func() time.Time
	Out   io.Writer
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) logf(format string, a ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", a...)
	}
}

func (o *Options) params() parity.Params {
	if o.Params.Redundancy == 0 {
		return parity.Defaults
	}
	return o.Params
}

// open checks the repository, opens the parity directory and takes its
// lock.
func open(o *Options, create bool) (*store.Store, func(), error) {
	if err := repo.Check(o.Repo); err != nil {
		return nil, nil, err
	}
	s, err := store.Open(o.Parity, create)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := s.Lock(o.Wait)
	if err != nil {
		return nil, nil, err
	}
	return s, unlock, nil
}

// ErrForeign means the parity directory belongs to another repository.
var ErrForeign = errors.New("the parity directory belongs to another repository")

// sameRepository: the repository shares content-named files with the
// parity (encrypted files are unique to their repository, so two
// repositories never share one). A parity directory with nothing but config
// in it says nothing either way.
func sameRepository(o *Options, files []repo.File, have map[string]int64) error {
	covered, shared := 0, 0
	for name := range have {
		if name != repo.Config {
			covered++
		}
	}
	if covered == 0 {
		return nil
	}
	for _, f := range files {
		if f.Name != repo.Config && have[f.Name] > 0 {
			shared++
		}
	}
	if shared == 0 {
		return fmt.Errorf("%w: none of the %d files it covers are in %s. Check the paths (each repository needs its own parity directory)", ErrForeign, covered, o.Repo)
	}
	return nil
}

// readParity reads and parses the parity for a repository file of the
// given size.
func readParity(s *store.Store, name string, size int64) (*parity.File, error) {
	pb, err := s.Read(name, store.MaxParity(max(size, 1<<20)))
	if err != nil {
		return nil, err
	}
	return parity.Parse(pb)
}

// configState is how config compares with its parity.
type configState int

const (
	configNoParity configState = iota
	configMatches
	configChanged
	configParityDamaged
)

func readConfig(o *Options, s *store.Store) ([]byte, configState, error) {
	data, _, err := repo.Read(o.Repo, repo.File{Name: repo.Config})
	if err != nil {
		return nil, 0, err
	}
	pf, err := readParity(s, repo.Config, int64(len(data)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return data, configNoParity, nil
	case errors.Is(err, parity.ErrDamaged), errors.Is(err, store.ErrTooBig):
		return data, configParityDamaged, nil
	case err != nil:
		return nil, 0, err
	case sha256.Sum256(data) == pf.SHA256:
		return data, configMatches, nil
	}
	return data, configChanged, nil
}

func addParity(s *store.Store, st *store.State, p parity.Params, name string, data []byte, now time.Time) error {
	pb, err := parity.Encode(data, p)
	if err != nil {
		return err
	}
	if err := s.Write(name, pb); err != nil {
		return err
	}
	st.Verified[name] = now.Unix()
	return nil
}

func addName(list []string, name string) []string {
	for _, n := range list {
		if n == name {
			return list
		}
	}
	list = append(list, name)
	sort.Strings(list)
	return list
}

func dropName(list []string, name string) []string {
	out := list[:0]
	for _, n := range list {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// nameHash is the hash a file's name gives (none for config).
func nameHash(name string) ([32]byte, bool) {
	var h [32]byte
	if name == repo.Config {
		return h, false
	}
	b, err := hex.DecodeString(filepath.Base(name))
	if err != nil || len(b) != 32 {
		return h, false
	}
	copy(h[:], b)
	return h, true
}

// due is when a file verified at v should be read again: between half of
// every and all of it later, at a point fixed by its name, so files
// covered together are not all due together.
func due(name string, v int64, every time.Duration) time.Time {
	sum := sha256.Sum256([]byte(name))
	half := int64(every / 2)
	off := int64(0)
	if half > 0 {
		off = int64(binary.LittleEndian.Uint64(sum[:8])>>1) % half
	}
	return time.Unix(v, 0).Add(time.Duration(half + off))
}

func isIOError(err error) bool { return errors.Is(err, syscall.EIO) }

// readAround reads what can be read of a file with bad sectors, a block at a
// time, leaving zeros where reads fail.
func readAround(path string, n int64) []byte {
	out := make([]byte, n)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return out
	}
	defer f.Close()
	const step = 4096
	for off := int64(0); off < n; off += step {
		f.ReadAt(out[off:min(off+step, n)], off)
	}
	return out
}

// readFile reads a repository file; a file with unreadable sectors is
// returned with zeros where the reads failed, and ok false.
func readFile(o *Options, f repo.File) (data []byte, ok bool, err error) {
	data, ok, err = repo.Read(o.Repo, f)
	if err != nil && isIOError(err) {
		return readAround(repo.Path(o.Repo, f.Name), f.Size), false, nil
	}
	return data, ok, err
}

// removeOld deletes leftover temporary files older than an hour.
func removeOld(paths []string, now time.Time) {
	for _, p := range paths {
		if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() && now.Sub(st.ModTime()) > time.Hour {
			os.Remove(p)
		}
	}
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Size formats a byte count.
func Size(n int64) string { return size(n) }

// sameDevice reports whether two paths are on one filesystem.
func sameDevice(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	x, ok1 := sa.Sys().(*syscall.Stat_t)
	y, ok2 := sb.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && x.Dev == y.Dev
}

// resolve finds a repository file by its name, its ID or a unique ID
// prefix of at least 8 characters (as restic prints them).
func resolve(files []repo.File, n string) (repo.File, string) {
	var hits []repo.File
	for _, f := range files {
		switch {
		case f.Name == n || f.ID() == n && n != "":
			return f, ""
		case len(n) >= 8 && f.ID() != "" && strings.HasPrefix(f.ID(), n):
			hits = append(hits, f)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], ""
	case 0:
		return repo.File{}, "not in the repository"
	}
	return repo.File{}, fmt.Sprintf("%d files start with %s; give more of the ID", len(hits), n)
}
