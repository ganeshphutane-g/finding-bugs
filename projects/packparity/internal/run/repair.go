package run

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/parity"
	"github.com/ganeshphutane-g/packparity/internal/repo"
	"github.com/ganeshphutane-g/packparity/internal/store"
)

// RepairOptions say what to repair and where to put it.
type RepairOptions struct {
	// Names are repository file names, IDs or ID prefixes (8 or more
	// characters); empty means the files the last check or sync found
	// damaged. config is repaired only when named.
	Names  []string
	DryRun bool
	// OutDir writes rebuilt files there instead of into the repository.
	OutDir string
}

// Outcomes of a repair.
const (
	Repaired       = "repaired"
	ParityRemade   = "parity-remade"
	NothingToDo    = "nothing to do"
	WouldRepair    = "would repair"
	WouldRemake    = "would remake parity"
	Failed         = "failed"
	NotInRepo      = "not in repository"
	CannotNoParity = "no parity"
)

// RepairResult is one file's outcome.
type RepairResult struct {
	Name   string `json:"name"`
	Done   string `json:"done"`
	Detail string `json:"detail,omitempty"`
}

// Failed reports whether the outcome leaves damage behind.
func (r RepairResult) Failed() bool { return r.Done == Failed || r.Done == CannotNoParity }

// Repair rebuilds damaged files.
func Repair(o Options, ro RepairOptions) ([]RepairResult, error) {
	s, unlock, err := open(&o, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
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
	names := ro.Names
	if len(names) == 0 {
		names = append([]string(nil), st.Damaged...)
	}
	var out []RepairResult
	now := o.now()
	write := !ro.DryRun && ro.OutDir == ""
	for _, n := range names {
		f, why := resolve(l.Files, n)
		if why != "" {
			if why == "not in the repository" {
				st.Damaged = dropName(st.Damaged, n)
				out = append(out, RepairResult{Name: n, Done: NotInRepo, Detail: "nothing to repair (a missing file cannot be rebuilt from parity; restic check says whether anything needs it)"})
			} else {
				out = append(out, RepairResult{Name: n, Done: Failed, Detail: why})
			}
			continue
		}
		if have[f.Name] == 0 {
			out = append(out, RepairResult{Name: f.Name, Done: CannotNoParity,
				Detail: "it has no parity (it was already damaged when sync first saw it): see restic's troubleshooting guide (restic repair packs, then repair index and snapshots)"})
			continue
		}
		r := repairFile(&o, s, f, ro, now)
		if write && (r.Done == Repaired || r.Done == ParityRemade || r.Done == NothingToDo) {
			st.Verified[f.Name] = now.Unix()
			st.Damaged = dropName(st.Damaged, f.Name)
		}
		out = append(out, r)
	}
	if !write {
		return out, nil
	}
	return out, s.SaveState(st)
}

func repairFile(o *Options, s *store.Store, f repo.File, ro RepairOptions, now time.Time) RepairResult {
	res := RepairResult{Name: f.Name}
	fail := func(format string, a ...any) RepairResult {
		res.Done, res.Detail = Failed, fmt.Sprintf(format, a...)
		return res
	}
	write := !ro.DryRun && ro.OutDir == ""
	data, ok, err := readFile(o, f)
	if err != nil {
		return fail("%s", shortErr(err))
	}
	remake := func(p parity.Params, why string) RepairResult {
		if !write {
			res.Done, res.Detail = WouldRemake, why
			return res
		}
		if err := addParity(s, &store.State{Verified: map[string]int64{}}, p, f.Name, data, now); err != nil {
			return fail("%v", err)
		}
		res.Done, res.Detail = ParityRemade, why
		return res
	}
	pf, perr := readParity(s, f.Name, f.Size)
	if perr != nil && !errors.Is(perr, parity.ErrDamaged) && !errors.Is(perr, store.ErrTooBig) {
		return fail("its parity cannot be read: %s", shortErr(perr))
	}
	if perr != nil {
		if f.Name == repo.Config || ok {
			return remake(o.params(), "the file is intact; its parity was damaged and is made again")
		}
		return fail("the parity is damaged too: %v", perr)
	}
	p := parity.Params{Redundancy: pf.Redundancy(), BlockSize: parity.DefaultBlockSize}
	if h, named := nameHash(f.Name); named && h != pf.SHA256 {
		return fail("its parity belongs to another file")
	}
	if pf.Check(data).OK {
		if pf.BadParity > 0 {
			return remake(p, fmt.Sprintf("the file is intact; %d damaged parity blocks are made again", pf.BadParity))
		}
		res.Done = NothingToDo
		return res
	}
	if f.Name == repo.Config && !contains(ro.Names, repo.Config) {
		return fail("config is repaired only when named")
	}
	fixed, err := pf.Repair(data)
	if err != nil {
		return fail("%v", err)
	}
	// Repair checked the rebuilt file against the parity's SHA-256, which
	// for named files is their name; check again, to be sure.
	if h, named := nameHash(f.Name); named && sha256.Sum256(fixed) != h {
		return fail("the rebuilt file does not match its name")
	}
	switch {
	case ro.DryRun:
		res.Done, res.Detail = WouldRepair, "rebuilt and proven; nothing written"
		return res
	case ro.OutDir != "":
		dst := filepath.Join(ro.OutDir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fail("%v", err)
		}
		if err := store.WriteFile(dst, fixed, 0o600); err != nil {
			return fail("%v", err)
		}
		res.Done, res.Detail = Repaired, "written to "+dst
		return res
	}
	q := filepath.Join(s.Root, "quarantine", filepath.FromSlash(f.Name)+"."+now.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(q), 0o700); err != nil {
		return fail("%v", err)
	}
	if err := store.WriteFile(q, data, 0o400); err != nil {
		return fail("keeping the damaged copy: %v", err)
	}
	if err := putBack(o.Repo, f.Name, fixed); err != nil {
		os.Remove(q)
		if errors.Is(err, fs.ErrNotExist) {
			return fail("it was removed while repairing (pruned?)")
		}
		return fail("%v", err)
	}
	res.Done, res.Detail = Repaired, "the damaged copy is in "+q
	if pf.BadParity > 0 {
		data = fixed
		if r := remake(p, ""); r.Done == Failed {
			res.Detail += "; but its damaged parity could not be made again: " + r.Detail
		} else {
			res.Detail += fmt.Sprintf("; its %d damaged parity blocks were made again", pf.BadParity)
		}
		res.Done = Repaired
	}
	return res
}
