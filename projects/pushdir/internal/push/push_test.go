package push

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushdir/internal/rclone"
)

// rcloneBin finds rclone for the tests (RCLONE_BIN, or rclone on PATH).
func rcloneBin(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("RCLONE_BIN"); b != "" {
		return b
	}
	b, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not found (set RCLONE_BIN)")
	}
	return b
}

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// harness runs a Pusher on temp folders.
type harness struct {
	t       *testing.T
	local   string
	remote  string // a local folder used as the remote
	p       *Pusher
	cancel  context.CancelFunc
	done    chan error
	log     *logBuf
	runner  *rclone.Runner
	options Options
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	base := t.TempDir()
	h := &harness{t: t, local: filepath.Join(base, "local"), remote: filepath.Join(base, "remote"), log: &logBuf{}}
	must(t, os.MkdirAll(h.local, 0o755))
	must(t, os.MkdirAll(h.remote, 0o755))
	h.runner = &rclone.Runner{Bin: rcloneBin(t), Extra: []string{"--config", filepath.Join(base, "rclone.conf")}}
	h.options = Options{
		Local: h.local, Remote: h.remote, Runner: h.runner, Log: h.log.add,
		Quiet: 200 * time.Millisecond, MaxDelay: 2 * time.Second, Settle: 300 * time.Millisecond,
		DeleteQuiet: time.Second,
	}
	if mutate != nil {
		mutate(&h.options)
	}
	return h
}

func (h *harness) start() {
	h.t.Helper()
	h.p = New(h.options)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.p.Run(ctx) }()
	h.t.Cleanup(h.stop)
	h.waitFor("the first full check", func() bool { return h.p.Status().Resyncs >= 1 })
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.local, filepath.FromSlash(rel))
	must(h.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(h.t, os.WriteFile(p, []byte(content), 0o644))
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\nstatus: %+v\nlog:\n%s", what, h.p.Status(), h.log)
}

// tree lists files under dir with their contents.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		data, _ := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return out
}

func keys(m map[string]string) string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return strings.Join(k, ", ")
}

func (h *harness) remoteHas(rel, content string) bool {
	data, err := os.ReadFile(filepath.Join(h.remote, filepath.FromSlash(rel)))
	return err == nil && string(data) == content
}

func (h *harness) remoteMissing(rel string) bool {
	_, err := os.Stat(filepath.Join(h.remote, filepath.FromSlash(rel)))
	return os.IsNotExist(err)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInitialCopyAndNewFiles(t *testing.T) {
	h := newHarness(t, nil)
	h.write("before.txt", "made before start")
	h.write("old/dir/a.txt", "a")
	h.start()
	if !h.remoteHas("before.txt", "made before start") || !h.remoteHas("old/dir/a.txt", "a") {
		t.Fatalf("first copy incomplete: %s", keys(tree(t, h.remote)))
	}

	h.write("new.txt", "one")
	h.write("brand/new/deep/folder/b.txt", "b") // folders created after start
	for i := 0; i < 50; i++ {
		h.write(fmt.Sprintf("burst/f%02d.txt", i), fmt.Sprint(i))
	}
	h.waitFor("every new file on the remote", func() bool {
		return len(tree(t, h.remote)) == len(tree(t, h.local))
	})
	if l, r := tree(t, h.local), tree(t, h.remote); fmt.Sprint(l) != fmt.Sprint(r) {
		t.Fatalf("remote differs:\nlocal  %s\nremote %s", keys(l), keys(r))
	}
	// Changes come in batches, not one rclone run per file.
	if b := h.p.Status().Batches; b > 10 {
		t.Errorf("52 files took %d batches", b)
	}
	h.write("new.txt", "two")
	h.waitFor("the overwrite", func() bool { return h.remoteHas("new.txt", "two") })
}

func TestHalfWrittenFilesWait(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Settle = 800 * time.Millisecond })
	h.start()
	p := filepath.Join(h.local, "growing.log")
	f, err := os.Create(p)
	must(t, err)
	sawPartial := false
	for i := 0; i < 8; i++ {
		fmt.Fprintf(f, "line %d\n", i)
		f.Sync()
		time.Sleep(250 * time.Millisecond)
		if data, err := os.ReadFile(filepath.Join(h.remote, "growing.log")); err == nil && len(data) > 0 && !strings.Contains(string(data), "line 7") {
			sawPartial = true
		}
	}
	f.Close()
	full, _ := os.ReadFile(p)
	h.waitFor("the finished file", func() bool { return h.remoteHas("growing.log", string(full)) })
	if sawPartial {
		t.Fatal("a half-written file was sent while it was still being written")
	}
}

