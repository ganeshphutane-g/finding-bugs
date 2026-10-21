// Package run is what the commands do: sync, check, repair and status.
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
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
	Params parity.Params
	// KeepRemoved is how long parity for gone files is kept.
	KeepRemoved time.Duration
	// AllowMassRemoval lets sync retire parity for more than a quarter of
	// the files at once.
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

// massRemoval: retiring more than this share of the parity at once (and
// more than massMin files) needs AllowMassRemoval.
const (
	massShare = 4 // a quarter
	massMin   = 20
)

// SyncResult is what a sync did.
type SyncResult struct {
	Added      int
	AddedBytes int64 // repository bytes newly covered
	Removed    int
	Restored   int
	Purged     int
	// Damaged are files whose contents do not match their names (no parity
	// was made for them).
	Damaged []string
	// Skipped are new files that did not match yet (still being written?).
	Skipped []string
	// ConfigChanged: config is not the one the parity has.
	ConfigChanged bool
}

func open(o *Options, create bool) (*store.Store, func(), error) {
	if err := repo.Check(o.Repo); err != nil {
		return nil, nil, err
	}
	s, err := store.Open(o.Parity, create)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, nil, err
	}
	return s, unlock, nil
}

// configMatches reads config and its parity's recorded hash.
func configMatches(o *Options, s *store.Store) (data []byte, hasParity, matches bool, err error) {
	data, _, err = repo.Read(o.Repo, repo.File{Name: repo.Config})
	if err != nil {
		return nil, false, false, err
	}
	pb, err := s.Read(repo.Config)
	if errors.Is(err, fs.ErrNotExist) {
		return data, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	pf, err := parity.Parse(pb)
	if err != nil {
		return data, true, false, nil
	}
	return data, true, sha256.Sum256(data) == pf.SHA256, nil
}

// Sync makes parity for new files and retires parity for gone ones.
func Sync(o Options) (*SyncResult, error) {
	s, unlock, err := open(&o, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.LoadState()
	if err != nil {
		return nil, err
	}
	now := o.now()
	res := &SyncResult{}

	files, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	inRepo := map[string]repo.File{}
	for _, f := range files {
		inRepo[f.Name] = f
	}

	// config first: if it is not the one the parity was made for, this may
	// be another repository, so nothing is retired.
	cdata, cpar, cok, err := configMatches(&o, s)
	if err != nil {
		return nil, err
	}
	switch {
	case !cpar:
		if err := addParity(&o, s, st, repo.Config, cdata, now); err != nil {
			return nil, err
		}
		res.Added++
	case !cok && o.AcceptConfig:
		if err := s.Remove(repo.Config, now); err != nil {
			return nil, err
		}
		if err := addParity(&o, s, st, repo.Config, cdata, now); err != nil {
			return nil, err
		}
		o.logf("config: took the changed config; the old one's parity is in removed/")
	case !cok:
		res.ConfigChanged = true
	}

	// Parity that was retired for a file that is back.
	rem, err := s.ListRemoved()
	if err != nil {
		return nil, err
	}
	for _, r := range rem {
		if _, back := inRepo[r.Name]; back && have[r.Name] == 0 && r.Name != repo.Config {
			if err := s.Restore(r); err != nil {
				return nil, err
			}
			have[r.Name] = r.Size
			res.Restored++
		}
	}

	var gone []string
	for name := range have {
		if _, ok := inRepo[name]; !ok && name != repo.Config {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 && res.ConfigChanged {
		o.logf("config changed: not retiring parity for %d gone files until that is settled", len(gone))
		gone = nil
	}
	if len(gone) > massMin && len(gone)*massShare > len(have) && !o.AllowMassRemoval {
		return nil, fmt.Errorf("%d of %d files with parity are gone from %s; that looks like the wrong repository or a damaged disk, not a prune. Check, then run again with --allow-mass-removal if it is right",
			len(gone), len(have), o.Repo)
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
		data, ok, err := repo.Read(o.Repo, f)
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned while syncing
		}
		if err != nil {
			return nil, err
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
		if err := addParity(&o, s, st, f.Name, data, now); err != nil {
			return nil, err
		}
		res.Added++
		res.AddedBytes += int64(len(data))
		if t := o.now(); t.Sub(saved) > time.Minute {
			o.logf("sync: %d files covered so far", res.Added)
			if err := s.SaveState(st); err != nil {
				return nil, err
			}
			saved = t
		}
	}
	for _, name := range gone {
		if err := s.Remove(name, now); err != nil {
			return nil, err
		}
		delete(st.Verified, name)
		st.Damaged = dropName(st.Damaged, name)
		res.Removed++
	}

	keep := o.KeepRemoved
	if keep == 0 {
		keep = 7 * 24 * time.Hour
	}
	rem, err = s.ListRemoved()
	if err != nil {
		return nil, err
	}
	for _, r := range rem {
		if now.Sub(r.At) > keep {
			if err := os.Remove(r.Path); err != nil {
				return nil, err
			}
			res.Purged++
		}
	}
	st.LastSync = now
	return res, s.SaveState(st)
}

func addParity(o *Options, s *store.Store, st *store.State, name string, data []byte, now time.Time) error {
	pb, err := parity.Encode(data, o.Params)
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

// States of a file in a check.
const (
	OK            = "ok"
	Repairable    = "repairable"
	Unrepairable  = "unrepairable"
	ParityDamaged = "parity-damaged" // the file is fine, its parity is not
	Missing       = "missing"        // parity but no file (pruned, or lost)
	NoParity      = "no-parity"      // a file sync has not covered yet
	Mismatch      = "mismatch"       // no parity, and the contents do not match the name
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
	// else files not verified within Every, oldest first.
	All    bool
	Sample float64
	Every  time.Duration
	// Rand picks the sample (tests set it).
	Rand *rand.Rand
}

// CheckResult is what a check found.
type CheckResult struct {
	Read    int
	Bytes   int64
	Reports []FileReport // everything not OK
}

// Worst is the most serious state found ("" when all is well; missing and
// not-yet-covered files are notes, not damage).
func (r *CheckResult) Worst() string {
	worst := ""
	rank := map[string]int{"": 0, Missing: 0, NoParity: 0, ParityDamaged: 1, Repairable: 1, Mismatch: 2, Unrepairable: 2}
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
	files, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	res := &CheckResult{}
	inRepo := map[string]bool{}
	var covered []repo.File
	for _, f := range files {
		inRepo[f.Name] = true
		if have[f.Name] > 0 {
			covered = append(covered, f)
		} else {
			res.Reports = append(res.Reports, FileReport{Name: f.Name, State: NoParity, Detail: "not covered yet: run packparity sync"})
		}
	}
	for name := range have {
		if !inRepo[name] {
			res.Reports = append(res.Reports, FileReport{Name: name, State: Missing,
				Detail: "gone from the repository: pruned (sync will retire its parity), or lost (run restic check)"})
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
		if every == 0 {
			every = 30 * 24 * time.Hour
		}
		pick = nil
		for _, f := range covered {
			if now.Sub(time.Unix(st.Verified[f.Name], 0)) >= every || contains(st.Damaged, f.Name) {
				pick = append(pick, f)
			}
		}
		sort.SliceStable(pick, func(i, j int) bool { return st.Verified[pick[i].Name] < st.Verified[pick[j].Name] })
	}

	for _, f := range pick {
		fr, data, err := checkFile(&o, s, f)
		if err != nil {
			return nil, err
		}
		res.Read++
		res.Bytes += int64(len(data))
		if fr.State == OK {
			st.Verified[f.Name] = now.Unix()
			st.Damaged = dropName(st.Damaged, f.Name)
			continue
		}
		// A changed config may be restic's doing (restic migrate): it is
		// repaired only when named.
		if fr.State != ParityDamaged && f.Name != repo.Config {
			st.Damaged = addName(st.Damaged, f.Name)
		}
		res.Reports = append(res.Reports, fr)
	}
	sort.Slice(res.Reports, func(i, j int) bool { return res.Reports[i].Name < res.Reports[j].Name })
	st.LastCheck = now
	return res, s.SaveState(st)
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

func checkFile(o *Options, s *store.Store, f repo.File) (FileReport, []byte, error) {
	fr := FileReport{Name: f.Name}
	data, ok, err := repo.Read(o.Repo, f)
	if errors.Is(err, fs.ErrNotExist) {
		fr.State, fr.Detail = Missing, "gone while checking (pruned?)"
		return fr, nil, nil
	}
	if err != nil {
		// An unreadable file (I/O error on a bad sector) is damage too.
		if !isIOError(err) {
			return fr, nil, err
		}
		data, ok = readAround(filepath.Join(o.Repo, filepath.FromSlash(f.Name)), f.Size), false
	}
	pb, err := s.Read(f.Name)
	if err != nil {
		return fr, nil, err
	}
	pf, perr := parity.Parse(pb)
	if perr != nil {
		if ok && f.Name != repo.Config {
			fr.State, fr.Detail = ParityDamaged, perr.Error()+"; repair makes it again from the intact file"
			return fr, data, nil
		}
		fr.State, fr.Detail = Unrepairable, "the file is damaged and so is its parity: "+perr.Error()
		return fr, data, nil
	}
	if h, named := nameHash(f.Name); named && h != pf.SHA256 {
		fr.State, fr.Detail = Unrepairable, "its parity belongs to another file"
		return fr, data, nil
	}
	r := pf.Check(data)
	fr.Blocks = pf.Blocks
	fr.BadBlocks = len(r.BadBlocks)
	switch {
	case r.OK && pf.BadParity > 0:
		fr.State, fr.Detail = ParityDamaged, fmt.Sprintf("%d parity blocks are damaged; repair makes them again", pf.BadParity)
	case r.OK:
		fr.State = OK
	case f.Name == repo.Config:
		fr.State = Repairable
		fr.Detail = "config is not the one the parity has: damaged, or changed by restic migrate. " +
			"If damaged, run packparity repair config; if restic changed it, run packparity sync --accept-config"
	case r.Repairable:
		fr.State = Repairable
		fr.Detail = describe(r, pf)
	default:
		fr.State = Unrepairable
		fr.Detail = describe(r, pf) + fmt.Sprintf("; a block group lost %d, more than its %d parity blocks", r.WorstGroup, pf.M)
	}
	return fr, data, nil
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

func isIOError(err error) bool {
	return errors.Is(err, syscall.EIO)
}

// readAround reads what can be read of a file with bad sectors, a block at a
// time, leaving zeros where reads fail.
func readAround(path string, n int64) []byte {
	out := make([]byte, n)
	f, err := os.Open(path)
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

// RepairOptions say what to repair and where to put it.
type RepairOptions struct {
	// Names are repository file names or IDs; empty means the files the
	// last check or sync found damaged.
	Names  []string
	DryRun bool
	// OutDir writes rebuilt files there instead of into the repository.
	OutDir string
}

// RepairResult is one file's outcome.
type RepairResult struct {
	Name   string `json:"name"`
	Done   string `json:"done"` // "repaired", "parity-remade", "nothing to do", "would repair", "failed"
	Detail string `json:"detail,omitempty"`
}

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
	files, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	byName := map[string]repo.File{}
	byID := map[string]repo.File{}
	for _, f := range files {
		byName[f.Name] = f
		byID[f.ID()] = f
	}
	names := ro.Names
	if len(names) == 0 {
		names = append([]string(nil), st.Damaged...)
	}
	var out []RepairResult
	now := o.now()
	for _, n := range names {
		f, ok := byName[n]
		if !ok {
			f, ok = byID[n]
		}
		if !ok {
			out = append(out, RepairResult{Name: n, Done: "failed", Detail: "not in the repository (a missing file cannot be rebuilt from parity; run restic check)"})
			continue
		}
		r := repairFile(&o, s, f, ro, now)
		if r.Done == "repaired" || r.Done == "parity-remade" || r.Done == "nothing to do" {
			st.Verified[f.Name] = now.Unix()
			st.Damaged = dropName(st.Damaged, f.Name)
		}
		out = append(out, r)
	}
	if ro.DryRun || ro.OutDir != "" {
		return out, nil
	}
	return out, s.SaveState(st)
}

func repairFile(o *Options, s *store.Store, f repo.File, ro RepairOptions, now time.Time) RepairResult {
	res := RepairResult{Name: f.Name}
	fail := func(format string, a ...any) RepairResult {
		res.Done, res.Detail = "failed", fmt.Sprintf(format, a...)
		return res
	}
	path := filepath.Join(o.Repo, filepath.FromSlash(f.Name))
	data, ok, err := repo.Read(o.Repo, f)
	if err != nil {
		if !isIOError(err) {
			return fail("%v", err)
		}
		data, ok = readAround(path, f.Size), false
	}
	pb, err := s.Read(f.Name)
	if err != nil {
		return fail("no parity: %v", err)
	}
	pf, perr := parity.Parse(pb)
	if ok && f.Name != repo.Config && (perr != nil || pf.BadParity > 0 || pf.SHA256 != sha256.Sum256(data)) {
		// The file is intact; its parity is not.
		if ro.DryRun || ro.OutDir != "" {
			res.Done, res.Detail = "would remake parity", "the file is intact"
			return res
		}
		if err := addParity(o, s, &store.State{Verified: map[string]int64{}}, f.Name, data, now); err != nil {
			return fail("%v", err)
		}
		res.Done, res.Detail = "parity-remade", "the file was intact; its parity is made again"
		return res
	}
	if perr != nil {
		return fail("the parity is damaged too: %v", perr)
	}
	if h, named := nameHash(f.Name); named && h != pf.SHA256 {
		return fail("its parity belongs to another file")
	}
	if pf.Check(data).OK {
		if pf.BadParity > 0 && !ro.DryRun && ro.OutDir == "" {
			if err := addParity(o, s, &store.State{Verified: map[string]int64{}}, f.Name, data, now); err != nil {
				return fail("%v", err)
			}
			res.Done, res.Detail = "parity-remade", "the file was intact; its parity is made again"
			return res
		}
		res.Done = "nothing to do"
		return res
	}
	fixed, err := pf.Repair(data)
	if err != nil {
		return fail("%v", err)
	}
	// Repair checked the rebuilt file against the parity's SHA-256, which
	// for named files is its name.
	if ro.DryRun {
		res.Done, res.Detail = "would repair", "rebuilt and proven; nothing written"
		return res
	}
	if ro.OutDir != "" {
		dst := filepath.Join(ro.OutDir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fail("%v", err)
		}
		if err := store.WriteFile(dst, fixed, 0o600); err != nil {
			return fail("%v", err)
		}
		res.Done, res.Detail = "repaired", "written to "+dst
		return res
	}
	q, err := putBack(s, path, f.Name, data, fixed, now)
	if err != nil {
		return fail("%v", err)
	}
	res.Done, res.Detail = "repaired", "the damaged copy is in "+q
	return res
}

// putBack keeps the damaged bytes in quarantine/, then replaces the file in
// one rename, with its mode and owner kept.
func putBack(s *store.Store, path, name string, damaged, fixed []byte, now time.Time) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	q := filepath.Join(s.Root, "quarantine", filepath.FromSlash(name)+"."+now.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(filepath.Dir(q), 0o700); err != nil {
		return "", err
	}
	if err := store.WriteFile(q, damaged, 0o400); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".packparity-tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(fixed); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(st.Mode().Perm()); err != nil {
		tmp.Close()
		return "", err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		tmp.Chown(int(sys.Uid), int(sys.Gid)) // only root can; otherwise it is already ours
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return q, nil
}

// Status is the state of the parity.
type Status struct {
	Files, Covered  int
	RepoBytes       int64
	ParityBytes     int64
	RemovedFiles    int
	RemovedBytes    int64
	Quarantined     int
	LastSync        time.Time
	LastCheck       time.Time
	OldestVerified  time.Time
	NeverVerified   int
	Damaged         []string
	ConfigMatches   bool
	ConfigHasParity bool
}

// GetStatus reads the state without reading the repository's contents
// (except config).
func GetStatus(o Options) (*Status, error) {
	s, unlock, err := open(&o, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.LoadState()
	if err != nil {
		return nil, err
	}
	files, err := repo.List(o.Repo)
	if err != nil {
		return nil, err
	}
	have, err := s.List()
	if err != nil {
		return nil, err
	}
	out := &Status{LastSync: st.LastSync, LastCheck: st.LastCheck, Damaged: st.Damaged}
	for _, f := range files {
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
	_, out.ConfigHasParity, out.ConfigMatches, err = configMatches(&o, s)
	return out, err
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
