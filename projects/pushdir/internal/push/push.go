// Package push keeps a remote copy of a local folder up to date: a full copy
// at start, then batches of changed files as they settle, with deletions
// only when asked for and never when the folder looks unmounted or emptied.
package push

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/pushdir/internal/rclone"
	"github.com/ganeshphutane-g/pushdir/internal/watch"
)

// Options configure a Pusher.
type Options struct {
	Local  string // folder to watch
	Remote string // rclone destination, e.g. "b2:bucket/photos"

	// Delete mirrors local deletions to the remote.
	Delete bool
	// MaxDelete is the most files that may be deleted on the remote within
	// one hour (and by one full sync); more than that pauses deletions until
	// pushdir is restarted.
	MaxDelete int
	// DeleteQuiet holds deletions until no file has been removed for this
	// long, so a burst of removals is judged as a whole (default 10s).
	DeleteQuiet time.Duration

	Quiet     time.Duration // wait this long after the last change before sending
	MaxDelay  time.Duration // never hold a change longer than this before trying
	Settle    time.Duration // a file must be unchanged this long to be sent
	MaxSettle time.Duration // send a file that keeps changing after this long anyway
	Resync    time.Duration // full check this often (0: never)

	Runner *rclone.Runner
	Log    func(string)
	// Ignore skips paths (and whole folders). rclone's own filters, passed as
	// extra flags, apply as well.
	Ignore func(rel string, dir bool) bool
}

func (o *Options) defaults() {
	if o.MaxDelete == 0 {
		o.MaxDelete = 100
	}
	if o.Quiet == 0 {
		o.Quiet = 2 * time.Second
	}
	if o.MaxDelay == 0 {
		o.MaxDelay = 30 * time.Second
	}
	if o.Settle == 0 {
		o.Settle = 2 * time.Second
	}
	if o.DeleteQuiet == 0 {
		o.DeleteQuiet = 10 * time.Second
	}
	if o.MaxSettle == 0 {
		o.MaxSettle = 10 * time.Minute
	}
	if o.Runner == nil {
		o.Runner = &rclone.Runner{}
	}
}

// Pusher runs the watch-and-push loop.
type Pusher struct {
	opt Options

	// loop state (only touched by the loop goroutine)
	pending     map[string]*pendingFile
	removed     map[string]bool // files removed locally, not yet removed remotely
	removedDirs map[string]bool
	resync      bool
	batchStart  time.Time
	retryAt     time.Time
	backoff     time.Duration
	rootDev     uint64
	lastRemoved time.Time   // latest local removal seen
	deleteLog   []time.Time // when remote deletions happened, for the hourly budget

	mu     sync.Mutex
	status Status
}

type pendingFile struct {
	first time.Time // first change seen
	last  time.Time // latest change seen
}

