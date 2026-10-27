package steps

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type rec struct {
	mu          sync.Mutex
	info, debug []string
}

func (r *rec) Info(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.info = append(r.info, fmt.Sprintf(f, a...))
}
func (r *rec) Debug(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.debug = append(r.debug, fmt.Sprintf(f, a...))
}
func (r *rec) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.info {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func TestOwnerAndMode(t *testing.T) {
	if o, err := ParseOwner("1000:1001"); err != nil || o != (Owner{1000, 1001}) {
		t.Errorf("%v %v", o, err)
	}
	if o, err := ParseOwner("1000"); err != nil || o != (Owner{1000, -1}) {
		t.Errorf("%v %v", o, err)
	}
	if o, err := ParseOwner("root"); err != nil || o != (Owner{0, 0}) {
		t.Errorf("root: %v %v", o, err)
	}
	for _, bad := range []string{"", ":1", "nosuchuser9", "1:nosuchgroup9"} {
		if _, err := ParseOwner(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// 32 bits, and not -1 (a value the kernel would cut down to another).
	for _, bad := range []string{"4294968296:1000", "1000:4294967296", "4294967295", "-1"} {
		if _, err := ParseOwner(bad); err == nil {
			t.Errorf("owner %q accepted", bad)
		}
	}
	if o, err := ParseOwner("4294967294:0"); err != nil || o.UID != 4294967294 {
		t.Errorf("%v %v", o, err)
	}
	if m, err := ParseMode("0750"); err != nil || m != 0o750 {
		t.Errorf("%v %v", m, err)
	}
	for _, bad := range []string{"", "9", "4755", "rwx"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("mode %q accepted", bad)
		}
	}
	// An unquoted 0750 in YAML arrives as 488.
	if _, err := ParseMode("488"); err == nil || !strings.Contains(err.Error(), `quote it, mode: "0750"`) {
		t.Errorf("%v", err)
	}
}

func TestMkdir(t *testing.T) {
	dir := t.TempDir()
	r := &rec{}
	mode := fs.FileMode(0o750)
	if err := Mkdir(r, dir, []string{"data", "deep/er/sub"}, nil, &mode); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]fs.FileMode{"data": 0o750, "deep/er/sub": 0o750, "deep": 0o755, "deep/er": 0o755} {
		st, err := os.Stat(filepath.Join(dir, p))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %o", p, st.Mode(), err, want)
		}
	}
	// Again: nothing to do, nothing said.
	r2 := &rec{}
	if err := Mkdir(r2, dir, []string{"data"}, nil, &mode); err != nil || len(r2.info) != 0 {
		t.Errorf("%v %v", err, r2.info)
	}
	// A changed mode is set on an existing directory.
	os.Chmod(filepath.Join(dir, "data"), 0o777)
	if err := Mkdir(r2, dir, []string{"data"}, nil, &mode); err != nil || !r2.has("data mode 0750") {
		t.Errorf("%v %v", err, r2.info)
	}
	// A file in the way, and a symbolic link.
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o644)
	if err := Mkdir(r, dir, []string{"file"}, nil, nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("%v", err)
	}
	// A link in a directory only we can change is followed.
	os.Mkdir(filepath.Join(dir, "real"), 0o755)
	os.Symlink("real", filepath.Join(dir, "link"))
	if err := Mkdir(r, dir, []string{"link/sub"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real", "sub")); err != nil {
		t.Fatal(err)
	}
	// One in a directory others can change is refused: as the last name,
	// in the middle, or with a trailing slash.
	shared := filepath.Join(dir, "shared")
	os.Mkdir(shared, 0o755)
	os.Chmod(shared, 0o777)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(shared, "link"))
	for _, p := range []string{"shared/link", "shared/link/sub", shared + "/link/"} {
		if err := Mkdir(r, dir, []string{p}, &Owner{-1, -1}, nil); err == nil || !strings.Contains(err.Error(), "symbolic link in a directory that other users can change") {
			t.Errorf("%s: %v", p, err)
		}
	}
	// Never the owner or mode of / (an unset ${DATA}/) or a system
	// directory, even through a link; unless nothing would change.
	if err := Mkdir(r, dir, []string{"/"}, &Owner{0, 0}, nil); err != nil {
		t.Errorf("no change to /: %v", err)
	}
	for _, p := range []string{"/", "//", "/etc/", "/usr/../var", "/proc/self/root", "/proc/self/root/var/tmp"} {
		if err := Mkdir(r, dir, []string{p}, &Owner{1000, 1000}, nil); err == nil || !strings.Contains(err.Error(), "does not change") {
			t.Errorf("%s: %v", p, err)
		}
	}
	// Created directories get exactly their modes, whatever the umask.
	old := syscall.Umask(0o077)
	err := Mkdir(r, dir, []string{"um/a/b"}, nil, &mode)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]fs.FileMode{"um": 0o755, "um/a": 0o755, "um/a/b": 0o750} {
		if st, err := os.Stat(filepath.Join(dir, p)); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %o", p, st.Mode(), err, want)
		}
	}
	if os.Geteuid() == 0 {
		// As root, a link in a directory a container user owns (one
		// composehost gave them) is refused.
		user := filepath.Join(dir, "user")
		os.Mkdir(user, 0o755)
		os.Chown(user, 1000, 1000)
		os.Symlink("/etc", filepath.Join(user, "pg"))
		if err := Mkdir(r, dir, []string{"user/pg/db"}, &Owner{1000, 1000}, nil); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%v", err)
		}
		if _, err := os.Stat("/etc/db"); err == nil {
			t.Fatal("followed the link")
		}
	}
	if os.Geteuid() == 0 {
		if err := Mkdir(r, dir, []string{"/" + filepath.Join(dir[1:], "owned")}, &Owner{1000, 1001}, nil); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(filepath.Join(dir, "owned"))
		if uid, gid := ownerOf(st); uid != 1000 || gid != 1001 {
			t.Errorf("owner %d:%d", uid, gid)
		}
	}
}

