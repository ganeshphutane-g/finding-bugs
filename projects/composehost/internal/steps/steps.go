// Package steps is what composehost does on the host: directories with an
// owner and mode, waits for mounts, paths and ports, host commands, and
// environment values for the dependent services. Every step is safe to
// repeat: Compose runs them on every "up".
package steps

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Report is where steps say what they do (protocol.Out).
type Report interface {
	Info(format string, args ...any)
	Debug(format string, args ...any)
}

// Owner is a uid and gid to give directories (-1: leave).
type Owner struct{ UID, GID int }

// ParseOwner reads "UID:GID", "USER:GROUP", "USER" (its primary group) or
// "UID" (the gid is left).
func ParseOwner(s string) (Owner, error) {
	u, g, hasGroup := strings.Cut(s, ":")
	o := Owner{-1, -1}
	if u == "" {
		return o, fmt.Errorf("owner %q: no user", s)
	}
	if n, err := strconv.Atoi(u); err == nil && n >= 0 {
		o.UID = n
	} else {
		pu, err := user.Lookup(u)
		if err != nil {
			return o, fmt.Errorf("owner %q: %v", s, err)
		}
		o.UID, _ = strconv.Atoi(pu.Uid)
		if !hasGroup {
			o.GID, _ = strconv.Atoi(pu.Gid)
		}
	}
	if hasGroup {
		if n, err := strconv.Atoi(g); err == nil && n >= 0 {
			o.GID = n
		} else {
			pg, err := user.LookupGroup(g)
			if err != nil {
				return o, fmt.Errorf("owner %q: %v", s, err)
			}
			o.GID, _ = strconv.Atoi(pg.Gid)
		}
	}
	return o, nil
}

// ParseMode reads an octal permission mode such as 0750 or 750 (no setuid,
// setgid or sticky bits).
func ParseMode(s string) (fs.FileMode, error) {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o777 {
		return 0, fmt.Errorf("mode %q: an octal permission mode such as 0750 (up to 0777)", s)
	}
	return fs.FileMode(n), nil
}

// Mkdir creates each directory (with parents) if missing, then gives it the
// owner and mode, when set and different. Relative paths are from dir.
func Mkdir(r Report, dir string, paths []string, owner *Owner, mode *fs.FileMode) error {
	for _, p := range paths {
		if p == "" {
			return errors.New("mkdir: an empty path")
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(dir, p)
		}
		st, err := os.Lstat(full)
		switch {
		case err == nil && st.Mode()&fs.ModeSymlink != 0:
			// Never chown or chmod through a link: it may point anywhere.
			return fmt.Errorf("mkdir %s: it is a symbolic link; give the real directory", p)
		case err == nil && !st.IsDir():
			return fmt.Errorf("mkdir %s: it exists and is not a directory", p)
		case errors.Is(err, fs.ErrNotExist):
			// Parents get the usual 0755; the owner and mode are the
			// directory's own.
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", p, err)
			}
			m := fs.FileMode(0o755)
			if mode != nil {
				m = *mode
			}
			if err := os.Mkdir(full, m); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("mkdir %s: %w", p, err)
			}
			r.Info("created %s", p)
			if st, err = os.Lstat(full); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("mkdir %s: %w", p, err)
		}
		if owner != nil {
			uid, gid := ownerOf(st)
			want := *owner
			if want.UID == -1 {
				want.UID = uid
			}
			if want.GID == -1 {
				want.GID = gid
			}
			if uid != want.UID || gid != want.GID {
				if err := os.Lchown(full, want.UID, want.GID); err != nil {
					return fmt.Errorf("mkdir %s: owner %d:%d: %w (composehost runs as the user running Compose)", p, want.UID, want.GID, err)
				}
				r.Info("%s owned by %d:%d", p, want.UID, want.GID)
			}
		}
		if mode != nil && st.Mode().Perm() != *mode {
			// MkdirAll's mode is cut by the umask; this sets it exactly.
			if err := os.Chmod(full, *mode); err != nil {
				return fmt.Errorf("mkdir %s: mode %04o: %w", p, uint32(*mode), err)
			}
			r.Info("%s mode %04o", p, uint32(*mode))
		}
	}
	return nil
}