func TestDeletionsAreOffByDefault(t *testing.T) {
	h := newHarness(t, nil)
	h.write("keep.txt", "k")
	h.start()
	must(t, os.Remove(filepath.Join(h.local, "keep.txt")))
	h.write("other.txt", "o") // a later batch must not delete keep.txt either
	h.waitFor("the later file", func() bool { return h.remoteHas("other.txt", "o") })
	// A restart runs a full check; that must not delete it either.
	h.stop()
	h.start()
	if !h.remoteHas("keep.txt", "k") {
		t.Fatal("a local deletion reached the remote without --delete")
	}
}

func TestDeleteMirrorsFilesAndFolders(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true })
	h.write("a.txt", "a")
	h.write("dir/b.txt", "b")
	h.write("dir/sub/c.txt", "c")
	h.write("stay.txt", "s")
	h.start()
	must(t, os.Remove(filepath.Join(h.local, "a.txt")))
	h.waitFor("a.txt gone remotely", func() bool { return h.remoteMissing("a.txt") })
	must(t, os.RemoveAll(filepath.Join(h.local, "dir")))
	h.waitFor("dir gone remotely", func() bool { return h.remoteMissing("dir/b.txt") && h.remoteMissing("dir/sub/c.txt") })
	if !h.remoteHas("stay.txt", "s") {
		t.Fatal("an unrelated file was deleted")
	}
}

func TestBackupDirKeepsDeletedAndOverwritten(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true })
	backup := filepath.Join(filepath.Dir(h.remote), "backup")
	h.runner.BackupDir = backup
	h.write("doc.txt", "v1")
	h.write("gone.txt", "bye")
	h.start()
	h.write("doc.txt", "v2")
	must(t, os.Remove(filepath.Join(h.local, "gone.txt")))
	h.waitFor("the new version and the deletion", func() bool {
		return h.remoteHas("doc.txt", "v2") && h.remoteMissing("gone.txt")
	})
	b := tree(t, backup)
	if b["doc.txt"] != "v1" || b["gone.txt"] != "bye" {
		t.Fatalf("backup dir has %v", b)
	}
}

func TestMassDeletionIsRefused(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.MaxDelete = 5 })
	for i := 0; i < 12; i++ {
		h.write(fmt.Sprintf("f%02d.txt", i), "x")
	}
	h.write("keep/marker.txt", "m") // so the folder is not empty afterwards
	h.start()
	for i := 0; i < 12; i++ {
		must(t, os.Remove(filepath.Join(h.local, fmt.Sprintf("f%02d.txt", i))))
	}
	h.waitFor("deletions to be paused", func() bool { return h.p.Status().DeletionsPaused != "" })
	if n := len(tree(t, h.remote)); n != 13 {
		t.Fatalf("the remote has %d files; nothing should have been deleted", n)
	}
	// Later deletions stay paused, uploads continue.
	h.write("after.txt", "a")
	h.waitFor("uploads to continue", func() bool { return h.remoteHas("after.txt", "a") })
	must(t, os.Remove(filepath.Join(h.local, "keep/marker.txt")))
	time.Sleep(time.Second)
	if !h.remoteHas("keep/marker.txt", "m") {
		t.Fatal("a deletion went through while deletions were paused")
	}
}

// TestSlowTrickleOfDeletionsIsCapped deletes files one at a time, each in
// its own batch: the hourly budget still stops them.
func TestSlowTrickleOfDeletionsIsCapped(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.MaxDelete = 3; o.DeleteQuiet = 300 * time.Millisecond })
	for i := 0; i < 8; i++ {
		h.write(fmt.Sprintf("f%d.txt", i), "x")
	}
	h.write("keep.txt", "k")
	h.start()
	for i := 0; i < 8; i++ {
		must(t, os.Remove(filepath.Join(h.local, fmt.Sprintf("f%d.txt", i))))
		time.Sleep(700 * time.Millisecond)
	}
	h.waitFor("deletions to be paused", func() bool { return h.p.Status().DeletionsPaused != "" })
	if got := h.p.Status().Deleted; got > 3 {
		t.Fatalf("%d files were deleted remotely; the budget is 3", got)
	}
}

