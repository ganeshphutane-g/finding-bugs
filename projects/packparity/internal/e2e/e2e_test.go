// End-to-end tests with real restic and rest-server. They run when
// PACKPARITY_INTEGRATION=1; RESTIC_BIN and REST_SERVER_BIN name the binaries
// (default: restic and rest-server on PATH).
package e2e

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/packparity/internal/cli"
)

func bin(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func need(t *testing.T) {
	t.Helper()
	if os.Getenv("PACKPARITY_INTEGRATION") != "1" {
		t.Skip("set PACKPARITY_INTEGRATION=1 to run against real restic")
	}
}

type env struct {
	t      *testing.T
	repo   string // directory on disk
	url    string // what restic is given (the directory, or rest:...)
	parity string
	src    string
}

func newEnv(t *testing.T) *env {
	need(t)
	d := t.TempDir()
	e := &env{t: t, repo: filepath.Join(d, "repo"), parity: filepath.Join(d, "parity"), src: filepath.Join(d, "src")}
	e.url = e.repo
	os.MkdirAll(e.src, 0o755)
	return e
}

func (e *env) restic(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(bin("RESTIC_BIN", "restic"), append([]string{"-r", e.url, "--no-cache"}, args...)...)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD=test-password")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustRestic(args ...string) string {
	e.t.Helper()
	out, err := e.restic(args...)
	if err != nil {
		e.t.Fatalf("restic %v: %v\n%s", args, err, out)
	}
	return out
}

func (e *env) pp(args ...string) (string, int) {
	e.t.Helper()
	var out bytes.Buffer
	cmdArgs := append([]string(nil), args[:1]...)
	cmdArgs = append(cmdArgs, args[1:]...)
	cmdArgs = append(cmdArgs, e.repo, e.parity)
	if args[0] == "repair" {
		// Names go after REPO PARITY.
		var flags, names []string
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				names = append(names, a)
			}
		}
		cmdArgs = append(append(append([]string{"repair"}, flags...), e.repo, e.parity), names...)
	}
	code := cli.Main(cmdArgs, &out, &out)
	return out.String(), code
}

func (e *env) mustPP(want int, args ...string) string {
	e.t.Helper()
	out, code := e.pp(args...)
	if code != want {
		e.t.Fatalf("packparity %v: exit %d, want %d\n%s", args, code, want, out)
	}
	return out
}

func (e *env) write(name string, n int) {
	b := make([]byte, n)
	rand.Read(b)
	if err := os.WriteFile(filepath.Join(e.src, name), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// files lists the repository's files in a directory, largest first.
func (e *env) files(dir string) []string {
	var out []string
	filepath.Walk(filepath.Join(e.repo, dir), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return size(out[i]) > size(out[j]) })
	return out
}

func size(p string) int64 {
	st, _ := os.Stat(p)
	if st == nil {
		return 0
	}
	return st.Size()
}

// damage overwrites n bytes at off (negative: from the end).
func damage(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	os.Chmod(path, 0o600)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if off < 0 {
		off += size(path)
	}
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

func (e *env) restoreMatches() {
	e.t.Helper()
	dst := e.t.TempDir()
	e.mustRestic("restore", "latest", "--target", dst)
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		want, err := os.ReadFile(filepath.Join(e.src, name))
		if err != nil {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dst, e.src, name))
		if err != nil || !bytes.Equal(got, want) {
			e.t.Fatalf("restored %s differs: %v", name, err)
		}
	}
}

func setup(t *testing.T, e *env) {
	e.mustRestic("init")
	e.write("a.bin", 40<<20)
	e.write("b.bin", 3<<20)
	e.mustRestic("backup", e.src)
	out := e.mustPP(0, "sync")
	if !strings.Contains(out, "new files covered") {
		t.Fatal(out)
	}
}

func TestRepairAcrossFileKinds(t *testing.T) {
	e := newEnv(t)
	setup(t, e)

	pack := e.files("data")[0]
	idx := e.files("index")[0]
	snap := e.files("snapshots")[0]
	// A zeroed run of 300 KiB in a pack, a flipped byte early in it, a bad
	// byte in the index, and a truncated snapshot.
	damage(t, pack, 5<<20, make([]byte, 300<<10))
	damage(t, pack, 100, []byte{0x5a})
	os.Chmod(pack, 0o400) // as restic left it
	damage(t, idx, 10, []byte("!!"))
	os.Chmod(snap, 0o600)
	os.Truncate(snap, size(snap)-7)

	if out, err := e.restic("check", "--read-data"); err == nil {
		t.Fatalf("restic did not notice:\n%s", out)
	}
	out := e.mustPP(3, "check", "--read", "all")
	for _, f := range []string{pack, idx, snap} {
		if !strings.Contains(out, filepath.Base(f)) {
			t.Fatalf("%s not reported:\n%s", f, out)
		}
	}
	e.mustPP(0, "repair", "--dry-run")
	if out, _ := e.restic("check"); !strings.Contains(out, "error") && !strings.Contains(out, "Fatal") {
		t.Fatalf("dry run changed something:\n%s", out)
	}
	out = e.mustPP(0, "repair")
	if strings.Count(out, "repaired") != 3 {
		t.Fatal(out)
	}
	e.mustRestic("check", "--read-data")
	e.restoreMatches()
	e.mustPP(0, "check", "--read", "all")
	// The damaged copies were kept.
	q := 0
	filepath.Walk(filepath.Join(e.parity, "quarantine"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			q++
		}
		return nil
	})
	if q != 3 {
		t.Fatalf("%d files in quarantine", q)
	}
	// The file's mode is kept (restic makes its files read-only).
	if st, _ := os.Stat(pack); st.Mode().Perm() != 0o400 {
		t.Fatalf("repaired pack mode %v", st.Mode())
	}
}

