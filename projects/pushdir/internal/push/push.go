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
	realLocal   string      // Local with symlinks resolved, as watched
	fatal       error       // a failure no retry fixes; Run stops with it
	lastRemoved time.Time   // latest local removal seen
	deleteLog   []time.Time // when remote deletions happened, for the hourly budget

	mu     sync.Mutex
	status Status
}

type pendingFile struct {
	first time.Time // first change seen
	last  time.Time // latest change seen
	size  int64     // size and modification time when last looked at
	mod   time.Time
	sigAt time.Time // when size or modification time last changed
}

// look records the file's size and modification time, noting when they
// change.
func (f *pendingFile) look(st os.FileInfo, now time.Time) {
	if st.Size() != f.size || !st.ModTime().Equal(f.mod) || f.sigAt.IsZero() {
		f.size, f.mod, f.sigAt = st.Size(), st.ModTime(), now
	}
}

// readyAt is when the file counts as finished: unchanged for settle since
// both its last event and its last size or time change, or after maxSettle
// of changing.
func (f *pendingFile) readyAt(settle, maxSettle time.Duration) time.Time {
	t := f.last
	if f.sigAt.After(t) {
		t = f.sigAt
	}
	t = t.Add(settle)
	if limit := f.first.Add(maxSettle); limit.Before(t) {
		return limit
	}
	return t
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
		status:      Status{Local: opt.Local, Remote: rclone.Redact(opt.Remote), State: "starting", StartedAt: time.Now()},
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
		p.opt.Log(rclone.Redact(fmt.Sprintf(format, args...)))
	}
}

func (p *Pusher) fail(err error) {
	p.log("error: %v", err)
	p.update(func(s *Status) { s.LastError, s.LastErrorTime = rclone.Redact(err.Error()), time.Now() })
}

