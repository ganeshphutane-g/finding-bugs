package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushdir/internal/rclone"
)

// Regression tests for problems found in review.

// waitExit waits for Run to return and gives its error.
func (h *harness) waitExit() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.cancel()
		h.cancel = nil
		return err
	case <-time.After(20 * time.Second):
		h.t.Fatalf("pushdir did not stop\nlog:\n%s", h.log)
		return nil
	}
}

func TestFiltersApplyToBatches(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.txt")
	must(t, os.WriteFile(rules, []byte("# private things\n- secret/**\n"), 0o644))
	cases := []struct {
		name  string
		flags []string
		sent  []string
		kept  []string
	}{
		{"exclude", []string{"--exclude", "*.tmp"}, []string{"a.txt", "dir/b.jpg"}, []string{"c.tmp", "dir/d.tmp"}},
		{"include", []string{"--include", "*.jpg"}, []string{"dir/b.jpg"}, []string{"a.txt", "c.tmp"}},
		{"filter-from", []string{"--filter-from", rules}, []string{"a.txt", "dir/b.jpg"}, []string{"secret/x.txt"}},
		{"size", []string{"--max-size", "5B"}, []string{"a.txt"}, []string{"big.txt"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.runner.Extra = append(h.runner.Extra, c.flags...)
			h.start()
			for _, f := range append(append([]string{}, c.sent...), c.kept...) {
				content := "x"
				if f == "big.txt" {
					content = "much more than five bytes"
				}
				h.write(f, content)
			}
			h.waitFor("the files to arrive", func() bool {
				for _, f := range c.sent {
					if !h.remoteHas(f, "x") {
						return false
					}
				}
				return true
			})
			time.Sleep(time.Second)
			for _, f := range c.kept {
				if !h.remoteMissing(f) {
					t.Errorf("%s should have been left out by %v", f, c.flags)
				}
			}
			if st := h.p.Status(); st.FailedBatches != 0 {
				t.Fatalf("%d failed batches: %s", st.FailedBatches, st.LastError)
			}
		})
	}
}

func TestReplacedRootStopsWithoutDeleting(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true })
	h.write("a.txt", "a")
	h.write("sub/b.txt", "b")
	h.start()
	must(t, os.Rename(h.local, h.local+".old"))
	must(t, os.MkdirAll(h.local, 0o755))
	h.write("placeholder", "p")
	err := h.waitExit()
	if err == nil || !strings.Contains(err.Error(), "removed or moved") {
		t.Fatalf("Run should stop with an error, got %v", err)
	}
	if !h.remoteHas("a.txt", "a") || !h.remoteHas("sub/b.txt", "b") {
		t.Fatalf("remote files were deleted: %s", keys(tree(t, h.remote)))
	}
}

func TestLookAlikeFileIsNotTakenForARename(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.DeleteQuiet = 200 * time.Millisecond })
	a, b := strings.Repeat("A", 1<<20), strings.Repeat("B", 1<<20)
	h.write("a.bin", a)
	h.start()
	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	must(t, os.Chtimes(filepath.Join(h.local, "a.bin"), mtime, mtime))
	must(t, os.Chtimes(filepath.Join(h.remote, "a.bin"), mtime, mtime))
	// Same size and time as a.bin, different content.
	tmp := filepath.Join(t.TempDir(), "b.bin")
	must(t, os.WriteFile(tmp, []byte(b), 0o644))
	must(t, os.Chtimes(tmp, mtime, mtime))
	must(t, os.Rename(tmp, filepath.Join(h.local, "b.bin")))
	must(t, os.Remove(filepath.Join(h.local, "a.bin")))
	h.waitFor("b.bin with its own content", func() bool { return h.remoteHas("b.bin", b) })
	if strings.Contains(h.log.String(), "renamed") {
		t.Fatalf("a different file was treated as a rename:\n%s", h.log)
	}
}