func TestEmptiedFolderDeletesNothing(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.MaxDelete = 1000 })
	for i := 0; i < 5; i++ {
		h.write(fmt.Sprintf("f%d.txt", i), "x")
	}
	h.start()
	entries, _ := os.ReadDir(h.local)
	for _, e := range entries {
		must(t, os.RemoveAll(filepath.Join(h.local, e.Name())))
	}
	h.waitFor("deletions to be paused", func() bool { return h.p.Status().DeletionsPaused != "" })
	if n := len(tree(t, h.remote)); n != 5 {
		t.Fatalf("the remote has %d files; an emptied folder must delete nothing", n)
	}
	if !strings.Contains(h.p.Status().DeletionsPaused, "empty") {
		t.Errorf("reason: %q", h.p.Status().DeletionsPaused)
	}
}

func TestStartupSyncCountsDeletionsFirst(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.MaxDelete = 3 })
	h.write("local.txt", "l")
	for i := 0; i < 10; i++ { // files only on the remote
		must(t, os.WriteFile(filepath.Join(h.remote, fmt.Sprintf("extra%d", i)), []byte("e"), 0o644))
	}
	h.start()
	if h.p.Status().DeletionsPaused == "" {
		t.Fatal("a startup sync deleting 10 files should pause deletions with --max-delete 3")
	}
	if n := len(tree(t, h.remote)); n != 11 {
		t.Fatalf("the remote has %d files; none should have been deleted", n)
	}
	if !h.remoteHas("local.txt", "l") {
		t.Fatal("the local file should still be copied")
	}
}

// TestRemoteOutage stops the remote server, makes changes, then brings it
// back: everything arrives once it is reachable again.
func TestRemoteOutage(t *testing.T) {
	bin := rcloneBin(t)
	served := t.TempDir()
	port := freePort(t)
	serve := func() *exec.Cmd {
		cmd := exec.Command(bin, "serve", "webdav", served, "--addr", "127.0.0.1:"+port)
		must(t, cmd.Start())
		waitPort(t, port)
		return cmd
	}
	srv := serve()
	defer func() { srv.Process.Kill(); srv.Wait() }()

	h := newHarness(t, func(o *Options) { o.Remote = "dav:" })
	h.runner.Extra = append(h.runner.Extra, "--retries", "1", "--low-level-retries", "1", "--contimeout", "1s", "--timeout", "2s")
	os.Setenv("RCLONE_CONFIG_DAV_TYPE", "webdav")
	os.Setenv("RCLONE_CONFIG_DAV_URL", "http://127.0.0.1:"+port)
	defer os.Unsetenv("RCLONE_CONFIG_DAV_TYPE")
	defer os.Unsetenv("RCLONE_CONFIG_DAV_URL")
	h.write("first.txt", "1")
	h.start()
	if _, err := os.Stat(filepath.Join(served, "first.txt")); err != nil {
		t.Fatal("first copy did not reach the WebDAV remote")
	}

	srv.Process.Kill()
	srv.Wait()
	h.write("during-outage.txt", "2")
	h.waitFor("a failed batch", func() bool { return h.p.Status().FailedBatches > 0 })

	srv = serve()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(served, "during-outage.txt")); err == nil && string(data) == "2" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the change made during the outage never arrived\n%s", h.log)
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func waitPort(t *testing.T, port string) {
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not start")
}

// TestRenameOfLargeFileIsAMoveNotAnUpload renames a large file and checks it
// was moved on the remote (no second upload), keeping the old name without
// --delete.
func TestRenameOfLargeFileIsAMoveNotAnUpload(t *testing.T) {
	for _, del := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete=%v", del), func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.Delete = del; o.DeleteQuiet = 200 * time.Millisecond })
			big := strings.Repeat("0123456789abcdef", 1<<17) // 2 MiB
			h.write("video/raw.mp4", big)
			h.start()
			must(t, os.Rename(filepath.Join(h.local, "video/raw.mp4"), filepath.Join(h.local, "video/final.mp4")))
			h.waitFor("the renamed file", func() bool { return h.remoteHas("video/final.mp4", big) })
			h.waitFor("the rename to be handled", func() bool { return strings.Contains(h.log.String(), "renamed video/raw.mp4 to video/final.mp4") })
			if del {
				h.waitFor("the old name gone", func() bool { return h.remoteMissing("video/raw.mp4") })
			} else if !h.remoteHas("video/raw.mp4", big) {
				t.Fatal("without --delete the old name must stay")
			}
		})
	}
}
