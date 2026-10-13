// Package rclone runs the rclone program for pushdir. pushdir never links
// rclone's code: every transfer is an rclone command, so the user's own
// rclone version, remotes and configuration apply.
package rclone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Runner runs rclone commands.
type Runner struct {
	// Bin is the rclone program (default "rclone" on PATH).
	Bin string
	// Extra flags are added to every command (bandwidth limits, filters,
	// --config, ...).
	Extra []string
	// BackupDir, when set, keeps overwritten and deleted remote files there
	// (rclone --backup-dir).
	BackupDir string
	// Log receives one line per command, when set.
	Log func(string)

	once  sync.Once
	flags flagSet
}

// Error is a failed rclone command with the end of its output.
type Error struct {
	Args   []string
	Code   int
	Output string
}

func (e *Error) Error() string {
	return Redact(fmt.Sprintf("rclone %s failed (exit %d): %s", strings.Join(e.Args, " "), e.Code, e.Output))
}

// Retryable reports whether rclone's exit code means trying the same
// command again later can work: 1 (uncategorised, often the network), 5
// (temporary error), 8 (--max-transfer reached) and 10 (--max-duration
// reached). Other failures (3 and 4 "not found", 6 "not retried") call for
// a full check instead.
func (e *Error) Retryable() bool {
	switch e.Code {
	case 1, 5, 8, 10:
		return true
	}
	return false
}

// permanentMessages are rclone errors that no retry fixes.
var permanentMessages = []string{
	"didn't find section in config file",
	"Failed to initialise global options",
	"Failed to load config file",
	"couldn't decrypt config",
	"unknown flag:",
	"unknown shorthand flag:",
	"can't use --backup-dir on a remote which doesn't support",
	"parameter to --backup-dir has to be on the same remote",
	"mustn't overlap",
}

// Permanent reports whether the failure needs the user to change something:
// a usage error (exit 2), a fatal error (exit 7, such as a suspended
// account), or a configuration problem.
func (e *Error) Permanent() bool {
	if e.Code == 2 || e.Code == 7 {
		return true
	}
	for _, m := range permanentMessages {
		if strings.Contains(e.Output, m) {
			return true
		}
	}
	return false
}

// secretParam matches sensitive parameters in rclone connection strings
// such as ":sftp,host=example.com,pass=SECRET:dir".
var secretParam = regexp.MustCompile(`(?i)([,{]\s*[a-z0-9_-]*(pass|secret|token|key|auth|cred|session|sig|sas|cookie|private)[a-z0-9_-]*\s*=\s*)("(?:[^"]|"")*"|'(?:[^']|'')*'|[^,:}\s]*)`)

// Redact hides the values of sensitive connection-string parameters.
func Redact(s string) string {
	return secretParam.ReplaceAllString(s, "${1}***")
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "rclone"
}

// running holds the rclone processes in flight, for KillAll.
var running = struct {
	sync.Mutex
	procs map[*os.Process]bool
}{procs: map[*os.Process]bool{}}

// KillAll kills every rclone process this program started that is still
// running (for a second interrupt, when waiting for a clean stop is not
// wanted).
func KillAll() {
	running.Lock()
	defer running.Unlock()
	for p := range running.procs {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL) // its process group
		_ = p.Kill()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	s := string(t.buf)
	if len(s) > t.max {
		s, t.cut = s[len(s)-t.max:], true
	}
	if t.cut {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = "..." + s
	}
	return strings.TrimSpace(s)
}

