package check

import (
	"strings"
	"testing"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
)

func all(supported bool) probe.Capabilities {
	c := probe.Capabilities{}
	for _, f := range probe.Order {
		c[f] = probe.Result{Supported: supported}
	}
	return c
}

func TestEverythingSupported(t *testing.T) {
	inv := []inventory.Bucket{{Name: "b", Versioning: "Enabled", OldVersions: 3, DeleteMarkers: 1, ObjectLock: true,
		Retained: 2, LegalHolds: 1, TaggedVersions: 4, Tags: map[string]string{"a": "b"}, LifecycleRules: 1, Policy: true, Metadata: 2}}
	r := Compare(inv, all(true), nil)
	if r.HasLoss() || len(r.Buckets[0].Losses) != 0 {
		t.Fatalf("unexpected losses: %+v", r.Buckets[0].Losses)
	}
}

func TestGarageLikeTarget(t *testing.T) {
	caps := all(true)
	for _, f := range []string{probe.Versioning, probe.DeleteMarker, probe.ObjectTags, probe.BucketTags, probe.Policy, probe.ObjectLock, probe.Retention, probe.LegalHold} {
		caps[f] = probe.Result{Detail: "Unimplemented"}
	}
	inv := []inventory.Bucket{
		{Name: "history", Versioning: "Enabled", OldVersions: 12345, DeleteMarkers: 2, TaggedVersions: 1},
		{Name: "plain", Objects: 10},
		{Name: "fresh", Versioning: "Enabled"},
		{Name: "locked", Versioning: "Enabled", ObjectLock: true, Retained: 2, LegalHolds: 1},
		{Name: "web", Policy: true, Tags: map[string]string{"team": "web"}, Encryption: "AES256"},
	}
	r := Compare(inv, caps, map[string]bool{"plain": true})
	by := map[string]Bucket{}
	for _, b := range r.Buckets {
		by[b.Name] = b
	}
	if !r.HasLoss() {
		t.Fatal("expected losses")
	}
	if len(by["plain"].Losses) != 0 || !by["plain"].TargetExists {
		t.Fatalf("plain: %+v", by["plain"])
	}
	h := by["history"].Losses
	if len(h) != 2 || !strings.Contains(h[0].What, "12,345 old versions and 2 delete markers would be lost") || h[1].Feature != probe.ObjectTags {
		t.Fatalf("history: %+v", h)
	}
	if f := by["fresh"].Losses; len(f) != 1 || !strings.Contains(f[0].What, "versioning would be off") {
		t.Fatalf("fresh: %+v", f)
	}
	if l := by["locked"].Losses; len(l) != 4 {
		t.Fatalf("locked: want versioning, lock, retention and legal hold losses, got %+v", l)
	}
	w := by["web"]
	if len(w.Losses) != 2 || len(w.Manual) != 1 {
		t.Fatalf("web: %+v", w)
	}
}
