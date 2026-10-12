// Package verify compares a bucket on two servers version by version: the
// same keys, the same version history in the same order, the same delete
// markers, and the same content, headers, metadata and tags.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/bucketmove/internal/s3util"
	"github.com/minio/minio-go/v7"
)

// Options control a comparison.
type Options struct {
	// LatestOnly compares only the current version of each key (for targets
	// without versioning, where only that was copied).
	LatestOnly bool
	// SkipTags skips object tag comparison (for targets without tags).
	SkipTags bool
	// SkipContent compares sizes instead of reading and hashing content.
	SkipContent bool
	// GuessedType accepts a different Content-Type on the target when the
	// source has application/octet-stream (for servers that guess types).
	GuessedType bool
	// MaxDiffs stops after this many differences (0: 1000).
	MaxDiffs int
	// Progress, when set, is called after each key.
	Progress func(keys int64)
}

// Result is the outcome for one bucket.
type Result struct {
	Bucket   string   `json:"bucket"`
	Keys     int64    `json:"keys_compared"`
	Versions int64    `json:"versions_compared"`
	Diffs    []string `json:"differences"`
	// Truncated is true when MaxDiffs was reached.
	Truncated bool `json:"truncated,omitempty"`
}

// OK reports whether no differences were found.
func (r Result) OK() bool { return len(r.Diffs) == 0 }

// version is what is compared for one version.
type version struct {
	marker  bool
	size    int64
	sha     string
	headers string
	meta    string
	tags    string
}

func (v version) String() string {
	if v.marker {
		return "delete marker"
	}
	s := fmt.Sprintf("%d bytes", v.size)
	if v.sha != "" {
		s += " sha256 " + v.sha[:12]
	}
	return s
}

// group is one key's versions, oldest first.
type group struct {
	key      string
	versions []minio.ObjectInfo
	err      error
}

// errOrder marks a listing that is not sorted by key.
var errOrder = errors.New("listing is not in key order")

// groups turns a version listing (newest first per key) into per-key groups.
func groups(ctx context.Context, c *minio.Client, bucket string) <-chan group {
	out := make(chan group)
	go func() {
		defer close(out)
		objects, _ := s3util.ListVersions(ctx, c, bucket, "", false)
		var cur group
		emit := func() bool {
			if len(cur.versions) == 0 && cur.err == nil {
				return true
			}
			for l, r := 0, len(cur.versions)-1; l < r; l, r = l+1, r-1 {
				cur.versions[l], cur.versions[r] = cur.versions[r], cur.versions[l]
			}
			select {
			case out <- cur:
			case <-ctx.Done():
				return false
			}
			cur = group{}
			return true
		}
		for obj := range objects {
			if obj.Err != nil {
				cur = group{err: obj.Err}
				emit()
				return
			}
			if len(cur.versions) > 0 && obj.Key != cur.key {
				if obj.Key < cur.key {
					cur = group{err: fmt.Errorf("%w (%q after %q)", errOrder, obj.Key, cur.key)}
					emit()
					return
				}
				if !emit() {
					return
				}
			}
			cur.key = obj.Key
			cur.versions = append(cur.versions, obj)
		}
		emit()
	}()
	return out
}

// Bucket compares bucket on src and dst. Both listings are normally walked
// side by side in key order; if the target lists keys out of order (some
// servers do), each source key is looked up on the target instead.
func Bucket(ctx context.Context, src, dst *minio.Client, bucket string, opts Options) (Result, error) {
	if opts.MaxDiffs == 0 {
		opts.MaxDiffs = 1000
	}
	res, err := merge(ctx, src, dst, bucket, opts)
	if errors.Is(err, errTargetOrder) {
		return perKey(ctx, src, dst, bucket, opts)
	}
	return res, err
}

var errTargetOrder = errors.New("target listing out of order")

