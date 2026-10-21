package run

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/parity"
)

// fakeRepo makes a repository-shaped directory: config, a key and n packs.
func fakeRepo(t *testing.T, n int) (string, []string) {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "config"), []byte("config v2"), 0o400)
	for _, d := range []string{"keys", "snapshots", "index", "data", "locks"} {
		os.MkdirAll(filepath.Join(root, d), 0o700)
	}
	put(t, root, "keys", 400)
	var packs []string
	for i := 0; i < n; i++ {
		packs = append(packs, put(t, root, "data", 200<<10))
	}
	return root, packs
}

// put writes a content-named file and returns its repository name.
func put(t *testing.T, root, dir string, size int) string {
	t.Helper()
	b := make([]byte, size)
	rand.Read(b)
	sum := sha256.Sum256(b)
	id := hex.EncodeToString(sum[:])
	name := dir + "/" + id
	if dir == "data" {
		name = dir + "/" + id[:2] + "/" + id
	}
	p := filepath.Join(root, filepath.FromSlash(name))
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, b, 0o400); err != nil {
		t.Fatal(err)
	}
	return name
}

func opts(root string, t *testing.T) Options {
	return Options{Repo: root, Parity: filepath.Join(t.TempDir(), "par"), Params: parity.Defaults, Init: true}
}

func flip(t *testing.T, root, name string, off int64) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	os.Chmod(p, 0o600)
	f, _ := os.OpenFile(p, os.O_RDWR, 0)
	b := []byte{0}
	f.ReadAt(b, off)
	b[0] ^= 0xff
	f.WriteAt(b, off)
	f.Close()
	os.Chmod(p, 0o400)
}

func TestSyncCheckRepair(t *testing.T) {
	root, packs := fakeRepo(t, 3)
	o := opts(root, t)
	res, err := Sync(o)
	if err != nil || res.Added != 5 || len(res.Damaged) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// Temporary and stray files are not covered.
	os.WriteFile(filepath.Join(root, "data", packs[0][5:7], "x-tmp-123"), []byte("partial"), 0o600)
	os.WriteFile(filepath.Join(root, "locks", strings.Repeat("a", 64)), []byte("lock"), 0o600)
	if res, _ := Sync(o); res.Added != 0 {
		t.Fatalf("%+v", res)
	}
	flip(t, root, packs[1], 1000)
	cr, err := Check(o, CheckOptions{All: true})
	if err != nil || cr.Worst() != Repairable || cr.Read != 5 {
		t.Fatalf("%+v %v", cr, err)
	}
	rr, err := Repair(o, RepairOptions{})
	if err != nil || len(rr) != 1 || rr[0].Done != "repaired" {
		t.Fatalf("%+v %v", rr, err)
	}
	if st, _ := os.Stat(filepath.Join(root, packs[1])); st.Mode().Perm() != 0o400 {
		t.Fatal(st.Mode())
	}
	if cr, _ := Check(o, CheckOptions{All: true}); cr.Worst() != "" {
		t.Fatalf("%+v", cr)
	}
	// A repair by ID, when the file is fine, does nothing.
	if rr, _ := Repair(o, RepairOptions{Names: []string{filepath.Base(packs[0])}}); rr[0].Done != "nothing to do" {
		t.Fatalf("%+v", rr)
	}
}

