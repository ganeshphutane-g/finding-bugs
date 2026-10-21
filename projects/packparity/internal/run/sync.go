package run

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/repo"
)

// SyncResult is what a sync did.
type SyncResult struct {
	Added      int   `json:"added"`
	AddedBytes int64 `json:"added_bytes"` // repository bytes newly covered
	Removed    int   `json:"retired"`
	Restored   int   `json:"restored"`
	Purged     int   `json:"purged"`
	// Damaged are files whose contents do not match their names, or that
	// cannot be read; no parity was made for them.
	Damaged []string `json:"damaged"`
	// Skipped are new files that did not match yet (still being written?).
	Skipped []string `json:"skipped"`
	// Gone are the files whose parity was retired.
	Gone []string `json:"gone"`
	// HeldBack: this many files are gone, and EmptyFolder has none of its
	// covered files left, so their parity was kept (see AllowMassRemoval).
	HeldBack    int    `json:"held_back,omitempty"`
	EmptyFolder string `json:"empty_folder,omitempty"`
	// ConfigChanged: config is not the one the parity has.
	ConfigChanged bool `json:"config_changed,omitempty"`
	// ConfigParityRemade: config's parity was damaged and made again.
	ConfigParityRemade bool `json:"config_parity_remade,omitempty"`
	// Ignored are entries named like repository files that are not regular
	// files (symlinks and the like).
	Ignored []string `json:"ignored,omitempty"`
	// SameDisk: the parity is on the repository's own filesystem.
	SameDisk bool `json:"same_disk,omitempty"`
}

// Sync makes parity for new files and retires parity for gone ones.
func Sync(o Options) (*SyncResult, error) {
	s, unlock, err := open(&o, o.Init)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.LoadState()
	if err != nil {
		return nil, err
	}
	now := o.now()
	res := &SyncResult{Ignored: nil, SameDisk: sameDevice(o.Repo, o.Parity)}

	l, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	files := l.Files
	res.Ignored = l.Ignored
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	if err := sameRepository(&o, files, have); err != nil {
		return nil, err
	}
	removeOld(l.Leftovers, now)
	removeOld(s.Leftovers(), now)
	inRepo := map[string]repo.File{}
	for _, f := range files {
		inRepo[f.Name] = f
	}

	// Free space first: a full parity disk must not stop the clean-up.
	keep := o.KeepRemoved
	if keep == 0 {
		keep = 7 * 24 * time.Hour
	}
	rem, err := s.ListRemoved()
	if err != nil {
		return nil, err
	}
	for _, r := range rem {
		if _, back := inRepo[r.Name]; back && have[r.Name] == 0 && r.Name != repo.Config {
			// The file is back (a disk that was missing for a while).
			if err := s.Restore(r); err != nil {
				return nil, err
			}
			have[r.Name] = r.Size
			res.Restored++
			continue
		}
		if now.Sub(r.At) > keep {
			if err := os.Remove(r.Path); err != nil {
				return nil, err
			}
			res.Purged++
		}
	}

	cdata, cstate, err := readConfig(&o, s)
	if err != nil {
		return nil, err
	}
	switch {
	case cstate == configNoParity:
		if err := addParity(s, st, o.params(), repo.Config, cdata, now); err != nil {
			return nil, err
		}
		res.Added++
	case cstate == configParityDamaged:
		// The repository is this one (checked above), and restic reads its
		// config, or nothing would work: make the parity again from it.
		if err := addParity(s, st, o.params(), repo.Config, cdata, now); err != nil {
			return nil, err
		}
		res.ConfigParityRemade = true
	case cstate == configChanged && o.AcceptConfig:
		if err := s.Remove(repo.Config, now); err != nil {
			return nil, err
		}
		if err := addParity(s, st, o.params(), repo.Config, cdata, now); err != nil {
			return nil, err
		}
		res.Added++
		o.logf("config: took the changed config; the old one's parity is in removed/")
	case cstate == configChanged:
		res.ConfigChanged = true
	}

	// Retire parity for gone files (into removed/, for KeepRemoved), unless
	// a whole folder is empty where files with parity were: an unmounted
	// disk or a broken link, not a prune (which always leaves an index and
	// keys, and packs while there are snapshots).
	var gone []string
	coveredIn, presentIn := map[string]int{}, map[string]int{}
	for _, f := range files {
		top, _, _ := strings.Cut(f.Name, "/")
		presentIn[top]++
	}
	for name := range have {
		if name == repo.Config {
			continue
		}
		top, _, _ := strings.Cut(name, "/")
		coveredIn[top]++
		if _, ok := inRepo[name]; !ok {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, top := range []string{"data", "index", "keys"} {
		if coveredIn[top] > 0 && presentIn[top] == 0 && !o.AllowMassRemoval {
			res.HeldBack, res.EmptyFolder = len(gone), top
		}
	}
	if res.HeldBack == 0 {
		for _, name := range gone {
			if err := s.Remove(name, now); err != nil {
				return nil, err
			}
			delete(st.Verified, name)
			res.Removed++
		}
		res.Gone = gone
	}
	// Damage reports for files that are gone are settled.
	for _, name := range append([]string(nil), st.Damaged...) {
		if _, ok := inRepo[name]; !ok {
			st.Damaged = dropName(st.Damaged, name)
		}
	}

	young := o.Young
	if young == 0 {
		young = 10 * time.Minute
	}
	saved := now
	for _, f := range files {
		if f.Name == repo.Config || have[f.Name] > 0 {
			continue
		}
		data, ok, err := readFile(&o, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned while syncing
		}
		if err != nil {
			// Unreadable (permissions, a bad disk): report it and go on.
			res.Damaged = append(res.Damaged, f.Name+" (cannot be read: "+shortErr(err)+")")
			st.Damaged = addName(st.Damaged, f.Name)
			continue
		}
		if !ok {
			if now.Sub(f.ModTime) < young {
				res.Skipped = append(res.Skipped, f.Name)
			} else {
				res.Damaged = append(res.Damaged, f.Name)
				st.Damaged = addName(st.Damaged, f.Name)
			}
			continue
		}
		if err := addParity(s, st, o.params(), f.Name, data, now); err != nil {
			s.SaveState(st)
			return nil, err
		}
		st.Damaged = dropName(st.Damaged, f.Name)
		res.Added++
		res.AddedBytes += int64(len(data))
		if t := o.now(); t.Sub(saved) > time.Minute {
			o.logf("sync: %d files covered so far (%s)", res.Added, size(res.AddedBytes))
			if err := s.SaveState(st); err != nil {
				return nil, err
			}
			saved = t
		}
	}
	st.LastSync = now
	return res, s.SaveState(st)
}

func shortErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return strings.TrimSpace(err.Error())
}