// run runs rclone with args followed by flags. Standard output is returned
// in full when wantOut is set (listings); otherwise only the end of the
// output is kept, for error messages.
func (r *Runner) run(ctx context.Context, flags []string, wantOut bool, args ...string) (string, error) {
	full := append(append([]string{}, args...), flags...)
	cmd := exec.CommandContext(ctx, r.bin(), full...)
	var out bytes.Buffer
	errs := &tailBuffer{max: 4000}
	if wantOut {
		cmd.Stdout = &out
	} else {
		cmd.Stdout = errs
	}
	cmd.Stderr = errs
	// rclone gets its own process group, so pushdir alone decides how it
	// stops: on cancel it is asked to finish cleanly instead of being killed
	// mid-upload, and KillAll takes anything it started with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	start := time.Now()
	err := cmd.Start()
	if err == nil {
		running.Lock()
		running.procs[cmd.Process] = true
		running.Unlock()
		err = cmd.Wait()
		running.Lock()
		delete(running.procs, cmd.Process)
		running.Unlock()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 9 {
		err = nil // "no files transferred" (--error-on-no-transfer) is fine here
	}
	if r.Log != nil {
		status := "ok"
		if err != nil {
			status = "failed"
		}
		r.Log(fmt.Sprintf("rclone %s: %s in %s", args[0], status, time.Since(start).Round(time.Millisecond)))
	}
	if err == nil {
		return out.String(), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.As(err, &exit) {
		return "", &Error{Args: args, Code: exit.ExitCode(), Output: dropNotices(errs.String())}
	}
	return "", fmt.Errorf("running rclone: %w", err)
}

// dropNotices removes rclone's harmless "config file not found" notice,
// which would otherwise start every error message.
func dropNotices(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "NOTICE: Config file") && strings.Contains(line, "not found") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// Version returns rclone's version line, checking that rclone works.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.run(ctx, nil, true, "version")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return line, nil
}

// Remotes lists the remotes in rclone's configuration (without the colon).
func (r *Runner) Remotes(ctx context.Context) ([]string, error) {
	out, err := r.run(ctx, r.split().listing(), true, "listremotes")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, strings.TrimSuffix(line, ":"))
		}
	}
	return names, nil
}

func (r *Runner) backupArgs() []string {
	if r.BackupDir == "" {
		return nil
	}
	return []string{"--backup-dir", r.BackupDir}
}

// CopyAll copies the whole local folder to the remote. Files that are the
// same on both sides are skipped by rclone.
func (r *Runner) CopyAll(ctx context.Context, local, remote string) error {
	args := append([]string{"copy", local, remote}, r.backupArgs()...)
	_, err := r.run(ctx, r.Extra, false, args...)
	return err
}

// SyncAll makes the remote match the local folder, deleting remote files
// that are gone locally (into the backup dir when set). maxDelete caps the
// number of deletions; rclone stops once it is reached.
func (r *Runner) SyncAll(ctx context.Context, local, remote string, maxDelete int) error {
	args := append([]string{"sync", local, remote, "--max-delete", fmt.Sprint(maxDelete)}, r.backupArgs()...)
	_, err := r.run(ctx, r.Extra, false, args...)
	return err
}

// ListFiles lists every file under path (local or remote), relative to it,
// with the user's filters applied.
func (r *Runner) ListFiles(ctx context.Context, path string) (map[string]bool, error) {
	out, err := r.run(ctx, dropProgress(r.Extra), true, "lsf", "-R", "--files-only", path)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

func lines(out string) map[string]bool {
	files := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line := strings.TrimRight(sc.Text(), "\r"); line != "" {
			files[line] = true
		}
	}
	return files
}