func merge(ctx context.Context, src, dst *minio.Client, bucket string, opts Options) (Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := Result{Bucket: bucket, Diffs: []string{}}
	add := func(format string, args ...any) bool {
		if len(res.Diffs) >= opts.MaxDiffs {
			res.Truncated = true
			return false
		}
		res.Diffs = append(res.Diffs, fmt.Sprintf(format, args...))
		return true
	}

	a, b := groups(ctx, src, bucket), groups(ctx, dst, bucket)
	ga, okA := <-a
	gb, okB := <-b
	for okA || okB {
		if okA && ga.err != nil {
			return res, fmt.Errorf("listing source: %w", ga.err)
		}
		if okB && gb.err != nil {
			if errors.Is(gb.err, errOrder) {
				return res, errTargetOrder
			}
			return res, fmt.Errorf("listing target: %w", gb.err)
		}
		switch {
		case okA && (!okB || ga.key < gb.key):
			if live(ga, opts) {
				if !add("%s: missing on the target", ga.key) {
					return res, nil
				}
			}
			ga, okA = <-a
		case okB && (!okA || gb.key < ga.key):
			if live(gb, opts) {
				if !add("%s: only on the target", gb.key) {
					return res, nil
				}
			}
			gb, okB = <-b
		default:
			res.Keys++
			diffs, n, err := compareKey(ctx, src, dst, bucket, ga, gb, opts)
			if err != nil {
				return res, fmt.Errorf("%s: %w", ga.key, err)
			}
			res.Versions += n
			for _, d := range diffs {
				if !add("%s: %s", ga.key, d) {
					return res, nil
				}
			}
			if opts.Progress != nil {
				opts.Progress(res.Keys)
			}
			ga, okA = <-a
			gb, okB = <-b
		}
	}
	return res, nil
}

// perKey compares each source key with the target's versions of that key,
// then looks for keys that exist only on the target.
func perKey(ctx context.Context, src, dst *minio.Client, bucket string, opts Options) (Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := Result{Bucket: bucket, Diffs: []string{}}
	add := func(format string, args ...any) bool {
		if len(res.Diffs) >= opts.MaxDiffs {
			res.Truncated = true
			return false
		}
		res.Diffs = append(res.Diffs, fmt.Sprintf(format, args...))
		return true
	}
	var matched int64
	for ga := range groups(ctx, src, bucket) {
		if ga.err != nil {
			return res, fmt.Errorf("listing source: %w", ga.err)
		}
		gb, err := keyGroup(ctx, dst, bucket, ga.key)
		if err != nil {
			return res, fmt.Errorf("listing target: %w", err)
		}
		if len(gb.versions) == 0 {
			if live(ga, opts) && !add("%s: missing on the target", ga.key) {
				return res, nil
			}
			continue
		}
		if live(gb, opts) {
			matched++
		}
		res.Keys++
		diffs, n, err := compareKey(ctx, src, dst, bucket, ga, gb, opts)
		if err != nil {
			return res, fmt.Errorf("%s: %w", ga.key, err)
		}
		res.Versions += n
		for _, d := range diffs {
			if !add("%s: %s", ga.key, d) {
				return res, nil
			}
		}
		if opts.Progress != nil {
			opts.Progress(res.Keys)
		}
	}
	// Keys only on the target: anything live there that the source lacks.
	seen := map[string]bool{}
	objects, _ := s3util.ListVersions(ctx, dst, bucket, "", false)
	for obj := range objects {
		if obj.Err != nil {
			return res, fmt.Errorf("listing target: %w", obj.Err)
		}
		if !obj.IsLatest || obj.IsDeleteMarker || seen[obj.Key] {
			continue
		}
		seen[obj.Key] = true
		ga, err := keyGroup(ctx, src, bucket, obj.Key)
		if err != nil {
			return res, fmt.Errorf("listing source: %w", err)
		}
		if len(ga.versions) == 0 && !add("%s: only on the target", obj.Key) {
			return res, nil
		}
	}
	return res, nil
}

// keyGroup lists one key's versions, oldest first.
func keyGroup(ctx context.Context, c *minio.Client, bucket, key string) (group, error) {
	g := group{key: key}
	objects, _ := s3util.ListVersions(ctx, c, bucket, key, false)
	for obj := range objects {
		if obj.Err != nil {
			return g, obj.Err
		}
		if obj.Key == key {
			g.versions = append(g.versions, obj)
		}
	}
	// Newest first in listings; sort oldest first by modification time,
	// keeping the listing order for equal times.
	sort.SliceStable(g.versions, func(i, j int) bool {
		return g.versions[i].LastModified.After(g.versions[j].LastModified)
	})
	for l, r := 0, len(g.versions)-1; l < r; l, r = l+1, r-1 {
		g.versions[l], g.versions[r] = g.versions[r], g.versions[l]
	}
	return g, nil
}

// live reports whether a key exists as far as the comparison is concerned:
// with LatestOnly, a key whose latest version is a delete marker does not.
func live(g group, opts Options) bool {
	if !opts.LatestOnly {
		return true
	}
	return !g.versions[len(g.versions)-1].IsDeleteMarker
}

