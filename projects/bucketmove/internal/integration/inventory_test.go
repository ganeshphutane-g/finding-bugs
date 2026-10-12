package integration

import (
	"context"
	"testing"

	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
)

func TestInventoryOfSeededMinIO(t *testing.T) {
	src, _ := servers(t)
	p := prefix(t)
	f := seed(t, src, p)

	inv, err := inventory.Take(context.Background(), src.c, inventory.Options{
		Buckets: []string{f.plain, f.history, f.locked, f.configured},
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]inventory.Bucket{}
	for _, b := range inv {
		by[b.Name] = b
	}

	plain := by[f.plain]
	if plain.Objects != 4 || plain.OldVersions != 0 || plain.DeleteMarkers != 0 || plain.Versioning != "" || plain.Metadata != 1 {
		t.Errorf("plain: %+v", plain)
	}

	h := by[f.history]
	// pre.txt 2 (null + one), notes.txt 3 versions, gone.txt 1 + marker,
	// back.txt 2 + marker, tagged.txt 1, big.bin 2.
	if h.Versioning != "Enabled" || h.Objects != 5 || h.OldVersions != 6 || h.DeleteMarkers != 2 || h.TaggedVersions != 1 {
		t.Errorf("history: %+v", h)
	}
	if h.TotalBytes < 19<<20 || h.CurrentBytes < 7<<20 {
		t.Errorf("history sizes: total %d current %d", h.TotalBytes, h.CurrentBytes)
	}

	l := by[f.locked]
	if !l.ObjectLock || l.LockDefault != "GOVERNANCE 1 days" || l.Retained != 2 || l.LegalHolds != 1 || l.Versioning != "Enabled" {
		t.Errorf("locked: %+v", l)
	}

	cfg := by[f.configured]
	if cfg.LifecycleRules != 1 || !cfg.Policy || cfg.Tags["team"] != "web" || cfg.Objects != 2 {
		t.Errorf("configured: %+v", cfg)
	}
	for name, b := range by {
		// MinIO has no per-bucket CORS; everything else it reports.
		for _, u := range b.Unsupported {
			if u != "CORS" {
				t.Errorf("%s: MinIO should report %s", name, u)
			}
		}
	}
}
