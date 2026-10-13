package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// waitEvents collects events until cond holds.
func waitEvents(t *testing.T, w *Watcher, cond func([]Event) bool) []Event {
	t.Helper()
	var all []Event
	deadline := time.After(10 * time.Second)
	for !cond(all) {
		select {
		case <-w.Ready:
			all = append(all, w.Take()...)
		case <-deadline:
			t.Fatalf("timed out; events so far: %+v", all)
		}
	}
	return all
}

func has(evs []Event, k Kind, path string) bool {
	for _, e := range evs {
		if e.Kind == k && e.Path == path {
			return true
		}
	}
	return false
}

func TestCloseDoesNotHangWithManyQueuedEvents(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, nil)
	must(t, err)
	for i := 0; i < 3000; i++ {
		must(t, os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0o644))
	}
	time.Sleep(200 * time.Millisecond) // nobody takes the events
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}

func TestRescanWatchesFoldersItMissed(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, nil)
	must(t, err)
	defer w.Close()
	must(t, os.MkdirAll(filepath.Join(dir, "missed"), 0o755))
	for i := 0; w.Dirs() != 2; i++ {
		if i > 200 {
			t.Fatal("the new folder was not watched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Pretend the folder's creation was lost in an overflow.
	w.forget("missed")
	w.rescan("test")
	if w.Dirs() != 2 {
		t.Fatalf("watching %d folders after rescan, want 2", w.Dirs())
	}
	must(t, os.WriteFile(filepath.Join(dir, "missed", "f.txt"), []byte("x"), 0o644))
	waitEvents(t, w, func(e []Event) bool { return has(e, Changed, "missed/f.txt") })
}

func TestRepeatedWritesAreCoalescedInOrder(t *testing.T) {
	w := &Watcher{changedAt: map[string]int{}, lastGone: -1, Ready: make(chan struct{}, 1)}
	w.send(Event{Kind: Changed, Path: "a/x"})
	w.send(Event{Kind: Changed, Path: "a/x"})
	w.send(Event{Kind: Removed, Path: "a", Dir: true})
	w.send(Event{Kind: Changed, Path: "a/x"}) // after the removal: must stay
	w.send(Event{Kind: Changed, Path: "a/x"})
	got := w.Take()
	want := []Kind{Changed, Removed, Changed}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Fatalf("got %+v", got)
		}
	}
}

func TestRootGone(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	must(t, os.MkdirAll(dir, 0o755))
	w, err := New(dir, nil)
	must(t, err)
	defer w.Close()
	must(t, os.Rename(dir, dir+".old"))
	waitEvents(t, w, func(e []Event) bool { return has(e, RootGone, "") })
}