// Run watches and pushes until ctx is cancelled. It returns an error when
// it cannot start (folder missing, rclone missing or misconfigured), when
// the watched folder itself goes away, or on a failure no retry can fix.
func (p *Pusher) Run(ctx context.Context) error {
	if _, err := p.opt.Runner.Version(ctx); err != nil {
		return fmt.Errorf("rclone does not run: %w", err)
	}
	if err := p.opt.Runner.Check(); err != nil {
		return err
	}
	if err := p.checkRemotes(ctx); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(p.opt.Local)
	if err != nil {
		return err
	}
	p.realLocal = real
	dev, err := deviceOf(real)
	if err != nil {
		return err
	}
	p.rootDev = dev

	// Watch first, so changes made during the first full copy are not missed.
	w, err := watch.New(real, p.opt.Ignore)
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
		case <-w.Ready:
			now := time.Now()
			for _, ev := range w.Take() {
				if ev.Kind == watch.RootGone {
					p.pauseDeletions("the watched folder itself was removed or moved")
					p.update(func(s *Status) { s.State = "stopped" })
					return fmt.Errorf("the watched folder %s was removed or moved away; stopping (nothing was deleted on the remote)", p.opt.Local)
				}
				p.note(ev, now)
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
			if ctx.Err() != nil {
				continue // stopping
			}
			p.flush(ctx)
			if p.fatal != nil {
				p.update(func(s *Status) { s.State = "stopped" })
				return p.fatal
			}
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

// remoteName returns the configured remote a path names ("b2" for
// "b2:bucket/dir"), or "" for local paths and on-the-fly remotes
// (":s3,...:bucket").
func remoteName(path string) string {
	i := strings.IndexByte(path, ':')
	if i <= 0 || strings.ContainsAny(path[:i], "/\\") {
		return ""
	}
	name, _, _ := strings.Cut(path[:i], ",")
	return name
}

// checkRemotes makes sure named remotes exist in rclone's configuration, so
// a typo or a missing config file stops pushdir at start instead of failing
// every batch.
func (p *Pusher) checkRemotes(ctx context.Context) error {
	var names []string
	for _, path := range []string{p.opt.Remote, p.opt.Runner.BackupDir} {
		if n := remoteName(path); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	have, err := p.opt.Runner.Remotes(ctx)
	if err != nil {
		return fmt.Errorf("listing rclone's remotes: %w", err)
	}
	for _, n := range names {
		found := false
		for _, h := range have {
			found = found || h == n
		}
		if !found {
			return fmt.Errorf("rclone has no remote called %q (check 'rclone listremotes' as the user pushdir runs as, or pass --config after --)", n)
		}
	}
	return nil
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
	case watch.Rescan:
		p.log("%s; a full check follows", ev.Reason)
		p.resync = true
	case watch.Changed:
		delete(p.removed, ev.Path)
		f, ok := p.pending[ev.Path]
		if !ok {
			f = &pendingFile{first: now}
			p.pending[ev.Path] = f
		}
		f.last = now
		if st, err := os.Stat(p.abs(ev.Path)); err == nil {
			f.look(st, now)
		}
	case watch.Removed:
		p.lastRemoved = now
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
	// The quiet period counts from the latest change of any kind, so a
	// removal and the new name of a rename land in the same batch.
	latest := p.lastRemoved
	for _, f := range p.pending {
		if f.last.After(latest) {
			latest = f.last
		}
	}
	at := latest.Add(p.opt.Quiet)
	if limit := p.batchStart.Add(p.opt.MaxDelay); limit.Before(at) {
		at = limit
	}
	if len(p.pending) > 0 {
		// Not before the first file can be finished.
		first := time.Time{}
		for _, f := range p.pending {
			if r := f.readyAt(p.opt.Settle, p.opt.MaxSettle); first.IsZero() || r.Before(first) {
				first = r
			}
		}
		if first.After(at) {
			at = first
		}
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
	if now.Before(p.retryAt) || ctx.Err() != nil {
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
		if !rclone.Listable(path) {
			delete(p.pending, path)
			p.log("%q cannot be sent on its own (unusual characters); a full check follows", path)
			p.resync = true
			continue
		}
		st, err := os.Stat(p.abs(path))
		switch {
		case errors.Is(err, os.ErrNotExist):
			delete(p.pending, path)
			p.removed[path] = true
		case err != nil:
			delete(p.pending, path)
			p.fail(fmt.Errorf("cannot read %s: %w", path, err))
		case !st.Mode().IsRegular():
			delete(p.pending, path)
		default:
			f.look(st, now)
			if now.Before(f.readyAt(p.opt.Settle, p.opt.MaxSettle)) {
				settling = true
			} else {
				ready = append(ready, path)
			}
		}
	}
	if p.resync {
		p.fullSync(ctx)
		return
	}
	sort.Strings(ready)
	p.update(func(s *Status) { s.State = "sending" })
	ready = p.renames(ctx, ready)
	if len(ready) > 0 && p.opt.Runner.Filtered() {
		// Leave out what the user's rclone filters exclude.
		sel, err := p.opt.Runner.Select(ctx, p.realLocal, ready)
		if err != nil {
			p.retry(fmt.Errorf("applying filters: %w", err))
			return
		}
		keep := map[string]bool{}
		for _, f := range sel {
			keep[f] = true
		}
		for _, f := range ready {
			if !keep[f] {
				delete(p.pending, f)
			}
		}
		ready = sel
	}

	ok := true
	if len(ready) > 0 {
		err := p.opt.Runner.CopyFiles(ctx, p.realLocal, p.opt.Remote, ready)
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

// renameMin is the smallest file worth turning into a remote-side move.
const renameMin = 1 << 20

// renames finds large new files that are a removed file under a new name
// and moves or copies them on the remote instead of uploading them again.
// A match needs the same size and modification time as the removed file has
// on the remote, and then the same content hash, so a different file that
// happens to look alike is uploaded, never put in its place. Without
// --delete the old name is kept (a remote-side copy). It returns the files
// still to upload.
func (p *Pusher) renames(ctx context.Context, ready []string) []string {
	if len(p.removed) == 0 || len(ready) == 0 {
		return ready
	}
	type key struct {
		size int64
		mod  int64
	}
	var big []string
	local := map[string]key{}
	for _, f := range ready {
		st, err := os.Stat(p.abs(f))
		if err == nil && st.Size() >= renameMin {
			big = append(big, f)
			local[f] = key{st.Size(), st.ModTime().Unix()}
		}
	}
	if len(big) == 0 {
		return ready
	}
	var gone []string
	for f := range p.removed {
		if rclone.Listable(f) {
			gone = append(gone, f)
		}
	}
	sort.Strings(gone)
	if p.opt.Runner.Filtered() {
		// A remote file the filters exclude is not pushdir's to move.
		var err error
		if gone, err = p.opt.Runner.Select(ctx, p.opt.Remote, gone); err != nil {
			return ready
		}
	}
	infos, err := p.opt.Runner.Stat(ctx, p.opt.Remote, gone)
	if err != nil {
		return ready // just upload
	}
	byKey := map[key][]string{}
	for _, in := range infos {
		k := key{in.Size, in.ModTime.Unix()}
		byKey[k] = append(byKey[k], in.Path)
	}
	moved := map[string]bool{}
	for _, f := range big {
		cands := byKey[local[f]]
		if len(cands) != 1 {
			continue // none, or ambiguous
		}
		old := cands[0]
		if !p.sameContent(ctx, old, f) {
			continue
		}
		from, to := rclone.Join(p.opt.Remote, old), rclone.Join(p.opt.Remote, f)
		var err error
		if p.opt.Delete && p.deletionsAllowed() {
			err = p.opt.Runner.MoveFile(ctx, from, to)
		} else {
			err = p.opt.Runner.CopyFile(ctx, from, to)
		}
		if err != nil {
			continue // upload it normally instead
		}
		delete(byKey, local[f])
		delete(p.removed, old)
		moved[f] = true
		p.log("renamed %s to %s on the remote", old, f)
	}
	if len(moved) == 0 {
		return ready
	}
	var rest []string
	for _, f := range ready {
		if moved[f] {
			delete(p.pending, f)
			p.update(func(s *Status) { s.Sent++ })
			continue
		}
		rest = append(rest, f)
	}
	return rest
}

// sameContent compares the remote file old with the local file f by a hash
// the remote has (md5 or sha1 preferred). Without a usable hash the answer
// is no, and the file is simply uploaded.
func (p *Pusher) sameContent(ctx context.Context, old, f string) bool {
	rinfo, err := p.opt.Runner.Stat(ctx, p.opt.Remote, []string{old}, "all")
	if err != nil || len(rinfo) != 1 {
		return false
	}
	var types []string
	for t, v := range rinfo[0].Hashes {
		if v != "" {
			types = append(types, t)
		}
	}
	sort.Slice(types, func(i, j int) bool { return hashRank(types[i]) < hashRank(types[j]) })
	if len(types) == 0 {
		return false
	}
	t := types[0]
	linfo, err := p.opt.Runner.Stat(ctx, p.realLocal, []string{f}, t)
	if err != nil || len(linfo) != 1 {
		return false
	}
	lh, rh := linfo[0].Hashes[t], rinfo[0].Hashes[t]
	return lh != "" && strings.EqualFold(lh, rh)
}

func hashRank(t string) string {
	switch t {
	case "md5":
		return "0"
	case "sha1":
		return "1"
	case "sha256":
		return "2"
	}
	return "3" + t
}

// retry schedules the next attempt with exponential backoff.
func (p *Pusher) retry(err error) {
	if errors.Is(err, context.Canceled) {
		return // shutting down; the next start does a full check
	}
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
	if errors.As(err, &re) && re.Permanent() {
		p.fatal = fmt.Errorf("%w (this needs fixing before pushdir can work)", err)
		p.fail(p.fatal)
		return
	}
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
		left := p.opt.MaxDelete - p.deletedLastHour()
		switch {
		case err != nil:
		case n > left:
			p.pauseDeletions(fmt.Sprintf("a full sync would delete %d files on the remote (%d more allowed within the hour by --max-delete %d)", n, left, p.opt.MaxDelete))
			err = p.opt.Runner.CopyAll(ctx, p.realLocal, p.opt.Remote)
		default:
			// rclone stops at the budget even if more files vanish meanwhile.
			err = p.opt.Runner.SyncAll(ctx, p.realLocal, p.opt.Remote, left)
			if err == nil {
				for i := 0; i < n; i++ {
					p.deleteLog = append(p.deleteLog, time.Now())
				}
			}
		}
	} else {
		err = p.opt.Runner.CopyAll(ctx, p.realLocal, p.opt.Remote)
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
	local, err := p.opt.Runner.ListFiles(ctx, p.realLocal)
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
	real, rerr := filepath.EvalSymlinks(p.opt.Local)
	dev, err := deviceOf(p.realLocal)
	switch {
	case rerr == nil && real != p.realLocal:
		p.pauseDeletions(fmt.Sprintf("%s now points to %s, not %s", p.opt.Local, real, p.realLocal))
		return false
	case err != nil:
		p.pauseDeletions(fmt.Sprintf("the watched folder cannot be read (%v)", err))
		return false
	case dev != p.rootDev:
		p.pauseDeletions("the watched folder is now on a different disk (unmounted or remounted?)")
		return false
	}
	entries, err := os.ReadDir(p.realLocal)
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
		if _, back := os.Lstat(p.abs(f)); back == nil {
			delete(p.removed, f)
			continue
		}
		if !rclone.Listable(f) {
			delete(p.removed, f)
			p.resync = true // a full sync removes it
			continue
		}
		files = append(files, f)
	}
	sort.Strings(files)
	// Only files that are on the remote (and inside the user's filters)
	// count: temporary files of an atomic save never reached it.
	files, err := p.opt.Runner.Select(ctx, p.opt.Remote, files)
	if err != nil {
		p.retry(fmt.Errorf("checking files to delete: %w", err))
		return false
	}
	onRemote := map[string]bool{}
	for _, f := range files {
		onRemote[f] = true
	}
	for f := range p.removed {
		if !onRemote[f] {
			delete(p.removed, f)
		}
	}
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

func (p *Pusher) abs(rel string) string {
	return filepath.Join(p.realLocal, filepath.FromSlash(rel))
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
