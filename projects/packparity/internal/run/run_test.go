package run

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	return Options{Repo: root, Parity: filepath.Join(t.TempDir(), "par"), Params: parity.Defaults}
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
	cr, _ := Check(o, CheckOptions{All: true})
	if cr.Worst() != "" || len(cr.Reports) != 1 || cr.Reports[0].State != NoParity {
		t.Fatalf("%+v", cr)
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
	for _, p := range packs[:25] {
		os.Remove(filepath.Join(root, p))
	}
	if _, err := Sync(o); err == nil || !strings.Contains(err.Error(), "allow-mass-removal") {
		t.Fatal(err)
	}
	o.AllowMassRemoval = true
	if res, err := Sync(o); err != nil || res.Removed != 25 {
		t.Fatal(res, err)
	}
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
	res, err := Sync(o)
	if err != nil || !res.ConfigChanged || res.Added != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// Repair without names leaves config alone.
	if rr, _ := Repair(o, RepairOptions{}); len(rr) != 0 {
		t.Fatalf("%+v", rr)
	}
	o.AcceptConfig = true
	res, _ = Sync(o)
	if res.ConfigChanged || res.Added != 2 {
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
