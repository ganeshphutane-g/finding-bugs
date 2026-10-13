// Package rclone runs the rclone program for pushdir. pushdir never links
// rclone's code: every transfer is an rclone command, so the user's own
// rclone version, remotes and configuration apply.
package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
}

// Error is a failed rclone command with the end of its output.
type Error struct {
	Args   []string
	Code   int
	Output string
}

func (e *Error) Error() string {
	return fmt.Sprintf("rclone %s failed (exit %d): %s", strings.Join(e.Args, " "), e.Code, e.Output)
}

// Retryable reports whether rclone's exit code means a temporary problem.
// rclone exits with 5 for temporary errors and with 1 for errors not
// otherwise categorised (often network trouble); usage errors (2) and
// "directory not found" (3, 4) are not worth retrying as they are.
func (e *Error) Retryable() bool {
	switch e.Code {
	case 1, 5, 6, 7, 8:
		return true
	}
	return false
}

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "rclone"
}

func (r *Runner) run(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, args...), r.Extra...)
	cmd := exec.CommandContext(ctx, r.bin(), full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	// Let rclone finish cleanly on cancel instead of being killed mid-upload.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	start := time.Now()
	err := cmd.Run()
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
		return out.String(), ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), &Error{Args: args, Code: exit.ExitCode(), Output: tail(out.String(), 2000)}
	}
	return out.String(), fmt.Errorf("running rclone: %w", err)
}

// tail keeps the last n bytes of s, starting at a line boundary.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "..." + s
}

// Version returns rclone's version line, checking that rclone works.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "version")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return line, nil
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
	_, err := r.run(ctx, args...)
	return err
}

// SyncAll makes the remote match the local folder, deleting remote files
// that are gone locally (into the backup dir when set). maxDelete caps the
// number of deletions; rclone refuses the whole run above it.
func (r *Runner) SyncAll(ctx context.Context, local, remote string, maxDelete int) error {
	args := append([]string{"sync", local, remote, "--max-delete", fmt.Sprint(maxDelete)}, r.backupArgs()...)
	_, err := r.run(ctx, args...)
	return err
}

// ListFiles lists every file under path (local or remote), relative to it,
// with the same filters as other commands.
func (r *Runner) ListFiles(ctx context.Context, path string) (map[string]bool, error) {
	out, err := r.run(ctx, "lsf", "-R", "--files-only", path)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" && !strings.Contains(line, " NOTICE: ") {
			files[line] = true
		}
	}
	return files, nil
}

// listFile writes paths (relative to the folder root) to a temporary file
// for --files-from-raw, which takes names exactly as written.
func listFile(paths []string) (string, func(), error) {
	f, err := os.CreateTemp("", "pushdir-list-*")
	if err != nil {
		return "", nil, err
	}
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			continue // cannot be listed one per line; the full resync covers it
		}
		if _, err := f.WriteString(filepath.ToSlash(p) + "\n"); err != nil {
			f.Close()
			os.Remove(f.Name())
			return "", nil, err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// CopyFiles copies the listed files (relative to local) to the remote, in
// one rclone call that does not list the whole remote.
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
	_, err = r.run(ctx, args...)
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
	if r.BackupDir != "" {
		_, err = r.run(ctx, "move", remote, r.BackupDir, "--files-from-raw", list, "--no-traverse")
		return err
	}
	_, err = r.run(ctx, "delete", remote, "--files-from-raw", list, "--no-traverse")
	return err
}

// MoveFile moves one file on the remote (a server-side move where the
// backend supports it).
func (r *Runner) MoveFile(ctx context.Context, from, to string) error {
	_, err := r.run(ctx, "moveto", from, to)
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
