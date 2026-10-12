package copier

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	State      string    `json:"state"`                 // "started", "done" or "bucket-created"
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
	// dstByKey holds, per key, the target version IDs already recorded as
	// done, so an earlier version is never mistaken for a lost upload.
	dstByKey map[string]map[string]bool
	// createdBucket is true once this copy has created the target bucket.
	createdBucket bool
}

// OpenJournal opens (or creates) the journal for a bucket in dir.
func OpenJournal(dir, bucket string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, bucket+".jsonl")
	j := &Journal{done: map[string]Entry{}, started: map[string]Entry{}, dstByKey: map[string]map[string]bool{}}
	if err := j.load(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	j.f, j.w = f, bufio.NewWriter(f)
	return j, nil
}

// load reads an existing journal line by line. A last line cut off by a
// crash is removed from the file, so new entries never follow a damaged one;
// damage anywhere else is reported.
func (j *Journal) load(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var offset, good int64
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			return err
		}
		offset += int64(len(line))
		text := strings.TrimSpace(string(line))
		if text == "" {
			good = offset
			continue
		}
		var e Entry
		if jerr := json.Unmarshal([]byte(text), &e); jerr != nil || line[len(line)-1] != '\n' {
			if err == io.EOF {
				// The cut-off last line: drop it; that version is redone.
				return os.Truncate(path, good)
			}
			return fmt.Errorf("journal %s line %d is damaged: %v", path, n, jerr)
		}
		good = offset
		switch e.State {
		case "done":
			j.markDone(e)
		case "started":
			j.started[e.id()] = e
		case "bucket-created":
			j.createdBucket = true
		}
		if err == io.EOF {
			break
		}
	}
	return nil
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
	switch e.State {
	case "done", "bucket-created":
		if err := j.f.Sync(); err != nil {
			return err
		}
		if e.State == "done" {
			j.markDone(e)
		} else {
			j.createdBucket = true
		}
	default:
		j.started[e.id()] = e
	}
	return nil
}

func (j *Journal) markDone(e Entry) {
	j.done[e.id()] = e
	delete(j.started, e.id())
	if e.DstVersion != "" {
		if j.dstByKey[e.Key] == nil {
			j.dstByKey[e.Key] = map[string]bool{}
		}
		j.dstByKey[e.Key][e.DstVersion] = true
	}
}

// CreatedBucket reports whether this copy created the target bucket.
func (j *Journal) CreatedBucket() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.createdBucket
}

// KnownDst reports whether a target version of key is already recorded.
func (j *Journal) KnownDst(key, dstVersion string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dstByKey[key][dstVersion]
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
