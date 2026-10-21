package run

import (
	"io/fs"
	"path/filepath"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/repo"
	"github.com/ganeshphutane-g/packparity/internal/store"
)

// Status is the state of the parity.
type Status struct {
	Files          int       `json:"files"`
	Covered        int       `json:"covered"`
	RepoBytes      int64     `json:"repo_bytes"`
	ParityBytes    int64     `json:"parity_bytes"`
	RemovedFiles   int       `json:"removed_files"`
	RemovedBytes   int64     `json:"removed_bytes"`
	Quarantined    int       `json:"quarantined"`
	LastSync       time.Time `json:"last_sync,omitzero"`
	LastCheck      time.Time `json:"last_check,omitzero"`
	OldestVerified time.Time `json:"oldest_verified,omitzero"`
	NeverVerified  int       `json:"never_verified"`
	Damaged        []string  `json:"damaged"`
	// DamagedNoParity are the damaged files parity cannot repair.
	DamagedNoParity []string `json:"damaged_no_parity"`
	Config          string   `json:"config"` // "matches", "changed", "no parity", "parity damaged"
}

// GetStatus reads the state. It takes no lock and writes nothing, so it
// works during a long run and for a user who can only read.
func GetStatus(o Options) (*Status, error) {
	if err := repo.Check(o.Repo); err != nil {
		return nil, err
	}
	s, err := store.Open(o.Parity, false)
	if err != nil {
		return nil, err
	}
	st, err := s.LoadState()
	if err != nil {
		return nil, err
	}
	l, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	if err := sameRepository(&o, l.Files, have); err != nil {
		return nil, err
	}
	out := &Status{LastSync: st.LastSync, LastCheck: st.LastCheck, Damaged: []string{}, DamagedNoParity: []string{}}
	inRepo := map[string]bool{}
	for _, f := range l.Files {
		inRepo[f.Name] = true
		out.Files++
		out.RepoBytes += f.Size
		if n := have[f.Name]; n > 0 {
			out.Covered++
			out.ParityBytes += n
			v := st.Verified[f.Name]
			if v == 0 {
				out.NeverVerified++
			} else if t := time.Unix(v, 0); out.OldestVerified.IsZero() || t.Before(out.OldestVerified) {
				out.OldestVerified = t
			}
		}
	}
	for _, n := range st.Damaged {
		switch {
		case !inRepo[n]:
		case have[n] > 0:
			out.Damaged = append(out.Damaged, n)
		default:
			out.DamagedNoParity = append(out.DamagedNoParity, n)
		}
	}
	rem, err := s.ListRemoved()
	if err != nil {
		return nil, err
	}
	for _, r := range rem {
		out.RemovedFiles++
		out.RemovedBytes += r.Size
	}
	filepath.WalkDir(filepath.Join(s.Root, "quarantine"), func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			out.Quarantined++
		}
		return nil
	})
	_, cs, err := readConfig(&o, s)
	out.Config = map[configState]string{configMatches: "matches", configChanged: "changed", configNoParity: "no parity", configParityDamaged: "parity damaged"}[cs]
	return out, err
}
