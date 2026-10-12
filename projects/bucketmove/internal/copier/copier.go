// Package copier copies buckets between S3 servers with their full version
// history, settings, metadata, tags and locks.
package copier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/s3util"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// Options control a copy.
type Options struct {
	// StateDir holds the per-bucket journals.
	StateDir string
	// Workers is how many keys are copied at once (versions of one key are
	// always copied one after another, oldest first).
	Workers int
	// IntoExisting allows copying into a target bucket that already exists
	// and was not created by an earlier run of this copy.
	IntoExisting bool
	// Progress, when set, receives one call per copied version.
	Progress func(bucket string, done int64, bytes int64)
	// Warn, when set, receives warnings.
	Warn func(string)
}

// Stats summarises one bucket's copy.
type Stats struct {
	Bucket   string   `json:"bucket"`
	Versions int64    `json:"versions_copied"`
	Markers  int64    `json:"delete_markers_copied"`
	Skipped  int64    `json:"already_copied"`
	Bytes    int64    `json:"bytes_copied"`
	Settings []string `json:"settings_copied"`
}

// Copier copies from one server to another.
type Copier struct {
	Src, Dst *minio.Client
	Caps     probe.Capabilities
	Opts     Options

	// beforeWrite, when set (tests only), runs after a version is recorded
	// as started and before it is written; an error simulates a crash.
	beforeWrite func(Entry) error
}

func (c *Copier) warn(format string, args ...any) {
	if c.Opts.Warn != nil {
		c.Opts.Warn(fmt.Sprintf(format, args...))
	}
}

// Bucket copies one bucket. b is its inventory entry on the source.
func (c *Copier) Bucket(ctx context.Context, b inventory.Bucket) (Stats, error) {
	st := Stats{Bucket: b.Name}
	j, err := OpenJournal(c.Opts.StateDir, b.Name)
	if err != nil {
		return st, fmt.Errorf("opening journal: %w", err)
	}
	defer j.Close()

	lockOK, err := c.prepareBucket(ctx, b, j)
	if err != nil {
		return st, err
	}
	// Version history only goes where both versions and delete markers
	// work; otherwise only the latest version of each key is copied.
	versioned := b.Versioning != "" && c.Caps.Supports(probe.Versioning) && c.Caps.Supports(probe.DeleteMarker)

	if err := c.copyObjects(ctx, b, j, versioned, lockOK, &st); err != nil {
		return st, err
	}
	settings, err := c.copySettings(ctx, b, lockOK)
	st.Settings = settings
	return st, err
}

// prepareBucket creates the target bucket (with object lock if needed),
// turns on versioning, and reports whether retention and legal holds can be
// set on the target bucket.
func (c *Copier) prepareBucket(ctx context.Context, b inventory.Bucket, j *Journal) (lockOK bool, err error) {
	exists, err := c.Dst.BucketExists(ctx, b.Name)
	if err != nil {
		return false, fmt.Errorf("checking target bucket: %w", err)
	}
	lock := b.ObjectLock && c.Caps.Supports(probe.ObjectLock)
	lockOK = lock
	switch {
	case !exists:
		if err := c.Dst.MakeBucket(ctx, b.Name, minio.MakeBucketOptions{ObjectLocking: lock}); err != nil {
			return false, fmt.Errorf("creating target bucket: %w", err)
		}
		if err := j.Record(Entry{State: "bucket-created"}); err != nil {
			return false, err
		}
	case j.DoneCount() == 0 && !j.CreatedBucket() && !c.Opts.IntoExisting:
		return false, fmt.Errorf("the target already has a bucket named %s that this copy did not create; pass --into-existing to copy into it anyway", b.Name)
	case lock:
		if l, _, _, _, err := c.Dst.GetObjectLockConfig(ctx, b.Name); err != nil || l != "Enabled" {
			c.warn("%s: the existing target bucket has no object lock, so retention and legal holds are not copied (object lock can only be chosen when a bucket is created)", b.Name)
			lockOK = false
		}
	}
	if b.Versioning != "" && c.Caps.Supports(probe.Versioning) {
		if err := c.Dst.EnableVersioning(ctx, b.Name); err != nil {
			return false, fmt.Errorf("turning on versioning: %w", err)
		}
	}
	// A default retention set by an earlier run would give newly copied
	// versions a retention their source versions do not have. Lift it while
	// copying; copySettings puts it back at the end.
	if lockOK && exists {
		_, mode, _, _, err := c.Dst.GetObjectLockConfig(ctx, b.Name)
		if err == nil && mode != nil {
			if err := c.Dst.SetObjectLockConfig(ctx, b.Name, nil, nil, nil); err != nil {
				return false, fmt.Errorf("lifting the target's default retention while copying: %w", err)
			}
			c.warn("%s: the target's default retention is lifted while copying and set again at the end", b.Name)
		}
	}
	return lockOK, nil
}