// Status is what /status reports.
type Status struct {
	Local           string    `json:"local"`
	Remote          string    `json:"remote"`
	State           string    `json:"state"` // starting, idle, sending, retrying, resyncing
	WatchedFolders  int       `json:"watched_folders"`
	Pending         int       `json:"pending_files"`
	PendingDeletes  int       `json:"pending_deletes"`
	Sent            int64     `json:"files_sent"`
	Deleted         int64     `json:"files_deleted"`
	Batches         int64     `json:"batches"`
	FailedBatches   int64     `json:"failed_batches"`
	Resyncs         int64     `json:"full_resyncs"`
	LastSuccess     time.Time `json:"last_success,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastErrorTime   time.Time `json:"last_error_time,omitempty"`
	DeletionsPaused string    `json:"deletions_paused,omitempty"`
	StartedAt       time.Time `json:"started_at"`
}

// New creates a Pusher.
func New(opt Options) *Pusher {
	opt.defaults()
	return &Pusher{
		opt:         opt,
		pending:     map[string]*pendingFile{},
		removed:     map[string]bool{},
		removedDirs: map[string]bool{},
		status:      Status{Local: opt.Local, Remote: opt.Remote, State: "starting", StartedAt: time.Now()},
	}
}

// Status returns a snapshot of the current status.
func (p *Pusher) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Pusher) update(f func(*Status)) {
	p.mu.Lock()
	f(&p.status)
	p.mu.Unlock()
}

func (p *Pusher) log(format string, args ...any) {
	if p.opt.Log != nil {
		p.opt.Log(fmt.Sprintf(format, args...))
	}
}

func (p *Pusher) fail(err error) {
	p.log("error: %v", err)
	p.update(func(s *Status) { s.LastError, s.LastErrorTime = err.Error(), time.Now() })
}

// Run watches and pushes until ctx is cancelled. It returns an error only
// when it cannot start (folder missing, rclone missing).
func (p *Pusher) Run(ctx context.Context) error {
	if _, err := p.opt.Runner.Version(ctx); err != nil {
		return fmt.Errorf("rclone does not run: %w", err)
	}
	dev, err := deviceOf(p.opt.Local)
	if err != nil {
		return err
	}
	p.rootDev = dev

	// Watch first, so changes made during the first full copy are not missed.
	w, err := watch.New(p.opt.Local, p.opt.Ignore)
	if err != nil {
		return err
	}
	defer w.Close()
	p.update(func(s *Status) { s.WatchedFolders = w.Dirs() })
	p.log("watching %s (%d folders), sending to %s", p.opt.Local, w.Dirs(), p.opt.Remote)

	p.resync = true
	p.batchStart = time.Now()
	timer := time.NewTimer(0) // run the first full copy straight away
	defer timer.Stop()
	var resyncTick <-chan time.Time
	if p.opt.Resync > 0 {
		t := time.NewTicker(p.opt.Resync)
		defer t.Stop()
		resyncTick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			p.update(func(s *Status) { s.State = "stopped" })
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("the folder watch stopped")
			}
			p.note(ev, time.Now())
			// Drain whatever else is queued before deciding when to send.
			for drained := false; !drained; {
				select {
				case ev, ok := <-w.Events:
					if !ok {
						return errors.New("the folder watch stopped")
					}
					p.note(ev, time.Now())
				default:
					drained = true
				}
			}
			resetTimer(timer, p.nextFlush(time.Now()))
		case err := <-w.Errors:
			if err != nil {
				p.fail(fmt.Errorf("watching: %w", err))
			}
		case <-resyncTick:
			p.resync = true
			resetTimer(timer, 0)
		case <-timer.C:
			p.flush(ctx)
			p.update(func(s *Status) {
				s.WatchedFolders = w.Dirs()
				s.Pending, s.PendingDeletes = len(p.pending), len(p.removed)+len(p.removedDirs)
			})
			if next := p.nextFlush(time.Now()); next >= 0 {
				resetTimer(timer, next)
			}
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	if d < 0 {
		return
	}
	t.Reset(d)
}

// note records one event.
func (p *Pusher) note(ev watch.Event, now time.Time) {
	if len(p.pending) == 0 && len(p.removed) == 0 && len(p.removedDirs) == 0 && !p.resync {
		p.batchStart = now
	}
	switch ev.Kind {
	case watch.Overflow:
		p.log("the kernel dropped file events; a full check follows")
		p.resync = true
	case watch.Changed:
		delete(p.removed, ev.Path)
		if f, ok := p.pending[ev.Path]; ok {
			f.last = now
		} else {
			p.pending[ev.Path] = &pendingFile{first: now, last: now}
		}
	case watch.Removed:
		p.lastRemoved = now
		if ev.Path == "" {
			p.fail(errors.New("the watched folder itself was removed or moved; nothing will be deleted remotely"))
			p.resync = true
			return
		}
		delete(p.pending, ev.Path)
		if ev.Dir {
			prefix := ev.Path + "/"
			for k := range p.pending {
				if strings.HasPrefix(k, prefix) {
					delete(p.pending, k)
				}
			}
			p.removedDirs[ev.Path] = true
		} else {
			p.removed[ev.Path] = true
		}
	}
}

// nextFlush is how long to wait before the next flush, or -1 for no flush.
func (p *Pusher) nextFlush(now time.Time) time.Duration {
	if len(p.pending) == 0 && len(p.removed) == 0 && len(p.removedDirs) == 0 && !p.resync {
		return -1
	}
	var latest time.Time
	for _, f := range p.pending {
		if f.last.After(latest) {
			latest = f.last
		}
	}
	at := latest.Add(p.opt.Quiet)
	if limit := p.batchStart.Add(p.opt.MaxDelay); limit.Before(at) {
		at = limit
	}
	if len(p.pending) == 0 && (len(p.removed) > 0 || len(p.removedDirs) > 0) && p.opt.Delete {
		at = p.lastRemoved.Add(p.opt.DeleteQuiet)
	}
	if p.resync && len(p.pending) == 0 {
		at = now
	}
	if p.retryAt.After(at) {
		at = p.retryAt
	}
	if d := at.Sub(now); d > 0 {
		return d
	}
	return 0
}

// flush sends what is ready.
func (p *Pusher) flush(ctx context.Context) {
	now := time.Now()
	if now.Before(p.retryAt) {
		return
	}
	if p.resync {
		p.fullSync(ctx)
		return
	}

	// Files that have settled.
	var ready []string
	settling := false
	for path, f := range p.pending {
		st, err := os.Stat(filepath.Join(p.opt.Local, filepath.FromSlash(path)))
		switch {
		case err != nil:
			delete(p.pending, path)
			if errors.Is(err, os.ErrNotExist) {
				p.removed[path] = true
			}
		case !st.Mode().IsRegular():
			delete(p.pending, path)
		case now.Sub(st.ModTime()) < p.opt.Settle && now.Sub(f.first) < p.opt.MaxSettle:
			settling = true
		default:
			ready = append(ready, path)
		}
	}
	sort.Strings(ready)

	p.update(func(s *Status) { s.State = "sending" })
	ok := true
	if len(ready) > 0 {
		err := p.opt.Runner.CopyFiles(ctx, p.opt.Local, p.opt.Remote, ready)
		if err != nil {
			ok = false
			p.retry(fmt.Errorf("sending %d %s: %w", len(ready), plural(len(ready), "file"), err))
		} else {
			for _, path := range ready {
				delete(p.pending, path)
			}
			p.log("sent %d %s", len(ready), plural(len(ready), "file"))
			p.update(func(s *Status) { s.Sent += int64(len(ready)); s.Batches++ })
		}
	}
	if ok && p.opt.Delete {
		if now.Sub(p.lastRemoved) >= p.opt.DeleteQuiet {
			ok = p.applyDeletes(ctx)
		}
	} else if !p.opt.Delete {
		// Deletions are not mirrored; forget them.
		p.removed, p.removedDirs = map[string]bool{}, map[string]bool{}
	}
	if ok {
		p.backoff = 0
		p.update(func(s *Status) { s.LastSuccess = time.Now(); s.State = "idle" })
	}
	if settling {
		p.update(func(s *Status) { s.State = "waiting for files to settle" })
	}
	p.batchStart = time.Now()
}

// retry schedules the next attempt with exponential backoff.
func (p *Pusher) retry(err error) {
	if p.backoff == 0 {
		p.backoff = time.Second
	} else {
		p.backoff *= 2
		if p.backoff > 5*time.Minute {
			p.backoff = 5 * time.Minute
		}
	}
	p.retryAt = time.Now().Add(p.backoff)
	var re *rclone.Error
	if errors.As(err, &re) && !re.Retryable() {
		// A permanent-looking failure in a batch: a full check finds out
		// which files really still need sending.
		p.resync = true
	}
	p.fail(fmt.Errorf("%w; retrying in %s", err, p.backoff))
	p.update(func(s *Status) { s.FailedBatches++; s.State = "retrying" })
}

// fullSync copies (or, with Delete, syncs) the whole folder.
func (p *Pusher) fullSync(ctx context.Context) {
	p.update(func(s *Status) { s.State = "resyncing" })
	p.log("full check of %s", p.opt.Local)
	var err error
	if p.opt.Delete && p.deletionsAllowed() {
		// rclone's --max-delete deletes up to the limit before it stops, so
		// count first and refuse the whole sync instead.
		var n int
		n, err = p.countRemoteOnly(ctx)
		switch {
		case err != nil:
		case n+p.deletedLastHour() > p.opt.MaxDelete:
			p.pauseDeletions(fmt.Sprintf("a full sync would delete %d files on the remote (more than --max-delete %d)", n, p.opt.MaxDelete))
			err = p.opt.Runner.CopyAll(ctx, p.opt.Local, p.opt.Remote)
		default:
			err = p.opt.Runner.SyncAll(ctx, p.opt.Local, p.opt.Remote, p.opt.MaxDelete)
			if err == nil {
				for i := 0; i < n; i++ {
					p.deleteLog = append(p.deleteLog, time.Now())
				}
			}
		}
	} else {
		err = p.opt.Runner.CopyAll(ctx, p.opt.Local, p.opt.Remote)
	}
	if err != nil {
		if ctx.Err() == nil {
			p.retry(fmt.Errorf("full check: %w", err))
		}
		return
	}
	// Everything up to now is on the remote.
	p.resync = false
	p.pending = map[string]*pendingFile{}
	p.removed, p.removedDirs = map[string]bool{}, map[string]bool{}
	p.backoff = 0
	p.log("full check done")
	p.update(func(s *Status) { s.Resyncs++; s.LastSuccess = time.Now(); s.State = "idle" })
}

// deletedLastHour counts remote deletions in the past hour.
func (p *Pusher) deletedLastHour() int {
	cut := time.Now().Add(-time.Hour)
	keep := p.deleteLog[:0]
	for _, t := range p.deleteLog {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	p.deleteLog = keep
	return len(keep)
}

// countRemoteOnly counts remote files that a sync would delete.
func (p *Pusher) countRemoteOnly(ctx context.Context) (int, error) {
	local, err := p.opt.Runner.ListFiles(ctx, p.opt.Local)
	if err != nil {
		return 0, fmt.Errorf("listing the folder: %w", err)
	}
	remote, err := p.opt.Runner.ListFiles(ctx, p.opt.Remote)
	var re *rclone.Error
	if errors.As(err, &re) && re.Code == 3 {
		return 0, nil // the remote folder does not exist yet
	}
	if err != nil {
		return 0, fmt.Errorf("listing the remote: %w", err)
	}
	n := 0
	for f := range remote {
		if !local[f] {
			n++
		}
	}
	return n, nil
}

// deletionsAllowed refuses to mirror deletions when the folder looks gone,
// unmounted or emptied.
func (p *Pusher) deletionsAllowed() bool {
	p.mu.Lock()
	paused := p.status.DeletionsPaused
	p.mu.Unlock()
	if paused != "" {
		return false
	}
	dev, err := deviceOf(p.opt.Local)
	switch {
	case err != nil:
		p.pauseDeletions(fmt.Sprintf("the watched folder cannot be read (%v)", err))
		return false
	case dev != p.rootDev:
		p.pauseDeletions("the watched folder is now on a different disk (unmounted or remounted?)")
		return false
	}
	entries, err := os.ReadDir(p.opt.Local)
	if err == nil && len(entries) == 0 {
		p.pauseDeletions("the watched folder is empty (unmounted?)")
		return false
	}
	return true
}

func (p *Pusher) pauseDeletions(reason string) {
	p.fail(fmt.Errorf("deletions paused: %s; nothing more will be deleted on the remote until pushdir is restarted", reason))
	p.update(func(s *Status) { s.DeletionsPaused = reason })
}

// applyDeletes mirrors local deletions, within the MaxDelete budget.
func (p *Pusher) applyDeletes(ctx context.Context) bool {
	if len(p.removed) == 0 && len(p.removedDirs) == 0 {
		return true
	}
	if !p.deletionsAllowed() {
		p.removed, p.removedDirs = map[string]bool{}, map[string]bool{}
		return true
	}
	// A file deleted and created again is in pending, not removed.
	var files []string
	for f := range p.removed {
		if _, back := os.Lstat(filepath.Join(p.opt.Local, filepath.FromSlash(f))); back == nil {
			delete(p.removed, f)
			continue
		}
		files = append(files, f)
	}
	sort.Strings(files)
	if used := p.deletedLastHour(); used+len(files) > p.opt.MaxDelete {
		p.pauseDeletions(fmt.Sprintf("%d files were deleted locally (%d more within the last hour), more than --max-delete %d", len(files), used, p.opt.MaxDelete))
		p.removed, p.removedDirs = map[string]bool{}, map[string]bool{}
		return true
	}
	if len(files) > 0 {
		if err := p.opt.Runner.DeleteFiles(ctx, p.opt.Remote, files); err != nil {
			p.retry(fmt.Errorf("deleting %d %s: %w", len(files), plural(len(files), "file"), err))
			return false
		}
		for _, f := range files {
			delete(p.removed, f)
			p.deleteLog = append(p.deleteLog, time.Now())
		}
		p.log("deleted %d %s", len(files), plural(len(files), "file"))
		p.update(func(s *Status) { s.Deleted += int64(len(files)) })
	}
	if len(p.removedDirs) > 0 {
		// Whole folders: a full sync removes exactly what is gone, with
		// rclone's own --max-delete as the guard.
		p.resync = true
		p.removedDirs = map[string]bool{}
	}
	return true
}

// deviceOf returns the device ID of a path, to notice unmounts.
func deviceOf(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("%s is not a folder", path)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}
	return uint64(sys.Dev), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