// Listable reports whether a name can go through rclone's file lists and
// filter rules unchanged: names with line breaks, other control characters
// or invalid UTF-8 cannot, and are left to full checks.
func Listable(p string) bool {
	if !utf8.ValidString(p) {
		return false
	}
	for _, c := range p {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// listFile writes paths (relative to the folder root) to a temporary file
// for --files-from-raw, which takes names exactly as written. Callers pass
// only Listable names.
func listFile(paths []string) (string, func(), error) {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(filepath.ToSlash(p) + "\n")
	}
	return tempFile("pushdir-list-*", b.String())
}

func tempFile(pattern, content string) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// Select returns the given paths (relative to root) that exist under root
// and pass the user's filters, in one rclone listing that only looks at the
// folders holding them. rclone's own filter rules decide, in rclone's own
// order; root can be the local folder or the remote.
func (r *Runner) Select(ctx context.Context, root string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	fl := r.split()
	var flags []string
	var done func()
	if fl.filtered() {
		rules, err := fl.ruleFile(paths)
		if err != nil {
			return nil, err
		}
		var name string
		name, done, err = tempFile("pushdir-filter-*", rules)
		if err != nil {
			return nil, err
		}
		flags = append(fl.listing(), fl.sel...)
		flags = append(flags, "--filter-from", name)
	} else {
		name, d, err := listFile(paths)
		if err != nil {
			return nil, err
		}
		done = d
		flags = append(fl.listing(), "--files-from-raw", name, "--no-traverse")
	}
	defer done()
	out, err := r.run(ctx, flags, true, "lsf", "-R", "--files-only", root)
	var re *Error
	if errors.As(err, &re) && re.Code == 3 {
		return nil, nil // the folder does not exist (yet)
	}
	if err != nil {
		return nil, err
	}
	found := lines(out)
	var keep []string
	for _, p := range paths {
		if found[filepath.ToSlash(p)] {
			keep = append(keep, p)
		}
	}
	return keep, nil
}

// CopyFiles copies the listed files (relative to local) to the remote, in
// one rclone call that does not list the whole remote. The caller has
// already applied the user's filters (Select).
func (r *Runner) CopyFiles(ctx context.Context, local, remote string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	list, done, err := listFile(paths)
	if err != nil {
		return err
	}
	defer done()
	args := append([]string{"copy", local, remote, "--files-from-raw", list, "--no-traverse"}, r.backupArgs()...)
	_, err = r.run(ctx, r.split().plain, false, args...)
	return err
}

// DeleteFiles removes the listed files (relative to remote) from the remote,
// or moves them into the backup dir when one is set.
func (r *Runner) DeleteFiles(ctx context.Context, remote string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	list, done, err := listFile(paths)
	if err != nil {
		return err
	}
	defer done()
	plain := r.split().plain
	if r.BackupDir != "" {
		_, err = r.run(ctx, plain, false, "move", remote, r.BackupDir, "--files-from-raw", list, "--no-traverse")
		return err
	}
	_, err = r.run(ctx, plain, false, "delete", remote, "--files-from-raw", list, "--no-traverse")
	return err
}

// FileInfo is what rclone reports about one file.
type FileInfo struct {
	Path    string            `json:"Path"`
	Size    int64             `json:"Size"`
	ModTime time.Time         `json:"ModTime"`
	Hashes  map[string]string `json:"Hashes"`
}

// Stat lists the given files (relative to path) that exist there. With
// hashTypes, it also asks for those hashes ("all" for every hash the
// backend has).
func (r *Runner) Stat(ctx context.Context, path string, paths []string, hashTypes ...string) ([]FileInfo, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	list, done, err := listFile(paths)
	if err != nil {
		return nil, err
	}
	defer done()
	args := []string{"lsjson", "-R", path, "--files-from-raw", list, "--no-traverse", "--files-only"}
	for _, h := range hashTypes {
		if h == "all" {
			args = append(args, "--hash")
		} else {
			args = append(args, "--hash", "--hash-type", h)
		}
	}
	out, err := r.run(ctx, r.split().listing(), true, args...)
	if err != nil {
		return nil, err
	}
	var files []FileInfo
	if err := json.Unmarshal([]byte(out), &files); err != nil {
		return nil, fmt.Errorf("reading lsjson output: %w", err)
	}
	return files, nil
}

// CopyFile copies one file on the remote (server-side where supported).
// With a backup dir, a file it replaces is kept there.
func (r *Runner) CopyFile(ctx context.Context, from, to string) error {
	_, err := r.run(ctx, r.split().plain, false, append([]string{"copyto", from, to}, r.backupArgs()...)...)
	return err
}

// MoveFile moves one file on the remote (a server-side move where the
// backend supports it). With a backup dir, a file it replaces is kept there.
func (r *Runner) MoveFile(ctx context.Context, from, to string) error {
	_, err := r.run(ctx, r.split().plain, false, append([]string{"moveto", from, to}, r.backupArgs()...)...)
	return err
}

// Join appends a relative path to a remote path such as "remote:dir".
func Join(remote, rel string) string {
	rel = filepath.ToSlash(rel)
	if remote == "" || strings.HasSuffix(remote, ":") || strings.HasSuffix(remote, "/") {
		return remote + rel
	}
	return remote + "/" + rel
}
