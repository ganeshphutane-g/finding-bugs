// Package check compares what source buckets use with what a target server
// supports, and says in plain words what a migration would lose.
package check

import (
	"fmt"
	"sort"

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

// Compare builds the report. existing names buckets the target already has.
func Compare(inv []inventory.Bucket, caps probe.Capabilities, existing map[string]bool) Report {
	r := Report{Target: caps}
	for _, b := range inv {
		out := Bucket{Name: b.Name, Losses: []Loss{}, TargetExists: existing[b.Name]}
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
		if b.ObjectLock && without(probe.ObjectLock) {
			lose(probe.ObjectLock, "object lock would be gone: nothing would stop objects being deleted or overwritten")
		}
		if b.Retained > 0 && (without(probe.ObjectLock) || without(probe.Retention)) {
			lose(probe.Retention, "%s %s under retention would lose it", count(b.Retained), plural(b.Retained, "version"))
		}
		if b.LegalHolds > 0 && (without(probe.ObjectLock) || without(probe.LegalHold)) {
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
		if b.CORSRules > 0 && without(probe.CORS) {
			lose(probe.CORS, "CORS rules would be lost (browsers on other sites could not use the bucket)")
		}

		if b.Encryption != "" {
			out.Manual = append(out.Manual, "default encryption ("+b.Encryption+")")
		}
		if b.Notifications > 0 {
			out.Manual = append(out.Manual, "event notifications")
		}
		if b.Replication > 0 {
			out.Manual = append(out.Manual, "replication rules")
		}
		r.Buckets = append(r.Buckets, out)
	}
	sort.Slice(r.Buckets, func(i, j int) bool { return r.Buckets[i].Name < r.Buckets[j].Name })
	return r
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
