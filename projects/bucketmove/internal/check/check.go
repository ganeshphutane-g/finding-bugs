// Package check compares what source buckets use with what a target server
// supports, and says in plain words what a migration would lose.
package check

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
)

// Loss is one thing that would not survive the move.
type Loss struct {
	Feature string `json:"feature"`
	What    string `json:"what"`
}

// Bucket is the verdict for one source bucket.
type Bucket struct {
	Name string `json:"name"`
	// Losses are things the target cannot keep.
	Losses []Loss `json:"losses"`
	// Changes are things the target keeps, but not exactly as they were.
	Changes []string `json:"changes,omitempty"`
	// Manual are settings bucketmove does not copy; they must be set up on
	// the target by hand.
	Manual []string `json:"manual,omitempty"`
	// TargetExists is true when the target already has a bucket of this name.
	TargetExists bool `json:"target_exists"`
}

// Report is the result for all buckets.
type Report struct {
	Buckets []Bucket           `json:"buckets"`
	Target  probe.Capabilities `json:"target_capabilities"`
}

// HasLoss reports whether any bucket would lose something.
func (r Report) HasLoss() bool {
	for _, b := range r.Buckets {
		if len(b.Losses) > 0 {
			return true
		}
	}
	return false
}

// Existing describes a bucket that is already on the target.
type Existing struct {
	ObjectLock bool // created with object lock
}

// Compare builds the report. existing holds the buckets the target already
// has.
func Compare(inv []inventory.Bucket, caps probe.Capabilities, existing map[string]Existing) Report {
	r := Report{Target: caps, Buckets: []Bucket{}}
	for _, b := range inv {
		ex, exists := existing[b.Name]
		out := Bucket{Name: b.Name, Losses: []Loss{}, TargetExists: exists}
		lose := func(feature, format string, args ...any) {
			out.Losses = append(out.Losses, Loss{Feature: feature, What: fmt.Sprintf(format, args...)})
		}
		without := func(feature string) bool { return !caps.Supports(feature) }

		if b.Versioning != "" && without(probe.Versioning) {
			switch {
			case b.OldVersions > 0 || b.DeleteMarkers > 0:
				lose(probe.Versioning, "%s old %s and %s delete %s would be lost: the target keeps only the latest version of each object",
					count(b.OldVersions), plural(b.OldVersions, "version"), count(b.DeleteMarkers), plural(b.DeleteMarkers, "marker"))
			default:
				lose(probe.Versioning, "versioning would be off: future overwrites and deletes could not be undone")
			}
		} else if b.DeleteMarkers > 0 && without(probe.DeleteMarker) {
			lose(probe.DeleteMarker, "%s delete %s would be lost", count(b.DeleteMarkers), plural(b.DeleteMarkers, "marker"))
		}
		if b.NoVersionListing {
			lose(probe.Versioning, "the source could not list old versions, so they cannot be copied")
		}
		noLock := without(probe.ObjectLock) || (exists && !ex.ObjectLock)
		if b.ObjectLock && noLock {
			if exists && !ex.ObjectLock && !without(probe.ObjectLock) {
				lose(probe.ObjectLock, "the existing target bucket has no object lock, and it cannot be added to a bucket later: nothing would stop objects being deleted or overwritten")
			} else {
				lose(probe.ObjectLock, "object lock would be gone: nothing would stop objects being deleted or overwritten")
			}
		}
		if b.Retained > 0 && (noLock || without(probe.Retention)) {
			lose(probe.Retention, "%s %s under retention would lose it", count(b.Retained), plural(b.Retained, "version"))
		}
		if b.LegalHolds > 0 && (noLock || without(probe.LegalHold)) {
			lose(probe.LegalHold, "%s legal %s would be dropped", count(b.LegalHolds), plural(b.LegalHolds, "hold"))
		}
		if b.TaggedVersions > 0 && without(probe.ObjectTags) {
			lose(probe.ObjectTags, "object tags on %s %s would be lost", count(b.TaggedVersions), plural(b.TaggedVersions, "version"))
		}
		if b.Metadata > 0 && without(probe.Metadata) {
			lose(probe.Metadata, "user metadata or headers on %s %s would not be kept (%s)", count(b.Metadata), plural(b.Metadata, "version"), caps[probe.Metadata].Detail)
		}
		if len(b.Tags) > 0 && without(probe.BucketTags) {
			lose(probe.BucketTags, "bucket tags would be lost")
		}
		if b.LifecycleRules > 0 && without(probe.Lifecycle) {
			lose(probe.Lifecycle, "%d lifecycle %s would not run", b.LifecycleRules, plural(int64(b.LifecycleRules), "rule"))
		}
		if b.Policy && without(probe.Policy) {
			lose(probe.Policy, "the bucket policy would be gone (check who can read and write the bucket)")
		}
		if len(b.StorageClasses) > 0 && without(probe.StorageClass) {
			var n int64
			for _, c := range b.StorageClasses {
				n += c
			}
			lose(probe.StorageClass, "the storage class of %s %s would be lost (%s)", count(n), plural(n, "version"), classes(b.StorageClasses))
		}
		if b.CORSRules > 0 && without(probe.CORS) {
			lose(probe.CORS, "CORS rules would be lost (browsers on other sites could not use the bucket)")
		}

		if b.Objects+b.OldVersions > 0 && without(probe.ExactType) {
			out.Changes = append(out.Changes, "objects stored as application/octet-stream may get a Content-Type guessed from their name")
		}
		for _, u := range b.Unsupported {
			if u != "version listing" {
				out.Changes = append(out.Changes, "the source could not report its "+u+" setting, so it is not checked or copied")
			}
		}
		// bucketmove does not copy these; leaving them behind is a loss
		// until they are set up on the target by hand.
		if b.Encryption != "" {
			lose("encryption", "default encryption (%s) is not copied: new objects on the target would be stored unencrypted unless you set it up there", b.Encryption)
		}
		if b.Notifications > 0 {
			lose("notifications", "event notifications are not copied; set them up on the target")
		}
		if b.Replication > 0 {
			lose("replication", "replication rules are not copied; set them up on the target")
		}
		r.Buckets = append(r.Buckets, out)
	}
	sort.Slice(r.Buckets, func(i, j int) bool { return r.Buckets[i].Name < r.Buckets[j].Name })
	return r
}

func classes(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %s", k, count(m[k]))
	}
	return strings.Join(parts, ", ")
}

func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func count(n int64) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
