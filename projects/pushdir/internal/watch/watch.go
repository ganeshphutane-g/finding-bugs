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
	// Overflow: the kernel dropped events; only a full rescan is reliable.
	Overflow
)

func (k Kind) String() string {
	switch k {
	case Changed:
		return "changed"
	case Removed:
		return "removed"
	}
	return "overflow"
}

// Event is one change. Path is relative to the watched root, with forward
// slashes. Dir is true when the path is (or was) a known folder.
type Event struct {
	Kind Kind
	Path string
	Dir  bool
}

// Watcher watches a folder tree.
type Watcher struct {
	root   string
	ignore func(rel string, dir bool) bool
	fw     *fsnotify.Watcher

	mu   sync.Mutex
	dirs map[string]bool // watched folders, relative

	Events chan Event
	Errors chan error
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
		return nil, err
	}
	w := &Watcher{
		root:   abs,
		ignore: ignore,
		fw:     fw,
		dirs:   map[string]bool{},
		Events: make(chan Event, 1024),
		Errors: make(chan error, 16),
		done:   make(chan struct{}),
	}
	if _, err := w.addTree(""); err != nil {
		fw.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// Close stops watching and closes the channels.
func (w *Watcher) Close() error {
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

func (w *Watcher) send(e Event) {
	w.Events <- e
}

func (w *Watcher) loop() {
	defer close(w.done)
	defer close(w.Events)
	defer close(w.Errors)
	for {
		select {
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				w.send(Event{Kind: Overflow})
				continue
			}
			select {
			case w.Errors <- err:
			default:
			}
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	r, ok := w.rel(ev.Name)
	if !ok || r == "" {
		if ok && r == "" && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
			// The root itself went away.
			w.send(Event{Kind: Removed, Path: "", Dir: true})
		}
		return
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
			return
		}
		w.send(Event{Kind: Removed, Path: r, Dir: wasDir})
	case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) || ev.Has(fsnotify.Chmod):
		st, err := os.Lstat(ev.Name)
		if err != nil {
			return // already gone; its Remove event follows
		}
		if st.IsDir() {
			if !ev.Has(fsnotify.Create) || (w.ignore != nil && w.ignore(r, true)) {
				return
			}
			files, err := w.addTree(r)
			if err != nil {
				select {
				case w.Errors <- err:
				default:
				}
			}
			for _, f := range files {
				w.send(Event{Kind: Changed, Path: f})
			}
			return
		}
		if !st.Mode().IsRegular() || (w.ignore != nil && w.ignore(r, false)) {
			return
		}
		if ev.Has(fsnotify.Chmod) && !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
			// Attribute changes (touch, chmod) can change mtime; rclone
			// decides whether anything needs sending.
		}
		w.send(Event{Kind: Changed, Path: r})
	}
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