func TestIsMountPoint(t *testing.T) {
	dir := t.TempDir()
	mi := filepath.Join(dir, "mountinfo")
	os.Mkdir(filepath.Join(dir, "my nas"), 0o755)
	os.Mkdir(filepath.Join(dir, "plain"), 0o755)
	os.Symlink(filepath.Join(dir, "my nas"), filepath.Join(dir, "link"))
	esc := strings.ReplaceAll(filepath.Join(dir, "my nas"), " ", `\040`)
	os.Mkdir(filepath.Join(dir, "auto"), 0o755)
	os.Mkdir(filepath.Join(dir, "auto2"), 0o755)
	auto, auto2 := filepath.Join(dir, "auto"), filepath.Join(dir, "auto2")
	os.WriteFile(mi, []byte("22 1 0:21 / / rw - ext4 /dev/vda rw\n99 22 0:50 / "+esc+" rw - nfs nas:/x rw\n"+
		// An automount point not mounted yet, and one mounted over.
		"100 22 0:51 / "+auto+" rw,relatime shared:5 - autofs systemd-1 rw\n"+
		"101 22 0:52 / "+auto2+" rw shared:6 - autofs systemd-1 rw\n"+
		"102 101 0:53 / "+auto2+" rw shared:7 - nfs4 nas:/y rw\n"), 0o644)
	old := MountInfo
	MountInfo = mi
	defer func() { MountInfo = old }()
	for p, want := range map[string]bool{"my nas": true, "plain": false, "link": true, "auto": false, "auto2": true} {
		if ok, err := IsMountPoint(filepath.Join(dir, p)); err != nil || ok != want {
			t.Errorf("%s: %v %v", p, ok, err)
		}
	}
	if _, err := IsMountPoint(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing path")
	}
	// The real one: / is a mount point.
	MountInfo = old
	if ok, err := IsMountPoint("/"); err != nil || !ok {
		t.Errorf("/: %v %v", ok, err)
	}
}

