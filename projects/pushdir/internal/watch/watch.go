// Package watch reports changes anywhere under a folder, including folders
// created after watching started.
package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Kind is what happened to a path.
type Kind int

const (
	// Changed: the path was created or written (it may since be gone).
	Changed Kind = iota
	// Removed: the path was deleted or moved away.
	Removed
	// Rescan: changes may have been missed (the kernel dropped events, or a
	// new folder could not be watched); only a full check is reliable.
	Rescan
	// RootGone: the watched folder itself was removed or moved away. No
	// further events arrive.
	RootGone
)

func (k Kind) String() string {
	switch k {
	case Changed:
		return "changed"
	case Removed:
		return "removed"
	case Rescan:
		return "rescan"
	}
	return "root gone"
}

// Event is one change. Path is relative to the watched root, with forward
// slashes. Dir is true when the path is (or was) a known folder. Reason says
// why a Rescan is needed.
type Event struct {
	Kind   Kind
	Path   string
	Dir    bool
	Reason string
}

// maxQueued is the most events held for the reader; beyond it they are
// replaced by one Rescan, so memory stays bounded however busy the folder.
const maxQueued = 1 << 20

// Watcher watches a folder tree. Events are queued without limit (up to
// maxQueued) while the reader is busy, so the kernel's own queue keeps
// draining during long uploads.
type Watcher struct {
	root   string
	ignore func(rel string, dir bool) bool
	fw     *fsnotify.Watcher

	mu        sync.Mutex
	dirs      map[string]bool // watched folders, relative
	queue     []Event
	changedAt map[string]int // index in queue of a queued Changed event
	lastGone  int            // index in queue of the latest Removed event, or -1

	// Ready receives a value when events are waiting to be taken.
	Ready chan struct{}
	// Errors receives watch errors; they are dropped if nobody reads them.
	Errors chan error
	quit   chan struct{}
	done   chan struct{}
}

// New starts watching root. ignore, when set, skips paths (and whole
// folders) it returns true for.
func New(root string, ignore func(rel string, dir bool) bool) (*Watcher, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", root)
	}
	fw, err := fsnotify.NewBufferedWatcher(4096)
	if err != nil {
		return nil, fmt.Errorf("starting the file watch: %w (raise fs.inotify.max_user_instances if many programs watch files)", err)
	}
	w := &Watcher{
		root:      abs,
		ignore:    ignore,
		fw:        fw,
		dirs:      map[string]bool{},
		changedAt: map[string]int{},
		lastGone:  -1,
		Ready:     make(chan struct{}, 1),
		Errors:    make(chan error, 16),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if _, err := w.addTree(""); err != nil {
		fw.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// Close stops watching.
func (w *Watcher) Close() error {
	select {
	case <-w.quit:
	default:
		close(w.quit)
	}
	err := w.fw.Close()
	<-w.done
	return err
}

// Dirs is the number of folders being watched.
func (w *Watcher) Dirs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.dirs)
}

// Take returns the queued events, oldest first, and empties the queue.
func (w *Watcher) Take() []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.queue
	w.queue = nil
	w.changedAt = map[string]int{}
	w.lastGone = -1
	return q
}

func (w *Watcher) rel(abs string) (string, bool) {
	r, err := filepath.Rel(w.root, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	if r == "." {
		return "", true
	}
	return filepath.ToSlash(r), true
}

// addTree watches rel and every folder below it, and returns the files found
// (relative paths), which may have been created before the watch was in
// place.
func (w *Watcher) addTree(rel string) ([]string, error) {
	var files []string
	start := filepath.Join(w.root, filepath.FromSlash(rel))
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // vanished while walking
			}
			return err
		}
		r, ok := w.rel(p)
		if !ok {
			return nil
		}
		if d.IsDir() {
			if r != "" && w.ignore != nil && w.ignore(r, true) {
				return filepath.SkipDir
			}
			if err := w.fw.Add(p); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return filepath.SkipDir
				}
				return fmt.Errorf("watching %s: %w (raise fs.inotify.max_user_watches if there are many folders)", p, err)
			}
			w.mu.Lock()
			w.dirs[r] = true
			w.mu.Unlock()
			return nil
		}
		if d.Type().IsRegular() && (w.ignore == nil || !w.ignore(r, false)) {
			files = append(files, r)
		}
		return nil
	})
	return files, err
}