// keyVersions is every version of one key, oldest first.
type keyVersions struct {
	key      string
	versions []minio.ObjectInfo
}

func (c *Copier) copyObjects(ctx context.Context, b inventory.Bucket, j *Journal, versioned, lockOK bool, st *Stats) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := c.Opts.Workers
	if workers < 1 {
		workers = 4
	}
	jobs := make(chan keyVersions)
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for kv := range jobs {
				if err := c.copyKey(ctx, b, j, versioned, lockOK, kv, st, &mu); err != nil {
					fail(fmt.Errorf("%s: %w", kv.key, err))
				}
			}
		}()
	}

	// Listings come grouped by key, newest version first.
	objects, _ := s3util.ListVersions(ctx, c.Src, b.Name, "", true)
	var cur keyVersions
	flush := func() bool {
		if cur.key == "" && len(cur.versions) == 0 {
			return true
		}
		s3util.NewestFirst(cur.versions)
		for l, r := 0, len(cur.versions)-1; l < r; l, r = l+1, r-1 {
			cur.versions[l], cur.versions[r] = cur.versions[r], cur.versions[l]
		}
		select {
		case jobs <- cur:
		case <-ctx.Done():
			return false
		}
		cur = keyVersions{}
		return true
	}
	listOK := true
	for obj := range objects {
		if obj.Err != nil {
			fail(fmt.Errorf("listing source: %w", obj.Err))
			listOK = false
			break
		}
		if len(cur.versions) > 0 && obj.Key < cur.key {
			// Versions of one key must be copied together, oldest first.
			fail(fmt.Errorf("the source lists keys out of order (%q after %q); bucketmove cannot copy version history from it safely", obj.Key, cur.key))
			listOK = false
			break
		}
		if len(cur.versions) > 0 && obj.Key != cur.key {
			if !flush() {
				listOK = false
				break
			}
		}
		cur.key = obj.Key
		cur.versions = append(cur.versions, obj)
	}
	if listOK {
		flush()
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (c *Copier) copyKey(ctx context.Context, b inventory.Bucket, j *Journal, versioned, lockOK bool, kv keyVersions, st *Stats, mu *sync.Mutex) error {
	versions := kv.versions
	if !versioned {
		// The target keeps one version: copy only the latest, and nothing
		// when the key is deleted.
		latest := versions[len(versions)-1]
		if latest.IsDeleteMarker {
			return nil
		}
		versions = []minio.ObjectInfo{latest}
	}
	for _, v := range versions {
		e := Entry{Key: v.Key, SrcVersion: v.VersionID, SrcETag: v.ETag, Size: v.Size, Marker: v.IsDeleteMarker}
		if _, ok := j.Done(e); ok {
			mu.Lock()
			st.Skipped++
			mu.Unlock()
			continue
		}
		if s, ok := j.Started(e); ok {
			adopted, err := c.adoptOrphan(ctx, b.Name, s, j)
			if err != nil {
				return err
			}
			if adopted != "" {
				e.State, e.DstVersion = "done", adopted
				if err := j.Record(e); err != nil {
					return err
				}
				continue
			}
		}
		e.State = "started"
		if err := j.Record(e); err != nil {
			return err
		}
		if c.beforeWrite != nil {
			if err := c.beforeWrite(e); err != nil {
				return err
			}
		}

		var err error
		if v.IsDeleteMarker {
			e.DstVersion, err = c.copyMarker(ctx, b.Name, v.Key)
		} else {
			e.DstVersion, e.SHA256, err = c.copyVersion(ctx, b, v, lockOK, j)
		}
		if err != nil {
			return err
		}
		e.State = "done"
		if err := j.Record(e); err != nil {
			return err
		}
		mu.Lock()
		if v.IsDeleteMarker {
			st.Markers++
		} else {
			st.Versions++
			st.Bytes += v.Size
		}
		done, bytes := st.Versions+st.Markers, st.Bytes
		mu.Unlock()
		if c.Opts.Progress != nil {
			c.Opts.Progress(b.Name, done, bytes)
		}
	}
	return nil
}

// adoptOrphan handles a version an earlier run started but did not record
// as done. The target's newest version of the key is that upload only if it
// is not a version already recorded, was written after the start, and
// matches the source version (type, size and, where both are plain MD5
// ETags, the ETag). Otherwise nothing is adopted and the version is copied
// again; at worst that leaves an extra version, never a missing one.
func (c *Copier) adoptOrphan(ctx context.Context, bucket string, started Entry, j *Journal) (string, error) {
	objects, _ := s3util.ListVersions(ctx, c.Dst, bucket, started.Key, false)
	var newest *minio.ObjectInfo
	for obj := range objects {
		if obj.Err != nil {
			return "", fmt.Errorf("listing target: %w", obj.Err)
		}
		if obj.Key == started.Key && (newest == nil || obj.IsLatest) {
			o := obj
			newest = &o
		}
	}
	switch {
	case newest == nil:
		return "", nil
	case started.Marker != newest.IsDeleteMarker:
		return "", nil
	case j.KnownDst(started.Key, newest.VersionID):
		return "", nil
	case newest.LastModified.Before(started.Time.Add(-2 * time.Minute)):
		return "", nil
	}
	if started.Marker {
		// A marker carries no content to compare; a new, unrecorded marker
		// written after the start is ours.
		return newest.VersionID, nil
	}
	if newest.Size != started.Size {
		return "", nil
	}
	a, b := strings.Trim(started.SrcETag, `"`), strings.Trim(newest.ETag, `"`)
	if plainMD5(a) && plainMD5(b) && a != b {
		return "", nil
	}
	if newest.VersionID == "" {
		// Unversioned targets have no version to adopt; rewriting the one
		// object is harmless.
		return "", nil
	}
	return newest.VersionID, nil
}

func plainMD5(etag string) bool {
	if len(etag) != 32 {
		return false
	}
	_, err := hex.DecodeString(etag)
	return err == nil
}

func (c *Copier) copyMarker(ctx context.Context, bucket, key string) (string, error) {
	// On a versioned bucket a plain delete adds a delete marker.
	if err := c.Dst.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return "", fmt.Errorf("adding delete marker: %w", err)
	}
	// Record the marker's version ID so a later resume knows it is ours.
	objects, _ := s3util.ListVersions(ctx, c.Dst, bucket, key, false)
	id := ""
	for obj := range objects {
		if obj.Err == nil && obj.Key == key && obj.IsLatest && obj.IsDeleteMarker {
			id = obj.VersionID
		}
	}
	return id, nil
}