func compareKey(ctx context.Context, src, dst *minio.Client, bucket string, ga, gb group, opts Options) ([]string, int64, error) {
	va, vb := ga.versions, gb.versions
	if opts.LatestOnly {
		va, vb = va[len(va)-1:], vb[len(vb)-1:]
		if va[0].IsDeleteMarker || vb[0].IsDeleteMarker {
			if va[0].IsDeleteMarker != vb[0].IsDeleteMarker {
				return []string{"deleted on one side only"}, 1, nil
			}
			return nil, 1, nil
		}
	}
	var diffs []string
	if len(va) != len(vb) {
		diffs = append(diffs, fmt.Sprintf("%d versions on the source, %d on the target", len(va), len(vb)))
	}
	n := len(va)
	if len(vb) < n {
		n = len(vb)
	}
	for i := 0; i < n; i++ {
		x, err := describe(ctx, src, bucket, va[i], opts)
		if err != nil {
			return nil, 0, fmt.Errorf("source version %s: %w", va[i].VersionID, err)
		}
		y, err := describe(ctx, dst, bucket, vb[i], opts)
		if err != nil {
			return nil, 0, fmt.Errorf("target version %s: %w", vb[i].VersionID, err)
		}
		label := fmt.Sprintf("version %d of %d", i+1, len(va))
		switch {
		case x.marker != y.marker || x.size != y.size || x.sha != y.sha:
			diffs = append(diffs, fmt.Sprintf("%s: source has %s, target has %s", label, x, y))
		case x.headers != y.headers && !(opts.GuessedType && guessed(x.headers, y.headers)):
			diffs = append(diffs, fmt.Sprintf("%s: headers differ: source %s, target %s", label, x.headers, y.headers))
		case x.meta != y.meta:
			diffs = append(diffs, fmt.Sprintf("%s: user metadata differs: source %s, target %s", label, x.meta, y.meta))
		case x.tags != y.tags:
			diffs = append(diffs, fmt.Sprintf("%s: tags differ: source %s, target %s", label, x.tags, y.tags))
		}
	}
	return diffs, int64(n), nil
}

var comparedHeaders = []string{"Content-Type", "Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language"}

func describe(ctx context.Context, c *minio.Client, bucket string, o minio.ObjectInfo, opts Options) (version, error) {
	if o.IsDeleteMarker {
		return version{marker: true}, nil
	}
	id := o.VersionID
	v := version{size: o.Size}
	var info minio.ObjectInfo
	if opts.SkipContent {
		var err error
		info, err = c.StatObject(ctx, bucket, o.Key, minio.StatObjectOptions{VersionID: id})
		if err != nil && id == "null" && o.IsLatest {
			info, err = c.StatObject(ctx, bucket, o.Key, minio.StatObjectOptions{})
		}
		if err != nil {
			return v, err
		}
	} else {
		obj, inf, err := s3util.GetVersion(ctx, c, bucket, o)
		if err != nil {
			return v, err
		}
		info = inf
		h := sha256.New()
		n, err := io.Copy(h, obj)
		obj.Close()
		if err != nil {
			return v, err
		}
		v.size, v.sha = n, hex.EncodeToString(h.Sum(nil))
	}
	v.headers = headerString(info.Metadata)
	v.meta = metaString(info.Metadata)
	if !opts.SkipTags {
		t, err := c.GetObjectTagging(ctx, bucket, o.Key, minio.GetObjectTaggingOptions{VersionID: id})
		if err != nil && !s3util.NotConfigured(err) {
			return v, fmt.Errorf("reading tags: %w", err)
		}
		if err == nil && t != nil {
			v.tags = mapString(t.ToMap())
		}
	}
	return v, nil
}

// guessed reports whether two header strings differ only because the target
// replaced application/octet-stream with another Content-Type.
func guessed(src, dst string) bool {
	const generic = "Content-Type=application/octet-stream"
	if !strings.HasPrefix(src, generic) {
		return false
	}
	rest := strings.TrimPrefix(src, generic)
	i := strings.Index(dst, ";")
	if i < 0 {
		return rest == "" && strings.HasPrefix(dst, "Content-Type=")
	}
	// Keep everything after the Content-Type part; the guessed type itself
	// may contain "; charset=...".
	return strings.HasPrefix(dst, "Content-Type=") && strings.HasSuffix(dst, rest)
}

func headerString(h http.Header) string {
	var parts []string
	for _, k := range comparedHeaders {
		val := h.Get(k)
		if k == "Content-Type" && val == "" {
			val = "binary/octet-stream"
		}
		if val != "" {
			parts = append(parts, k+"="+val)
		}
	}
	return strings.Join(parts, "; ")
}

func metaString(h http.Header) string {
	m := map[string]string{}
	for k, vals := range h {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && len(vals) > 0 {
			m[name] = vals[0]
		}
	}
	return mapString(m)
}

func mapString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