func TestBeyondReach(t *testing.T) {
	e := newEnv(t)
	setup(t, e)
	pack := e.files("data")[0]
	before, _ := os.ReadFile(pack)
	// 2 MiB zeroed: more than 10% of a 16 MiB pack in one run.
	damage(t, pack, 1<<20, make([]byte, 2<<20))
	damaged, _ := os.ReadFile(pack)
	out := e.mustPP(4, "check", "--read", "all")
	if !strings.Contains(out, "UNREPAIRABLE") {
		t.Fatal(out)
	}
	out = e.mustPP(4, "repair")
	if !strings.Contains(out, "failed") {
		t.Fatal(out)
	}
	after, _ := os.ReadFile(pack)
	if !bytes.Equal(after, damaged) || bytes.Equal(after, before) {
		t.Fatal("an unrepairable file was changed")
	}
}

func TestPruneAndGuards(t *testing.T) {
	e := newEnv(t)
	setup(t, e)
	e.write("a.bin", 20<<20) // replaced: its old packs become unused
	e.mustRestic("backup", e.src)
	e.mustPP(0, "sync")
	e.mustRestic("forget", "--keep-last", "1", "--prune")
	out := e.mustPP(0, "sync")
	if strings.Contains(out, " 0 retired") {
		t.Fatalf("prune retired nothing:\n%s", out)
	}
	e.mustPP(0, "check", "--read", "all")
	st := e.mustPP(0, "status")
	if strings.Contains(st, "removed     0 ") {
		t.Fatal(st)
	}
	// Retired parity is purged after --keep-removed.
	out = e.mustPP(0, "sync", "--keep-removed", "0d")
	if strings.Contains(out, " 0 purged") {
		t.Fatal(out)
	}

	// An empty folder (an unmounted disk) is not a repository.
	empty := t.TempDir()
	var buf bytes.Buffer
	if code := cli.Main([]string{"sync", empty, e.parity}, &buf, &buf); code != 1 || !strings.Contains(buf.String(), "not a restic repository") {
		t.Fatalf("%d %s", code, buf.String())
	}

	// Another repository: its config is not the one the parity has.
	other := newEnv(t)
	other.mustRestic("init")
	other.write("a.bin", 1<<20)
	other.mustRestic("backup", other.src)
	other.parity = e.parity
	out = other.mustPP(3, "sync")
	if !strings.Contains(out, "CONFIG CHANGED") {
		t.Fatal(out)
	}
	e.mustPP(0, "sync") // the right one still matches

	// Many files gone at once: refused without --allow-mass-removal.
	for i := 0; i < 30; i++ {
		e.write(fmt.Sprintf("s%d.bin", i), 4<<10)
		e.mustRestic("backup", e.src, "--tag", fmt.Sprint(i))
	}
	e.mustPP(0, "sync")
	// Snapshots and index files gone: more than a quarter of the files.
	for _, d := range []string{"snapshots", "index"} {
		for _, s := range e.files(d)[:28] {
			os.Remove(s)
		}
	}
	out, code := e.pp("sync")
	if code != 1 || !strings.Contains(out, "--allow-mass-removal") {
		t.Fatalf("%d %s", code, out)
	}
	e.mustPP(0, "sync", "--allow-mass-removal")
}

func TestConfigRepair(t *testing.T) {
	e := newEnv(t)
	setup(t, e)
	cfg := filepath.Join(e.repo, "config")
	damage(t, cfg, 20, []byte("??"))
	if _, err := e.restic("snapshots"); err == nil {
		t.Fatal("restic read a damaged config")
	}
	out := e.mustPP(3, "check", "--read", "all")
	if !strings.Contains(out, "restic migrate") {
		t.Fatal(out)
	}
	// Not repaired unless named.
	if out := e.mustPP(0, "repair"); !strings.Contains(out, "nothing known") {
		t.Fatal(out)
	}
	e.mustPP(0, "repair", "config")
	e.mustRestic("check")
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestRestServer(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(t.TempDir(), "rest")
	addr := freePort(t)
	srv := exec.Command(bin("REST_SERVER_BIN", "rest-server"), "--path", root, "--listen", addr, "--no-auth")
	var log bytes.Buffer
	srv.Stdout, srv.Stderr = &log, &log
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Process.Kill(); srv.Wait() })
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.url = "rest:http://" + addr + "/r1"
	e.repo = filepath.Join(root, "r1")
	setup(t, e)
	pack := e.files("data")[0]
	damage(t, pack, -4096, make([]byte, 4096))
	if out, err := e.restic("check", "--read-data"); err == nil {
		t.Fatalf("restic did not notice:\n%s", out)
	}
	e.mustPP(3, "check", "--read", "all")
	e.mustPP(0, "repair")
	e.mustRestic("check", "--read-data")
	e.restoreMatches()
}

func TestSyncDuringBackup(t *testing.T) {
	e := newEnv(t)
	setup(t, e)
	e.write("c.bin", 120<<20)
	var bout string
	var berr error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		bout, berr = e.restic("backup", e.src)
	}()
	runs := 0
	for done := false; !done; {
		out, code := e.pp("sync", "-q")
		if code != 0 {
			t.Fatalf("sync during backup: exit %d\n%s", code, out)
		}
		runs++
		select {
		case <-finished:
			done = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if berr != nil {
		t.Fatalf("backup: %v\n%s", berr, bout)
	}
	e.mustPP(0, "sync")
	e.mustPP(0, "check", "--read", "all")
	t.Logf("%d syncs ran during the backup", runs)
}