// copyVersion copies one version, retrying temporary failures. The upload
// streams straight from the source, so each attempt reads the source again.
func (c *Copier) copyVersion(ctx context.Context, b inventory.Bucket, v minio.ObjectInfo, lockOK bool, j *Journal) (dstVersion, sum string, err error) {
	delay := time.Second
	for attempt := 1; ; attempt++ {
		start := time.Now()
		dstVersion, sum, err = c.copyVersionOnce(ctx, b, v, lockOK)
		if err == nil || attempt == 4 || !s3util.Transient(err) || ctx.Err() != nil {
			return dstVersion, sum, err
		}
		// The server may have stored the object and only the reply was
		// lost; uploading again would add a duplicate version.
		if id, aerr := c.adoptOrphan(ctx, b.Name, Entry{Key: v.Key, SrcETag: v.ETag, Size: v.Size, Time: start}, j); aerr == nil && id != "" {
			return id, "", nil
		}
		c.warn("%s/%s: %v; retrying in %s", b.Name, v.Key, err, delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
		delay *= 2
	}
}

func (c *Copier) copyVersionOnce(ctx context.Context, b inventory.Bucket, v minio.ObjectInfo, lockOK bool) (dstVersion, sum string, err error) {
	versionID := v.VersionID
	obj, info, err := s3util.GetVersion(ctx, c.Src, b.Name, v)
	if err != nil {
		return "", "", fmt.Errorf("reading source: %w", err)
	}
	defer obj.Close()

	opts := minio.PutObjectOptions{
		ContentType:        info.ContentType,
		CacheControl:       info.Metadata.Get("Cache-Control"),
		ContentDisposition: info.Metadata.Get("Content-Disposition"),
		ContentEncoding:    info.Metadata.Get("Content-Encoding"),
		ContentLanguage:    info.Metadata.Get("Content-Language"),
		UserMetadata:       userMetadata(info.Metadata),
		PartSize:           partSize(info.Size),
		StorageClass:       storageClass(info, c.Caps),
		// A plain body with a Content-MD5 the server checks. Streaming
		// signatures (aws-chunked) are avoided: some servers store
		// "aws-chunked" as the object's Content-Encoding (SeaweedFS does).
		DisableContentSha256: true,
		SendContentMd5:       true,
	}
	if exp := info.Metadata.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			opts.Expires = t
		}
	}
	if c.Caps.Supports(probe.ObjectTags) && tagCount(info) > 0 {
		t, err := c.Src.GetObjectTagging(ctx, b.Name, v.Key, minio.GetObjectTaggingOptions{VersionID: versionID})
		if err != nil {
			return "", "", fmt.Errorf("reading tags: %w", err)
		}
		opts.UserTags = t.ToMap()
	}
	if lockOK {
		if c.Caps.Supports(probe.Retention) {
			mode, until, err := c.Src.GetObjectRetention(ctx, b.Name, v.Key, versionID)
			if err != nil && !s3util.NotConfigured(err) {
				return "", "", fmt.Errorf("reading retention: %w", err)
			}
			if err == nil && mode != nil && until != nil && until.After(time.Now()) {
				opts.Mode, opts.RetainUntilDate = *mode, *until
			}
		}
		if c.Caps.Supports(probe.LegalHold) {
			hold, err := c.Src.GetObjectLegalHold(ctx, b.Name, v.Key, minio.GetObjectLegalHoldOptions{VersionID: versionID})
			if err != nil && !s3util.NotConfigured(err) {
				return "", "", fmt.Errorf("reading legal hold: %w", err)
			}
			if err == nil && hold != nil && *hold == minio.LegalHoldEnabled {
				opts.LegalHold = minio.LegalHoldEnabled
			}
		}
	}

	h := sha256.New()
	body := io.TeeReader(obj, h)
	up, err := c.Dst.PutObject(ctx, b.Name, v.Key, body, info.Size, opts)
	if err != nil {
		return "", "", fmt.Errorf("writing target: %w", err)
	}
	if up.Size != info.Size {
		return "", "", fmt.Errorf("target stored %d bytes, source has %d", up.Size, info.Size)
	}
	return up.VersionID, hex.EncodeToString(h.Sum(nil)), nil
}