func TestWait(t *testing.T) {
	dir := t.TempDir()
	r := &rec{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	go func() { time.Sleep(150 * time.Millisecond); os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644) }()
	w := Waits{Paths: []string{"ready"}, TCP: []string{ln.Addr().String()}, Timeout: 5 * time.Second, Poll: 20 * time.Millisecond}
	if err := Wait(context.Background(), r, dir, w); err != nil {
		t.Fatal(err)
	}
	if !r.has("waiting for ready to exist") || r.has("accept connections") {
		t.Errorf("%v", r.info)
	}
	// A timeout says what was still missing.
	w = Waits{Paths: []string{"never"}, TCP: []string{"127.0.0.1:1"}, Timeout: 200 * time.Millisecond, Poll: 20 * time.Millisecond}
	err = Wait(context.Background(), &rec{}, dir, w)
	if err == nil || !strings.Contains(err.Error(), "gave up after 200ms waiting for never to exist, 127.0.0.1:1 to accept connections") {
		t.Errorf("%v", err)
	}
	if err := Wait(context.Background(), r, dir, Waits{}); err != nil {
		t.Error(err)
	}
	// Stopped (Ctrl-C) is not a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	w.Timeout = time.Minute
	if err := Wait(ctx, &rec{}, dir, w); err == nil || !strings.Contains(err.Error(), "stopped while waiting for never to exist") {
		t.Errorf("%v", err)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	r := &rec{}
	err := Run(context.Background(), r, dir, "run", "pwd; echo $COMPOSEHOST_SERVICE; echo oops >&2", time.Second, []string{"COMPOSEHOST_SERVICE=prep"})
	if err != nil || strings.Join(r.debug, "|") != "run: "+dir+"|run: prep|run (stderr): oops" {
		t.Fatalf("%q %v", r.debug, err)
	}
	// Output returns stdout, and does not log it (it may be secrets).
	r.debug = nil
	out, err := Output(context.Background(), r, dir, "env", "echo TOKEN=s3cret; echo note >&2", time.Second, nil)
	if err != nil || string(out) != "TOKEN=s3cret\n" || strings.Join(r.debug, "|") != "env (stderr): note" {
		t.Fatalf("%q %q %v", out, r.debug, err)
	}
	err = Run(context.Background(), r, dir, "run", "echo one >&2; echo two >&2; exit 3", time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 3: one / two") {
		t.Errorf("%v", err)
	}
	start := time.Now()
	err = Run(context.Background(), r, dir, "run", "sleep 10", 200*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "took longer than 200ms") || time.Since(start) > 5*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	// A background child holding the output open does not hang it, nor
	// fail it.
	start = time.Now()
	if err := Run(context.Background(), r, dir, "run", "sleep 30 & echo started", 5*time.Second, nil); err != nil || time.Since(start) > 4*time.Second {
		t.Errorf("%v after %s", err, time.Since(start))
	}
	// On a timeout, what it started is killed too.
	err = Run(context.Background(), r, dir, "run", "(sleep 1; touch late) & sleep 10", 200*time.Millisecond, nil)
	time.Sleep(1500 * time.Millisecond)
	if _, serr := os.Stat(filepath.Join(dir, "late")); err == nil || serr == nil {
		t.Errorf("%v; the child ran on", err)
	}
	for cmd, want := range map[string]string{
		"./prepare.sh":                  "./prepare.sh",
		"echo hi; exit 1":               "echo …",
		"API_TOKEN=s3cret ./deploy --x": "./deploy …",
		"A=1 B=2":                       "…",
		"curl&& x":                      "curl …",
		"'quoted secret' x":             "…",
		"FOO=bar":                       "…",
		"PASS='correct horse' restic x": "…",
		`PASS=abc\ def ./x`:             "…",
		"   ":                           "…",
	} {
		if got := short(cmd); got != want {
			t.Errorf("short(%q) = %q, want %q", cmd, got, want)
		}
	}
	// The error shows the last lines, even past the cap.
	err = Run(context.Background(), r, dir, "run", "yes progress | head -c 5000000 >&2; echo REAL ERROR >&2; exit 3", 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "REAL ERROR") {
		t.Errorf("%v", err)
	}
	// Output past the cap is dropped; for env that is an error.
	if _, err := Output(context.Background(), r, dir, "env", "head -c 5000000 /dev/zero", 5*time.Second, nil); err == nil || !strings.Contains(err.Error(), "more than 4 MiB") {
		t.Errorf("%v", err)
	}
	// Only the command's first word is shown: the rest may hold secrets.
	r = &rec{}
	err = Run(context.Background(), r, dir, "run", "curl -H 'Authorization: Bearer s3cret' x; exit 4", time.Second, nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") || !r.has("run: curl …") || strings.Contains(strings.Join(r.info, " "), "s3cret") {
		t.Errorf("%v %v", err, r.info)
	}
	if err := Run(context.Background(), r, dir, "run", "  ", time.Second, nil); err == nil {
		t.Error("an empty command")
	}
}

func TestParseEnv(t *testing.T) {
	env, err := ParseEnv([]byte("# comment\nA=1\n\nexport B=\"two words\"\nC='x=y'\nD=\n"))
	if err != nil || len(env) != 4 || env[1] != [2]string{"B", "two words"} || env[2] != [2]string{"C", "x=y"} || env[3] != [2]string{"D", ""} {
		t.Fatalf("%v %v", env, err)
	}
	// A bad line is not shown: it may be a secret.
	if _, err := ParseEnv([]byte("A=1\nhunter2\n")); err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("%v", err)
	}
	long := "CERT=" + strings.Repeat("x", 200000)
	if env, err := ParseEnv([]byte(long)); err != nil || len(env[0][1]) != 200000 {
		t.Errorf("%v", err)
	}
	for _, bad := range []string{"no equals", "1A=x", "A B=c"} {
		if _, err := ParseEnv([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
