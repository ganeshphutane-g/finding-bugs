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
	if n, err := id(u); err == nil {
		o.UID = n
	} else if isNumber(u) {
		return o, fmt.Errorf("owner %q: %v", s, err)
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
		if n, err := id(g); err == nil {
			o.GID = n
		} else if isNumber(g) {
			return o, fmt.Errorf("owner %q: %v", s, err)
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

// id reads a numeric uid or gid: 32 bits, and not 4294967295 (-1 to the
// kernel: "leave it").
func id(s string) (int, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 1<<32-1 {
		return 0, fmt.Errorf("%s is not a valid user or group number", s)
	}
	return int(n), nil
}

func isNumber(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// ParseMode reads an octal permission mode such as 0750 or 750 (no setuid,
// setgid or sticky bits).
func ParseMode(s string) (fs.FileMode, error) {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o777 {
		hint := ""
		// YAML reads an unquoted 0750 as octal, and Compose passes 488.
		if d, err := strconv.ParseUint(s, 10, 32); err == nil && d <= 0o777 {
			hint = fmt.Sprintf(" (an unquoted %04o in YAML becomes %d: quote it, mode: \"%04o\")", d, d, d)
		}
		return 0, fmt.Errorf("mode %q: an octal permission mode such as 0750 (up to 0777)%s", s, hint)
	}
	return fs.FileMode(n), nil
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
	parent := ctx
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
			if parent.Err() != nil {
				return fmt.Errorf("stopped while waiting for %s", strings.Join(pending, ", "))
			}
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
		if _, err := os.Stat(abs(p)); errors.Is(err, fs.ErrNotExist) {
			pending = append(pending, p+" to exist")
		} else if err != nil {
			return nil, fmt.Errorf("wait-path %s: %w", p, err)
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
// mount will cover is not one, nor an automount point not yet mounted.
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
	// The last mount at the path is the one on top. An autofs one is only a
	// placeholder: the real filesystem is mounted over it on first use.
	mounted := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && unescape(f[4]) == real {
			mounted = true
			for i := 5; i+1 < len(f); i++ {
				if f[i] == "-" {
					mounted = f[i+1] != "autofs"
					break
				}
			}
		}
	}
	return mounted, nil
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
// failure carries the last lines of its error output.
func Run(ctx context.Context, r Report, dir, what, command string, timeout time.Duration, env []string) error {
	_, err := run(ctx, r, dir, what, command, timeout, env, true)
	return err
}

// Output is Run for a command whose output is the result, such as
// secrets: it returns stdout, and only stderr goes to Debug.
func Output(ctx context.Context, r Report, dir, what, command string, timeout time.Duration, env []string) ([]byte, error) {
	return run(ctx, r, dir, what, command, timeout, env, false)
}

func run(ctx context.Context, r Report, dir, what, command string, timeout time.Duration, env []string, showStdout bool) ([]byte, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("%s: an empty command", what)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	stdout, stderr := &capped{max: maxOutput}, &capped{max: maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A child left running in the background (and holding the pipes open)
	// does not hang Compose; on a timeout, the whole group is killed.
	cmd.WaitDelay = 2 * time.Second
	inGroup(cmd)
	// Compose has already put the values of variables into the command, so
	// it may hold secrets: only its first word is shown.
	name := short(command)
	r.Info("%s: %s", what, name)
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// It exited 0; something it started still has its output.
		r.Debug("%s: a process it started still holds its output, which is no longer read", what)
		err = nil
	}
	if showStdout {
		for _, l := range lines(stdout.Bytes()) {
			r.Debug("%s: %s", what, l)
		}
	}
	for _, l := range lines(stderr.Bytes()) {
		r.Debug("%s (stderr): %s", what, l)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s: %s took longer than %s", what, name, timeout)
	}
	if err == nil && stdout.over && !showStdout {
		return nil, fmt.Errorf("%s: %s printed more than %d MiB", what, name, maxOutput>>20)
	}
	if err != nil {
		msg := fmt.Sprintf("%s: %s failed: %v", what, name, err)
		if tail := lastLines(stderr.Bytes(), 5); tail != "" {
			msg += ": " + tail
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnv reads KEY=VALUE lines (an optional "export " before; blank
// lines and # comments skipped). Matching quotes around the whole value are
// removed; nothing else is (no escapes, no comments after a value).
func ParseEnv(out []byte) ([][2]string, error) {
	var env [][2]string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20) // a certificate on one line
	for no := 1; sc.Scan(); no++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envKey.MatchString(k) {
			// The line is likely a secret: it is not shown.
			return nil, fmt.Errorf("env: line %d of the output is not KEY=VALUE", no)
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

// maxOutput is the most of a command's output kept, for each stream.
const maxOutput = 4 << 20

// capped keeps the first max bytes written to it. (Not an embedded
// bytes.Buffer: io.Copy would use its ReadFrom, past the cap.)
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(b) > room {
		c.over = true
		c.buf.Write(b[:max(room, 0)])
		return len(b), nil
	}
	return c.buf.Write(b)
}

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// short is a command's first word (after any NAME=value ones: values may
// be secrets), and "…" if there is more.
func short(command string) string {
	f := strings.Fields(command)
	for len(f) > 0 && assignment.MatchString(f[0]) {
		f = f[1:]
	}
	if len(f) == 0 {
		return "…"
	}
	w := strings.TrimRight(f[0], ";&|")
	if strings.ContainsAny(w, "=$`'\"") {
		return "…"
	}
	if len(f) > 1 || w != f[0] || len(f) < len(strings.Fields(command)) {
		w += " …"
	}
	return w
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
