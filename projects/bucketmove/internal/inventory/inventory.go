// Package inventory records what an S3 server's buckets contain and which
// features they use, so nothing is overlooked in a migration.
package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/s3util"
	"github.com/minio/minio-go/v7"
)

// Feature state values.
const (
	Off         = ""
	Unsupported = "unsupported" // the server does not implement the setting
)

// Bucket is what one bucket holds and uses.
type Bucket struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`

	// Settings.
	Versioning     string            `json:"versioning"`             // "Enabled", "Suspended" or ""
	ObjectLock     bool              `json:"object_lock"`            // bucket created with object lock
	LockDefault    string            `json:"lock_default,omitempty"` // e.g. "GOVERNANCE 30 days"
	LifecycleRules int               `json:"lifecycle_rules"`        // number of rules
	Policy         bool              `json:"policy"`                 // has a bucket policy
	CORSRules      int               `json:"cors_rules"`             // number of CORS rules
	Tags           map[string]string `json:"tags,omitempty"`         // bucket tags
	Encryption     string            `json:"encryption,omitempty"`   // default encryption algorithm
	Notifications  int               `json:"notifications"`          // notification targets
	Replication    int               `json:"replication_rules"`      // replication rules
	Unsupported    []string          `json:"unsupported,omitempty"`  // settings the server could not report

	// Contents.
	Objects        int64 `json:"objects"`         // current (latest, not deleted) objects
	OldVersions    int64 `json:"old_versions"`    // noncurrent versions
	DeleteMarkers  int64 `json:"delete_markers"`  // delete markers (latest or not)
	CurrentBytes   int64 `json:"current_bytes"`   // size of current objects
	TotalBytes     int64 `json:"total_bytes"`     // size of all versions
	TaggedVersions int64 `json:"tagged_versions"` // versions with object tags
	Retained       int64 `json:"retained"`        // versions under retention
	LegalHolds     int64 `json:"legal_holds"`     // versions under legal hold
	Metadata       int64 `json:"with_metadata"`   // versions with user metadata or extra headers
	// StorageClasses counts versions per storage class other than STANDARD.
	StorageClasses map[string]int64 `json:"storage_classes,omitempty"`
	// NoVersionListing is true when the server could not list old versions,
	// so OldVersions and DeleteMarkers may be too low.
	NoVersionListing bool `json:"no_version_listing,omitempty"`
}

// Options control how deep the inventory looks.
type Options struct {
	// Buckets limits the inventory to these buckets (all when empty).
	Buckets []string
	// Progress, when set, is called while objects are counted.
	Progress func(bucket string, versions int64)
}

// Take inventories the server behind c.
func Take(ctx context.Context, c *minio.Client, opts Options) ([]Bucket, error) {
	list, err := c.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing buckets: %w", err)
	}
	want := map[string]bool{}
	for _, b := range opts.Buckets {
		want[b] = true
	}
	var out []Bucket
	found := map[string]bool{}
	for _, info := range list {
		if len(want) > 0 && !want[info.Name] {
			continue
		}
		found[info.Name] = true
		b, err := bucket(ctx, c, info, opts)
		if err != nil {
			return out, fmt.Errorf("bucket %s: %w", info.Name, err)
		}
		out = append(out, b)
	}
	for _, name := range opts.Buckets {
		if !found[name] {
			return out, fmt.Errorf("bucket %q does not exist", name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func bucket(ctx context.Context, c *minio.Client, info minio.BucketInfo, opts Options) (Bucket, error) {
	b := Bucket{Name: info.Name, Created: info.CreationDate}
	name := info.Name

	// check records a setting the server cannot report instead of failing.
	check := func(setting string, err error) error {
		switch {
		case err == nil || s3util.NotConfigured(err):
			return nil
		// Garage answers NoSuchBucket for an existing bucket without CORS
		// or lifecycle rules; the bucket was just listed, so it exists.
		case s3util.Code(err) == "NoSuchBucket" && (setting == "CORS" || setting == "lifecycle"):
			return nil
		case s3util.NotSupported(err):
			b.Unsupported = append(b.Unsupported, setting)
			return nil
		}
		return fmt.Errorf("%s: %w", setting, err)
	}

	v, err := c.GetBucketVersioning(ctx, name)
	if err := check("versioning", err); err != nil {
		return b, err
	}
	b.Versioning = v.Status

	lock, mode, validity, unit, err := c.GetObjectLockConfig(ctx, name)
	if err := check("object lock", err); err != nil {
		return b, err
	}
	if lock == "Enabled" {
		b.ObjectLock = true
		if mode != nil && validity != nil && unit != nil {
			b.LockDefault = fmt.Sprintf("%s %d %s", *mode, *validity, strings.ToLower(unit.String()))
		}
	}

	lc, err := c.GetBucketLifecycle(ctx, name)
	if err := check("lifecycle", err); err != nil {
		return b, err
	}
	if err == nil && lc != nil {
		b.LifecycleRules = len(lc.Rules)
	}

	policy, err := c.GetBucketPolicy(ctx, name)
	if err := check("policy", err); err != nil {
		return b, err
	}
	b.Policy = strings.TrimSpace(policy) != ""

	cors, err := c.GetBucketCors(ctx, name)
	if err := check("CORS", err); err != nil {
		return b, err
	}
	if err == nil && cors != nil {
		b.CORSRules = len(cors.CORSRules)
	}

	t, err := c.GetBucketTagging(ctx, name)
	if err := check("bucket tags", err); err != nil {
		return b, err
	}
	if err == nil && t != nil && len(t.ToMap()) > 0 {
		b.Tags = t.ToMap()
	}

	enc, err := c.GetBucketEncryption(ctx, name)
	if err := check("encryption", err); err != nil {
		return b, err
	}
	if err == nil && enc != nil && len(enc.Rules) > 0 {
		b.Encryption = enc.Rules[0].Apply.SSEAlgorithm
	}

	n, err := c.GetBucketNotification(ctx, name)
	if err := check("notifications", err); err != nil {
		return b, err
	}
	if err == nil {
		b.Notifications = len(n.LambdaConfigs) + len(n.TopicConfigs) + len(n.QueueConfigs)
	}

	r, err := c.GetBucketReplication(ctx, name)
	if err := check("replication", err); err != nil {
		return b, err
	}
	if err == nil {
		b.Replication = len(r.Rules)
	}

	return b, countObjects(ctx, c, &b, opts)
}

func countObjects(ctx context.Context, c *minio.Client, b *Bucket, opts Options) error {
	var seen int64
	objects, versioned := s3util.ListVersions(ctx, c, b.Name, "", true)
	defer func() {
		if b.Versioning != "" && !versioned() {
			b.NoVersionListing = true
			b.Unsupported = append(b.Unsupported, "version listing")
		}
	}()
	for obj := range objects {
		if obj.Err != nil {
			return fmt.Errorf("listing objects: %w", obj.Err)
		}
		seen++
		if opts.Progress != nil && seen%1000 == 0 {
			opts.Progress(b.Name, seen)
		}
		if obj.IsDeleteMarker {
			b.DeleteMarkers++
			continue
		}
		b.TotalBytes += obj.Size
		if obj.IsLatest || obj.VersionID == "" {
			b.Objects++
			b.CurrentBytes += obj.Size
		} else {
			b.OldVersions++
		}
		if len(obj.UserTags) > 0 || obj.UserTagCount > 0 {
			b.TaggedVersions++
		}
		if hasMetadata(obj.UserMetadata) {
			b.Metadata++
		}
		if sc := obj.StorageClass; sc != "" && sc != "STANDARD" {
			if b.StorageClasses == nil {
				b.StorageClasses = map[string]int64{}
			}
			b.StorageClasses[sc]++
		}
		if b.ObjectLock {
			if err := countLocks(ctx, c, b, obj); err != nil {
				return err
			}
		}
	}
	// An interrupted listing just ends; never report its partial counts.
	return ctx.Err()
}

// hasMetadata reports user metadata or headers beyond Content-Type.
func hasMetadata(m map[string]string) bool {
	for k := range m {
		switch lk := strings.ToLower(k); {
		case strings.HasPrefix(lk, "x-amz-meta-"),
			lk == "cache-control", lk == "content-disposition", lk == "content-language",
			lk == "content-encoding", lk == "expires":
			return true
		}
	}
	return false
}

func countLocks(ctx context.Context, c *minio.Client, b *Bucket, obj minio.ObjectInfo) error {
	mode, until, err := c.GetObjectRetention(ctx, b.Name, obj.Key, obj.VersionID)
	if err != nil && !s3util.NotConfigured(err) {
		return fmt.Errorf("retention of %s: %w", obj.Key, err)
	}
	if err == nil && mode != nil && until != nil && until.After(time.Now()) {
		b.Retained++
	}
	hold, err := c.GetObjectLegalHold(ctx, b.Name, obj.Key, minio.GetObjectLegalHoldOptions{VersionID: obj.VersionID})
	if err != nil && !s3util.NotConfigured(err) {
		return fmt.Errorf("legal hold of %s: %w", obj.Key, err)
	}
	if err == nil && hold != nil && *hold == minio.LegalHoldEnabled {
		b.LegalHolds++
	}
	return nil
}