// Waits are conditions to wait for before the dependent services start.
type Waits struct {
	Mounts, Paths, TCP []string
	Timeout            time.Duration
	// Poll is how often to look again (default 500 ms).
	Poll time.Duration
}

// Empty says whether there is nothing to wait for.
func (w Waits) Empty() bool { return len(w.Mounts)+len(w.Paths)+len(w.TCP) == 0 }

// MountInfo is /proc/self/mountinfo (a variable for tests).
var MountInfo = "/proc/self/mountinfo"

// Wait waits until every condition holds, or the timeout passes. Relative
// paths are from dir.
func Wait(ctx context.Context, r Report, dir string, w Waits) error {
	if w.Empty() {
		return nil
	}
	poll := w.Poll
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	told := map[string]bool{}
	for {
		pending, err := Pending(ctx, dir, w)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, p := range pending {
			if !told[p] {
				told[p] = true
				r.Info("waiting for %s", p)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up after %s waiting for %s", w.Timeout, strings.Join(pending, ", "))
		case <-time.After(poll):
		}
	}
}

// Pending looks once and says which conditions do not hold yet.
func Pending(ctx context.Context, dir string, w Waits) ([]string, error) {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	poll := w.Poll
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	var pending []string
	for _, m := range w.Mounts {
		ok, err := IsMountPoint(abs(m))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("wait-mount %s: %w", m, err)
		}
		if !ok {
			pending = append(pending, m+" to be mounted")
		}
	}
	for _, p := range w.Paths {
		if _, err := os.Stat(abs(p)); err != nil {
			pending = append(pending, p+" to exist")
		}
	}
	for _, a := range w.TCP {
		d := net.Dialer{Timeout: min(2*time.Second, poll*4)}
		c, err := d.DialContext(ctx, "tcp", a)
		if err != nil {
			pending = append(pending, a+" to accept connections")
			continue
		}
		c.Close()
	}
	return pending, nil
}

// IsMountPoint says whether path (symbolic links resolved) is where a
// filesystem is mounted, by /proc/self/mountinfo: an empty directory that a
// mount will cover is not one.
func IsMountPoint(path string) (bool, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(MountInfo)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && unescape(f[4]) == real {
			return true, nil
		}
	}
	return false, nil
}

// unescape undoes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Run runs a host command with sh -c in dir. Its output goes to Debug; a
// failure carries the last lines of its error output. It returns stdout.
func Run(ctx context.Context, r Report, dir, what, command string, timeout time.Duration, env []string) ([]byte, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%s: an empty command", what)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second // a child holding the pipes open does not hang Compose
	r.Info("%s: %s", what, oneLine(command))
	err := cmd.Run()
	for _, l := range lines(stdout.Bytes()) {
		r.Debug("%s: %s", what, l)
	}
	for _, l := range lines(stderr.Bytes()) {
		r.Debug("%s (stderr): %s", what, l)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %q took longer than %s", what, oneLine(command), timeout)
	}
	if err != nil {
		msg := fmt.Sprintf("%s: %q failed: %v", what, oneLine(command), err)
		if tail := lastLines(stderr.Bytes(), 5); tail != "" {
			msg += ": " + tail
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnv reads KEY=VALUE lines (an optional "export " before; blank
// lines and # comments skipped). Quotes around a value are removed.
func ParseEnv(out []byte) ([][2]string, error) {
	var env [][2]string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for no := 1; sc.Scan(); no++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envKey.MatchString(k) {
			return nil, fmt.Errorf("env: line %d is not KEY=VALUE: %q", no, oneLine(line))
		}
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		env = append(env, [2]string{k, v})
	}
	return env, sc.Err()
}

func lines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func lastLines(b []byte, n int) string {
	l := lines(b)
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " / ")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