func TestRenameOverAFileKeepsItInTheBackupDir(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "backup")
	h := newHarness(t, func(o *Options) { o.Delete = true; o.DeleteQuiet = 200 * time.Millisecond })
	h.runner.BackupDir = backup
	a, old := strings.Repeat("A", 1<<20), strings.Repeat("O", 1<<20)
	h.write("a.bin", a)
	h.write("b.bin", old)
	h.start()
	must(t, os.Rename(filepath.Join(h.local, "a.bin"), filepath.Join(h.local, "b.bin")))
	h.waitFor("b.bin replaced", func() bool { return h.remoteHas("b.bin", a) && h.remoteMissing("a.bin") })
	data, err := os.ReadFile(filepath.Join(backup, "b.bin"))
	if err != nil || string(data) != old {
		t.Fatalf("the overwritten b.bin is not in the backup dir (%v)\nlog:\n%s", err, h.log)
	}
}

func TestNamesWithLineBreaksArriveByFullCheck(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	h.write("two\nlines.txt", "n")
	h.waitFor("the file and a full check", func() bool { return h.remoteHas("two\nlines.txt", "n") && h.p.Status().Resyncs >= 2 })
	if st := h.p.Status(); st.Resyncs < 2 {
		t.Fatalf("expected a full check, status %+v", st)
	}
}

func TestAtomicSavesDoNotUseTheDeleteBudget(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Delete = true; o.MaxDelete = 3; o.DeleteQuiet = 200 * time.Millisecond })
	h.write("doc.txt", "v0")
	h.start()
	for i := 1; i <= 6; i++ {
		v := "v" + string(rune('0'+i))
		tmp := filepath.Join(h.local, ".doc.txt.swp")
		must(t, os.WriteFile(tmp, []byte(v), 0o644))
		must(t, os.Rename(tmp, filepath.Join(h.local, "doc.txt")))
		h.waitFor("save "+v, func() bool { return h.remoteHas("doc.txt", v) })
		time.Sleep(400 * time.Millisecond)
	}
	if st := h.p.Status(); st.DeletionsPaused != "" || st.Deleted != 0 {
		t.Fatalf("atomic saves counted as deletions: %+v", st)
	}
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestNoBusyLoopWhileAFileSettles(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Settle = 3 * time.Second })
	h.start()
	h.write("future.txt", "f")
	future := time.Now().Add(24 * time.Hour)
	must(t, os.Chtimes(filepath.Join(h.local, "future.txt"), future, future))
	before := cpuTime()
	time.Sleep(2 * time.Second)
	if used := cpuTime() - before; used > 500*time.Millisecond {
		t.Fatalf("used %s of CPU in 2s while waiting", used)
	}
	// A clock far ahead does not hold the file back.
	h.waitFor("the file", func() bool { return h.remoteHas("future.txt", "f") })
}

func TestUnknownRemoteStopsAtStart(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Remote = "nosuchremote:dir" })
	h.p = New(h.options)
	err := h.p.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `no remote called "nosuchremote"`) {
		t.Fatalf("got %v", err)
	}
}

func TestNoTransferExitCodeIsFine(t *testing.T) {
	h := newHarness(t, nil)
	h.runner.Extra = append(h.runner.Extra, "--error-on-no-transfer")
	h.write("a.txt", "a")
	h.start()
	h.write("b.txt", "b")
	h.waitFor("b.txt", func() bool { return h.remoteHas("b.txt", "b") })
	if st := h.p.Status(); st.FailedBatches != 0 {
		t.Fatalf("failed batches: %s", st.LastError)
	}
}

func TestSecretsAreNotLogged(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Remote = ":sftp,host=127.0.0.1,port=1,user=u,pass=SECRETPW:dir" })
	h.runner.Extra = append(h.runner.Extra, "--contimeout", "1s", "--retries", "1", "--low-level-retries", "1")
	h.p = New(h.options)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.p.Run(ctx) }()
	h.t.Cleanup(h.stop)
	h.waitFor("a failure", func() bool { return h.p.Status().LastError != "" })
	st := h.p.Status()
	if strings.Contains(h.log.String(), "SECRETPW") || strings.Contains(st.Remote, "SECRETPW") || strings.Contains(st.LastError, "SECRETPW") {
		t.Fatalf("password leaked:\nstatus %+v\nlog:\n%s", st, h.log)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		":sftp,host=h,pass=abc:dir":               ":sftp,host=h,pass=***:dir",
		`s3,secret_access_key="a,b",region=x:bkt`: `s3,secret_access_key=***,region=x:bkt`,
		"b2:bucket/dir":                           "b2:bucket/dir",
	} {
		if got := rclone.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
