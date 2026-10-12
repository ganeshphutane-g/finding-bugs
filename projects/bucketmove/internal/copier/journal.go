package copier

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one line of the journal. A version is first recorded as
// "started" and then as "done" with its new version ID, so a run that stops
// between the two can recognise its own upload on the next run.
type Entry struct {
	Key        string    `json:"key"`
	SrcVersion string    `json:"src_version"` // source version ID ("" or "null" when unversioned)
	SrcETag    string    `json:"src_etag"`
	Size       int64     `json:"size"`
	State      string    `json:"state"`                 // "started" or "done"
	DstVersion string    `json:"dst_version,omitempty"` // version ID on the target
	SHA256     string    `json:"sha256,omitempty"`      // content hash, for verify
	Marker     bool      `json:"delete_marker,omitempty"`
	Time       time.Time `json:"time"`
}

// id identifies a source version. ETag and size are included because an
// unversioned object keeps the same ("null") version ID when overwritten.
func (e Entry) id() string {
	return e.Key + "\x00" + e.SrcVersion + "\x00" + e.SrcETag + "\x00" + fmt.Sprint(e.Size)
}

// Journal records copied versions for one bucket, one JSON object per line.
// It doubles as the map from old to new version IDs.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	done    map[string]Entry
	started map[string]Entry
}

// OpenJournal opens (or creates) the journal for a bucket in dir.
func OpenJournal(dir, bucket string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, bucket+".jsonl")
	j := &Journal{done: map[string]Entry{}, started: map[string]Entry{}}
	if data, err := os.ReadFile(path); err == nil {
		var lines []string
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		for i, line := range lines {
			var e Entry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				if i == len(lines)-1 {
					break // the last line was cut off by a crash; it is redone
				}
				return nil, fmt.Errorf("journal %s line %d is damaged: %w", path, i+1, err)
			}
			switch e.State {
			case "done":
				j.done[e.id()] = e
				delete(j.started, e.id())
			case "started":
				j.started[e.id()] = e
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	j.f, j.w = f, bufio.NewWriter(f)
	// Start on a fresh line if the last one was cut off.
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if r, err := os.Open(path); err == nil {
			if _, err := r.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
				_, _ = j.w.WriteString("\n")
			}
			r.Close()
		}
	}
	return j, nil
}

// Done returns the finished entry for a source version, if any.
func (j *Journal) Done(e Entry) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	d, ok := j.done[e.id()]
	return d, ok
}

// Started returns an entry that was started but never finished.
func (j *Journal) Started(e Entry) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s, ok := j.started[e.id()]
	return s, ok
}

// DoneCount is the number of finished versions.
func (j *Journal) DoneCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.done)
}

// Record appends an entry and flushes it to disk.
func (j *Journal) Record(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.w.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	if e.State == "done" {
		if err := j.f.Sync(); err != nil {
			return err
		}
		j.done[e.id()] = e
		delete(j.started, e.id())
	} else {
		j.started[e.id()] = e
	}
	return nil
}

// Close flushes and closes the journal.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.w.Flush(); err != nil {
		j.f.Close()
		return err
	}
	return j.f.Close()
}
