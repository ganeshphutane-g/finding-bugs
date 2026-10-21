package run

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/parity"
	"github.com/ganeshphutane-g/packparity/internal/repo"
	"github.com/ganeshphutane-g/packparity/internal/store"
)

// States of a file in a check.
const (
	OK            = "ok"
	Repairable    = "repairable"
	Unrepairable  = "unrepairable"
	ParityDamaged = "parity-damaged" // the file is fine, its parity is not
	ConfigChanged = "config-changed" // config differs from the one the parity has
	Missing       = "missing"        // parity but no file (pruned, or lost)
	NoParity      = "no-parity"      // a file sync has not covered yet
	Mismatch      = "mismatch"       // no parity, and the contents do not match the name
	Ignored       = "ignored"        // named like a repository file, but not a regular file
)

// FileReport is one file's result.
type FileReport struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	BadBlocks int    `json:"bad_blocks,omitempty"`
	Blocks    int    `json:"blocks,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// CheckOptions say what to read.
type CheckOptions struct {
	// All reads every file; else Sample (0 to 100 percent) picks at random;
	// else the files that are due: each is read again within Every (at a
	// point between half of it and all of it, fixed by its name).
	All    bool
	Sample float64
	Every  time.Duration
	// Rand picks the sample (tests set it).
	Rand *rand.Rand
}

// CheckResult is what a check found.
type CheckResult struct {
	Read    int          `json:"read"`
	Bytes   int64        `json:"bytes"`
	Reports []FileReport `json:"reports"` // everything not OK
}

var rank = map[string]int{"": 0, Missing: 0, NoParity: 0, Ignored: 0, OK: 0,
	ParityDamaged: 1, Repairable: 1, ConfigChanged: 1, Mismatch: 2, Unrepairable: 2}

// Worst is the most serious state found ("" when all is well; missing,
// ignored and not-yet-covered files are notes, not damage).
func (r *CheckResult) Worst() string {
	worst := ""
	for _, f := range r.Reports {
		if rank[f.State] > rank[worst] {
			worst = f.State
		}
	}
	return worst
}

// Check reads files and compares them with their parity.
func Check(o Options, co CheckOptions) (*CheckResult, error) {
	s, unlock, err := open(&o, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.LoadState()
	if err != nil {
		return nil, err
	}
	now := o.now()
	l, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	files := l.Files
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	if err := sameRepository(&o, files, have); err != nil {
		return nil, err
	}
	res := &CheckResult{}
	for _, n := range l.Ignored {
		res.Reports = append(res.Reports, FileReport{Name: n, State: Ignored, Detail: "not a regular file (restic makes none): left out"})
	}
	inRepo := map[string]bool{}
	var covered []repo.File
	for _, f := range files {
		inRepo[f.Name] = true
		if have[f.Name] > 0 {
			covered = append(covered, f)
			continue
		}
		fr := FileReport{Name: f.Name, State: NoParity, Detail: "not covered yet: run packparity sync"}
		if contains(st.Damaged, f.Name) {
			// Found damaged before sync could cover it: still so?
			_, ok, err := readFile(&o, f)
			switch {
			case err == nil && ok:
				st.Damaged = dropName(st.Damaged, f.Name)
			case err == nil || !errors.Is(err, fs.ErrNotExist):
				fr.State, fr.Detail = Mismatch, "damaged, with no parity to repair it from (it was damaged before sync covered it): see restic's troubleshooting guide"
			}
		}
		res.Reports = append(res.Reports, fr)
	}
	for name := range have {
		if !inRepo[name] {
			res.Reports = append(res.Reports, FileReport{Name: name, State: Missing,
				Detail: "gone from the repository: pruned (sync will retire its parity), or lost (run restic check)"})
		}
	}
	for _, name := range append([]string(nil), st.Damaged...) {
		if !inRepo[name] {
			st.Damaged = dropName(st.Damaged, name)
		}
	}

	pick := covered
	switch {
	case co.All:
	case co.Sample > 0:
		r := co.Rand
		if r == nil {
			r = rand.New(rand.NewSource(now.UnixNano()))
		}
		n := int(float64(len(covered))*co.Sample/100 + 0.999)
		pick = nil
		for _, i := range r.Perm(len(covered))[:min(n, len(covered))] {
			pick = append(pick, covered[i])
		}
	default:
		every := co.Every
		if every <= 0 {
			every = 30 * 24 * time.Hour
		}
		pick = nil
		for _, f := range covered {
			if !now.Before(due(f.Name, st.Verified[f.Name], every)) || contains(st.Damaged, f.Name) {
				pick = append(pick, f)
			}
		}
		sort.SliceStable(pick, func(i, j int) bool { return st.Verified[pick[i].Name] < st.Verified[pick[j].Name] })
	}

	saved := now
	for _, f := range pick {
		fr, n := checkFile(&o, s, f)
		res.Read++
		res.Bytes += n
		switch fr.State {
		case OK:
			st.Verified[f.Name] = now.Unix()
			st.Damaged = dropName(st.Damaged, f.Name)
			continue
		case Missing:
			// Pruned while checking: not damage.
		case ConfigChanged:
			// Possibly restic's doing (restic migrate): repaired only when
			// named.
		default:
			st.Damaged = addName(st.Damaged, f.Name)
		}
		res.Reports = append(res.Reports, fr)
		if t := o.now(); t.Sub(saved) > time.Minute {
			if err := s.SaveState(st); err != nil {
				return nil, err
			}
			saved = t
		}
	}
	sort.Slice(res.Reports, func(i, j int) bool { return res.Reports[i].Name < res.Reports[j].Name })
	st.LastCheck = now
	return res, s.SaveState(st)
}

// checkFile checks one covered file, and returns how much it read. A file
// or parity that cannot be read is reported, not an error.
func checkFile(o *Options, s *store.Store, f repo.File) (FileReport, int64) {
	fr := FileReport{Name: f.Name}
	data, ok, err := readFile(o, f)
	if errors.Is(err, fs.ErrNotExist) {
		fr.State, fr.Detail = Missing, "gone while checking (pruned?)"
		return fr, 0
	}
	if err != nil {
		fr.State, fr.Detail = Unrepairable, "cannot be read: "+shortErr(err)
		return fr, 0
	}
	n := int64(len(data))
	pf, perr := readParity(s, f.Name, f.Size)
	if perr != nil && !errors.Is(perr, parity.ErrDamaged) && !errors.Is(perr, store.ErrTooBig) {
		fr.State, fr.Detail = Unrepairable, "its parity cannot be read: "+shortErr(perr)
		return fr, n
	}
	if perr != nil {
		if f.Name == repo.Config || ok {
			// config: the repository is this one (checked), and restic
			// reads its config or nothing would work.
			fr.State, fr.Detail = ParityDamaged, perr.Error()+"; packparity repair makes it again from the intact file"
			return fr, n
		}
		fr.State, fr.Detail = Unrepairable, "the file is damaged and so is its parity: "+perr.Error()
		return fr, n
	}
	if h, named := nameHash(f.Name); named && h != pf.SHA256 {
		fr.State, fr.Detail = Unrepairable, "its parity belongs to another file"
		return fr, n
	}
	r := pf.Check(data)
	fr.Blocks = pf.Blocks
	fr.BadBlocks = len(r.BadBlocks)
	switch {
	case r.OK && pf.BadParity > 0:
		fr.State, fr.Detail = ParityDamaged, fmt.Sprintf("%d parity blocks are damaged; packparity repair makes them again", pf.BadParity)
	case r.OK:
		fr.State = OK
	case f.Name == repo.Config:
		fr.State = ConfigChanged
		fr.Detail = "config is not the one the parity has: damaged, or changed by restic (restic migrate). " +
			"If restic changed it: packparity sync --accept-config. If it is damaged (restic cannot open the repository): packparity repair REPO PARITY config"
	case r.Repairable:
		fr.State = Repairable
		fr.Detail = describe(r, pf)
	default:
		fr.State = Unrepairable
		fr.Detail = describe(r, pf) + fmt.Sprintf("; a block group lost %d blocks, more than its %d parity blocks", r.WorstLost, pf.M)
	}
	return fr, n
}

func describe(r parity.Report, pf *parity.File) string {
	var parts []string
	if len(r.BadBlocks) > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d blocks damaged (%s)", len(r.BadBlocks), pf.Blocks, size(int64(len(r.BadBlocks))*int64(pf.BlockSize))))
	}
	if r.SizeWrong {
		parts = append(parts, "the size is wrong")
	}
	if len(parts) == 0 {
		parts = append(parts, "the contents do not match")
	}
	return strings.Join(parts, ", ")
}