func TestNewDamagedAndYoung(t *testing.T) {
	root, packs := fakeRepo(t, 1)
	o := opts(root, t)
	flip(t, root, packs[0], 5)
	p := filepath.Join(root, packs[0])
	// Just written: skipped, perhaps still being written.
	res, err := Sync(o)
	if err != nil || len(res.Skipped) != 1 || len(res.Damaged) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	res, _ = Sync(o)
	if len(res.Damaged) != 1 || res.Added != 0 {
		t.Fatalf("%+v", res)
	}
	// check keeps reporting it as damage nothing can repair.
	cr, _ := Check(o, CheckOptions{All: true})
	if cr.Worst() != Mismatch {
		t.Fatalf("%+v", cr)
	}
	if st, _ := GetStatus(o); len(st.DamagedNoParity) != 1 {
		t.Fatalf("%+v", st)
	}
	if rr, _ := Repair(o, RepairOptions{}); len(rr) != 1 || rr[0].Done != CannotNoParity {
		t.Fatalf("%+v", rr)
	}
	// Once restic has dealt with it (the file is gone), it is settled.
	os.Remove(p)
	Sync(o)
	if st, _ := GetStatus(o); len(st.DamagedNoParity) != 0 || len(st.Damaged) != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestRemovedAndRestored(t *testing.T) {
	root, packs := fakeRepo(t, 2)
	o := opts(root, t)
	Sync(o)
	p := filepath.Join(root, packs[0])
	b, _ := os.ReadFile(p)
	os.Remove(p)
	cr, _ := Check(o, CheckOptions{All: true})
	if cr.Worst() != "" || cr.Reports[0].State != Missing {
		t.Fatalf("%+v", cr)
	}
	if res, _ := Sync(o); res.Removed != 1 {
		t.Fatalf("%+v", res)
	}
	// Back again (a disk that came back): the parity is restored, not remade.
	os.WriteFile(p, b, 0o400)
	if res, _ := Sync(o); res.Restored != 1 || res.Added != 0 {
		t.Fatalf("%+v", res)
	}
	// Purged after keep-removed.
	os.Remove(p)
	Sync(o)
	o.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if res, _ := Sync(o); res.Purged != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestGuards(t *testing.T) {
	root, packs := fakeRepo(t, 30)
	o := opts(root, t)
	Sync(o)
	// Most packs gone (a prune that repacked): retired as usual.
	for _, p := range packs[:25] {
		os.Remove(filepath.Join(root, p))
	}
	if res, err := Sync(o); err != nil || res.Removed != 25 || res.HeldBack != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// data/ emptied (an unmounted disk): retiring is held back, while new
	// files elsewhere are still covered.
	data := filepath.Join(root, "data")
	os.Rename(data, data+".away")
	os.Mkdir(data, 0o700)
	put(t, root, "index", 1000)
	res, err := Sync(o)
	if err != nil || res.HeldBack != 5 || res.EmptyFolder != "data" || res.Removed != 0 || res.Added != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// Back again: nothing was lost.
	os.Remove(data)
	os.Rename(data+".away", data)
	if res, err := Sync(o); err != nil || res.HeldBack != 0 || res.Removed != 0 || res.Added != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// A parity directory that is not set up is not started without --init
	// (an unmounted disk).
	o.Init = false
	o.Parity = filepath.Join(t.TempDir(), "unmounted")
	if _, err := Sync(o); err == nil || !strings.Contains(err.Error(), "--init") {
		t.Fatal(err)
	}
	o.Init = true
	// Not a repository.
	o.Repo = t.TempDir()
	if _, err := Sync(o); err == nil || !strings.Contains(err.Error(), "not a restic repository") {
		t.Fatal(err)
	}
	// A parity directory must be empty or packparity's.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "notes.txt"), []byte("x"), 0o600)
	o.Repo, o.Parity = root, other
	if _, err := Sync(o); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatal(err)
	}
}

func TestConfigChanged(t *testing.T) {
	root, _ := fakeRepo(t, 1)
	o := opts(root, t)
	Sync(o)
	cfg := filepath.Join(root, "config")
	os.Chmod(cfg, 0o600)
	os.WriteFile(cfg, []byte("config v3, migrated"), 0o400)
	put(t, root, "data", 1000)
	// Reported, while new files are still covered (it is the same
	// repository: its files are there).
	res, err := Sync(o)
	if err != nil || !res.ConfigChanged || res.Added != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if cr, _ := Check(o, CheckOptions{All: true}); cr.Worst() != ConfigChanged {
		t.Fatalf("%+v", cr)
	}
	// Repair without names leaves config alone.
	if rr, _ := Repair(o, RepairOptions{}); len(rr) != 0 {
		t.Fatalf("%+v", rr)
	}
	o.AcceptConfig = true
	res, _ = Sync(o)
	if res.ConfigChanged || res.Added != 1 {
		t.Fatalf("%+v", res)
	}
	// Now damage it, and repair it by name.
	os.Chmod(cfg, 0o600)
	os.WriteFile(cfg, []byte("config v3, migrateX"), 0o400)
	if rr, _ := Repair(o, RepairOptions{Names: []string{"config"}}); rr[0].Done != "repaired" {
		t.Fatalf("%+v", rr)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "config v3, migrated" {
		t.Fatal(string(b))
	}
}

func TestDamagedParity(t *testing.T) {
	root, packs := fakeRepo(t, 1)
	o := opts(root, t)
	Sync(o)
	pp := filepath.Join(o.Parity, filepath.FromSlash(packs[0])+".par")
	b, _ := os.ReadFile(pp)
	b[10] ^= 1 // the header
	os.WriteFile(pp, b, 0o600)
	cr, _ := Check(o, CheckOptions{All: true})
	if cr.Worst() != ParityDamaged {
		t.Fatalf("%+v", cr)
	}
	if rr, _ := Repair(o, RepairOptions{Names: []string{packs[0]}}); rr[0].Done != "parity-remade" {
		t.Fatalf("%+v", rr)
	}
	if cr, _ := Check(o, CheckOptions{All: true}); cr.Worst() != "" {
		t.Fatalf("%+v", cr)
	}
	// Damaged file and damaged parity: unrepairable, nothing written.
	flip(t, root, packs[0], 7)
	b, _ = os.ReadFile(pp)
	b[10] ^= 1
	os.WriteFile(pp, b, 0o600)
	cr, _ = Check(o, CheckOptions{All: true})
	if cr.Worst() != Unrepairable {
		t.Fatalf("%+v", cr)
	}
}

func TestCheckSelection(t *testing.T) {
	root, _ := fakeRepo(t, 10)
	o := opts(root, t)
	Sync(o)
	// Everything was verified by the sync: nothing is due.
	if cr, _ := Check(o, CheckOptions{}); cr.Read != 0 {
		t.Fatalf("%+v", cr)
	}
	o.Now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if cr, _ := Check(o, CheckOptions{}); cr.Read != 12 {
		t.Fatalf("%+v", cr)
	}
	o.Now = nil
	if cr, _ := Check(o, CheckOptions{Sample: 25}); cr.Read != 3 {
		t.Fatalf("%+v", cr)
	}
}

func TestForeignRepository(t *testing.T) {
	a, _ := fakeRepo(t, 2)
	b, _ := fakeRepo(t, 2)
	o := opts(a, t)
	Sync(o)
	o.Repo = b
	for name, f := range map[string]func() error{
		"sync":   func() error { _, err := Sync(o); return err },
		"check":  func() error { _, err := Check(o, CheckOptions{All: true}); return err },
		"repair": func() error { _, err := Repair(o, RepairOptions{Names: []string{"config"}}); return err },
		"status": func() error { _, err := GetStatus(o); return err },
	} {
		if err := f(); !errors.Is(err, ErrForeign) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRepairWithoutParams(t *testing.T) {
	// As the CLI calls it: no redundancy given, so the old parity's (or the
	// default) is used to make damaged parity again.
	root, packs := fakeRepo(t, 1)
	o := opts(root, t)
	o.Params.Redundancy = 25
	Sync(o)
	pp := filepath.Join(o.Parity, filepath.FromSlash(packs[0])+".par")
	b, _ := os.ReadFile(pp)
	b[len(b)-1] ^= 1 // a parity block
	os.WriteFile(pp, b, 0o600)
	o.Params = parity.Params{}
	cr, _ := Check(o, CheckOptions{All: true})
	if cr.Worst() != ParityDamaged {
		t.Fatalf("%+v", cr)
	}
	rr, err := Repair(o, RepairOptions{}) // found by check, so no names needed
	if err != nil || len(rr) != 1 || rr[0].Done != ParityRemade {
		t.Fatalf("%+v %v", rr, err)
	}
	nb, _ := os.ReadFile(pp)
	pf, err := parity.Parse(nb)
	// 25% of 50 blocks is 13 parity blocks, kept.
	if err != nil || pf.M != 13 {
		t.Fatalf("%v %d", err, pf.M)
	}
	// config's parity damaged: remade, not "config changed".
	cp := filepath.Join(o.Parity, "config.par")
	b, _ = os.ReadFile(cp)
	b[10] ^= 1
	os.WriteFile(cp, b, 0o600)
	if res, err := Sync(o); err != nil || res.ConfigChanged || !res.ConfigParityRemade {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestDueSpread(t *testing.T) {
	root, _ := fakeRepo(t, 40)
	o := opts(root, t)
	Sync(o)
	start := time.Now()
	var reads []int
	for week := 1; week <= 9; week++ {
		w := week
		o.Now = func() time.Time { return start.Add(time.Duration(w) * 7 * 24 * time.Hour) }
		cr, _ := Check(o, CheckOptions{Every: 28 * 24 * time.Hour})
		reads = append(reads, cr.Read)
	}
	// 42 files, each read once per 28 days: never all at once, and every
	// file within the first four weeks.
	total := 0
	for i, n := range reads {
		if n == 42 {
			t.Fatalf("all due at once: %v", reads)
		}
		if i < 4 {
			total += n
		}
	}
	if total != 42 {
		t.Fatalf("%v", reads)
	}
}

func TestShortIDs(t *testing.T) {
	root, packs := fakeRepo(t, 2)
	o := opts(root, t)
	Sync(o)
	flip(t, root, packs[0], 3)
	id := filepath.Base(packs[0])
	if rr, _ := Repair(o, RepairOptions{Names: []string{id[:8]}}); rr[0].Done != Repaired {
		t.Fatalf("%+v", rr)
	}
	if rr, _ := Repair(o, RepairOptions{Names: []string{"deadbeef0"}}); rr[0].Done != NotInRepo {
		t.Fatalf("%+v", rr)
	}
}

func TestStaleDamage(t *testing.T) {
	root, packs := fakeRepo(t, 2)
	o := opts(root, t)
	Sync(o)
	flip(t, root, packs[0], 3)
	Check(o, CheckOptions{All: true})
	// Pruned before anyone repaired it: no longer anyone's problem.
	os.Remove(filepath.Join(root, packs[0]))
	if rr, _ := Repair(o, RepairOptions{}); len(rr) != 1 || rr[0].Done != NotInRepo || rr[0].Failed() {
		t.Fatalf("%+v", rr)
	}
	if st, _ := GetStatus(o); len(st.Damaged) != 0 {
		t.Fatalf("%+v", st)
	}
}