// send queues an event and wakes the reader. It never blocks.
func (w *Watcher) send(e Event) {
	w.mu.Lock()
	switch {
	case len(w.queue) >= maxQueued:
		w.queue = []Event{{Kind: Rescan, Reason: "too many changes queued"}}
		w.changedAt = map[string]int{}
		w.lastGone = -1
	case e.Kind == Changed:
		// A Changed event already queued after the latest removal covers
		// this one.
		if i, ok := w.changedAt[e.Path]; ok && i > w.lastGone {
			break
		}
		w.changedAt[e.Path] = len(w.queue)
		w.queue = append(w.queue, e)
	default:
		if e.Kind == Removed {
			w.lastGone = len(w.queue)
		}
		w.queue = append(w.queue, e)
	}
	w.mu.Unlock()
	select {
	case w.Ready <- struct{}{}:
	default:
	}
}

func (w *Watcher) report(err error) {
	select {
	case w.Errors <- err:
	default:
	}
}

func (w *Watcher) loop() {
	defer close(w.done)
	for {
		select {
		case <-w.quit:
			return
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if !w.handle(ev) {
				return
			}
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.rescan("the kernel dropped file events")
				continue
			}
			w.report(err)
		}
	}
}

// rescan re-adds watches for the whole tree (folders created while events
// were lost would otherwise stay unwatched) and asks for a full check.
func (w *Watcher) rescan(reason string) {
	if _, err := w.addTree(""); err != nil {
		w.report(err)
	}
	w.send(Event{Kind: Rescan, Reason: reason})
}

// handle processes one event; it returns false once the root is gone.
func (w *Watcher) handle(ev fsnotify.Event) bool {
	r, ok := w.rel(ev.Name)
	if !ok {
		return true
	}
	if r == "" {
		if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
			w.send(Event{Kind: RootGone, Dir: true})
			return false
		}
		return true
	}
	w.mu.Lock()
	wasDir := w.dirs[r]
	w.mu.Unlock()

	switch {
	case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
		if wasDir {
			w.forget(r)
		}
		if w.ignore != nil && w.ignore(r, wasDir) {
			return true
		}
		w.send(Event{Kind: Removed, Path: r, Dir: wasDir})
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) || ev.Has(fsnotify.Chmod):
		st, err := os.Lstat(ev.Name)
		if err != nil {
			return true // already gone; its Remove event follows
		}
		if st.IsDir() {
			if !ev.Has(fsnotify.Create) || (w.ignore != nil && w.ignore(r, true)) {
				return true
			}
			files, err := w.addTree(r)
			if err != nil {
				w.report(err)
				w.send(Event{Kind: Rescan, Reason: fmt.Sprintf("a new folder could not be watched: %v", err)})
			}
			for _, f := range files {
				w.send(Event{Kind: Changed, Path: f})
			}
			return true
		}
		if !st.Mode().IsRegular() || (w.ignore != nil && w.ignore(r, false)) {
			return true
		}
		// Attribute changes (touch, chmod) can change mtime; rclone decides
		// whether anything needs sending.
		w.send(Event{Kind: Changed, Path: r})
	}
	return true
}

// forget drops a removed folder and everything below it from the watch set.
func (w *Watcher) forget(rel string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prefix := rel + "/"
	for d := range w.dirs {
		if d == rel || strings.HasPrefix(d, prefix) {
			delete(w.dirs, d)
			_ = w.fw.Remove(filepath.Join(w.root, filepath.FromSlash(d)))
		}
	}
}