// storageClass keeps a non-standard storage class where the target keeps them.
func storageClass(info minio.ObjectInfo, caps probe.Capabilities) string {
	if info.StorageClass == "" || info.StorageClass == "STANDARD" || !caps.Supports(probe.StorageClass) {
		return ""
	}
	return info.StorageClass
}

// partSize keeps multipart uploads within S3's 10,000-part limit, with
// 16 MiB parts for most objects so memory use stays small.
func partSize(size int64) uint64 {
	const min = 16 << 20
	p := int64(min)
	if size/9000 > p {
		p = (size/9000 + (1 << 20)) &^ ((1 << 20) - 1)
	}
	return uint64(p)
}

// userMetadata returns x-amz-meta-* headers without their prefix.
func userMetadata(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vals := range h {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && len(vals) > 0 {
			out[name] = vals[0]
		}
	}
	return out
}

func tagCount(info minio.ObjectInfo) int {
	if info.UserTagCount > 0 {
		return info.UserTagCount
	}
	if v := info.Metadata.Get("X-Amz-Tagging-Count"); v != "" && v != "0" {
		return 1
	}
	return 0
}

// copySettings copies the bucket settings the target supports, after the
// objects so that lifecycle rules do not act on a half-copied bucket.
func (c *Copier) copySettings(ctx context.Context, b inventory.Bucket, lockOK bool) ([]string, error) {
	var done []string
	name := b.Name
	if b.LifecycleRules > 0 && c.Caps.Supports(probe.Lifecycle) {
		lc, err := c.Src.GetBucketLifecycle(ctx, name)
		if err != nil {
			return done, fmt.Errorf("reading lifecycle: %w", err)
		}
		if err := c.Dst.SetBucketLifecycle(ctx, name, lc); err != nil {
			return done, fmt.Errorf("setting lifecycle: %w", err)
		}
		done = append(done, "lifecycle")
	}
	if b.Policy && c.Caps.Supports(probe.Policy) {
		p, err := c.Src.GetBucketPolicy(ctx, name)
		if err != nil {
			return done, fmt.Errorf("reading policy: %w", err)
		}
		if err := c.Dst.SetBucketPolicy(ctx, name, p); err != nil {
			return done, fmt.Errorf("setting policy: %w", err)
		}
		done = append(done, "policy")
	}
	if b.CORSRules > 0 && c.Caps.Supports(probe.CORS) {
		cfg, err := c.Src.GetBucketCors(ctx, name)
		if err != nil {
			return done, fmt.Errorf("reading CORS: %w", err)
		}
		if err := c.Dst.SetBucketCors(ctx, name, cfg); err != nil {
			return done, fmt.Errorf("setting CORS: %w", err)
		}
		done = append(done, "CORS")
	}
	if len(b.Tags) > 0 && c.Caps.Supports(probe.BucketTags) {
		t, err := tags.NewTags(b.Tags, false)
		if err != nil {
			return done, err
		}
		if err := c.Dst.SetBucketTagging(ctx, name, t); err != nil {
			return done, fmt.Errorf("setting bucket tags: %w", err)
		}
		done = append(done, "bucket tags")
	}
	// The default retention goes on last, so copied versions keep their own
	// retention instead of getting a fresh default one.
	if lockOK && b.LockDefault != "" {
		_, mode, validity, unit, err := c.Src.GetObjectLockConfig(ctx, name)
		if err != nil {
			return done, fmt.Errorf("reading object lock: %w", err)
		}
		if err := c.Dst.SetObjectLockConfig(ctx, name, mode, validity, unit); err != nil {
			return done, fmt.Errorf("setting default retention: %w", err)
		}
		done = append(done, "default retention")
	}
	if b.Versioning == "Suspended" && c.Caps.Supports(probe.Versioning) {
		if err := c.Dst.SuspendVersioning(ctx, name); err != nil {
			return done, fmt.Errorf("suspending versioning: %w", err)
		}
		done = append(done, "versioning suspended")
	}
	return done, nil
}
