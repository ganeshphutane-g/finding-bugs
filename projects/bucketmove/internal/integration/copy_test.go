package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/copier"
	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/verify"
	"github.com/minio/minio-go/v7"
)

// copyTargets are the targets that are a different server from the source.
func copyTargets(t *testing.T) (server, []server) {
	src, targets := servers(t)
	var out []server
	for _, s := range targets {
		if s.ep.URL.Host != src.ep.URL.Host {
			out = append(out, s)
		}
	}
	return src, out
}

func verifyOptions(caps probe.Capabilities, b inventory.Bucket) verify.Options {
	return verify.Options{
		LatestOnly:   b.Versioning == "" || !caps.Supports(probe.Versioning) || !caps.Supports(probe.DeleteMarker),
		SkipTags:     !caps.Supports(probe.ObjectTags),
		GuessedType:  !caps.Supports(probe.ExactType),
		Locks:        b.ObjectLock && caps.Supports(probe.ObjectLock),
		StorageClass: caps.Supports(probe.StorageClass),
	}
}

func TestCopyToEveryTarget(t *testing.T) {
	src, targets := copyTargets(t)
	p := prefix(t)
	f := seed(t, src, p)
	ctx := context.Background()
	inv, err := inventory.Take(ctx, src.c, inventory.Options{Buckets: []string{f.plain, f.history, f.locked, f.configured}})
	if err != nil {
		t.Fatal(err)
	}

	for _, dst := range targets {
		t.Run(dst.name, func(t *testing.T) {
			t.Cleanup(func() { removeBuckets(t, dst, p) })
			caps, err := probe.Run(ctx, dst.c)
			if err != nil {
				t.Fatal(err)
			}
			cp := copier.Copier{Src: src.c, Dst: dst.c, Caps: caps, Opts: copier.Options{StateDir: t.TempDir(), Workers: 3}}
			for _, b := range inv {
				st, err := cp.Bucket(ctx, b)
				if err != nil {
					t.Fatalf("copy %s: %v", b.Name, err)
				}
				res, err := verify.Bucket(ctx, src.c, dst.c, b.Name, verifyOptions(caps, b))
				if err != nil {
					t.Fatalf("verify %s: %v", b.Name, err)
				}
				if !res.OK() {
					t.Errorf("verify %s found differences:\n  %s", b.Name, strings.Join(res.Diffs, "\n  "))
				}
				t.Logf("%s: %+v; verified %d keys, %d versions", b.Name, st, res.Keys, res.Versions)
			}

			if caps.Supports(probe.Versioning) {
				h := inventoryOf(t, dst, f.history)
				if h.OldVersions != 6 || h.DeleteMarkers != 2 || h.Objects != 5 {
					t.Errorf("history on target: %+v", h)
				}
			}
			if caps.Supports(probe.ObjectLock) {
				l := inventoryOf(t, dst, f.locked)
				if !l.ObjectLock || l.LockDefault != "GOVERNANCE 1 days" || l.Retained != 2 || l.LegalHolds != 1 {
					t.Errorf("locked on target: %+v", l)
				}
			}
			cfg := inventoryOf(t, dst, f.configured)
			if caps.Supports(probe.Lifecycle) && cfg.LifecycleRules != 1 {
				t.Errorf("lifecycle not copied: %+v", cfg)
			}
			if caps.Supports(probe.Policy) && !cfg.Policy {
				t.Errorf("policy not copied: %+v", cfg)
			}
			if caps.Supports(probe.BucketTags) && cfg.Tags["team"] != "web" {
				t.Errorf("bucket tags not copied: %+v", cfg)
			}
		})
	}
}

func inventoryOf(t *testing.T, s server, bucket string) inventory.Bucket {
	t.Helper()
	inv, err := inventory.Take(context.Background(), s.c, inventory.Options{Buckets: []string{bucket}})
	if err != nil {
		t.Fatal(err)
	}
	return inv[0]
}

// TestCopyResumes interrupts a copy part-way and checks that running it
// again finishes the job without duplicating versions.
func TestCopyResumes(t *testing.T) {
	src, targets := copyTargets(t)
	var dst server
	for _, s := range targets {
		if s.name == "rustfs" || s.name == "seaweedfs" {
			dst = s
			break
		}
	}
	if dst.c == nil {
		t.Skip("needs a versioned target (RustFS or SeaweedFS)")
	}
	p := prefix(t)
	f := seed(t, src, p)
	t.Cleanup(func() { removeBuckets(t, dst, p) })
	ctx := context.Background()
	caps, err := probe.Run(ctx, dst.c)
	if err != nil {
		t.Fatal(err)
	}
	b := inventoryOf(t, src, f.history)
	state := t.TempDir()

	runCtx, cancel := context.WithCancel(ctx)
	cp := copier.Copier{Src: src.c, Dst: dst.c, Caps: caps, Opts: copier.Options{StateDir: state, Workers: 1,
		Progress: func(_ string, done, _ int64) {
			if done == 3 {
				cancel()
			}
		}}}
	if _, err := cp.Bucket(runCtx, b); err == nil {
		t.Fatal("expected the interrupted copy to report an error")
	}
	cancel()

	cp.Opts.Progress = nil
	st, err := cp.Bucket(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped < 3 {
		t.Errorf("the second run should skip the versions already copied: %+v", st)
	}
	res, err := verify.Bucket(ctx, src.c, dst.c, b.Name, verifyOptions(caps, b))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("after resume:\n  %s", strings.Join(res.Diffs, "\n  "))
	}
}

// TestCopyRefusesExistingBucket checks that an existing target bucket is not
// written into unless asked.
func TestCopyRefusesExistingBucket(t *testing.T) {
	src, targets := copyTargets(t)
	if len(targets) == 0 {
		t.Skip("no targets")
	}
	dst := targets[0]
	p := prefix(t)
	f := seed(t, src, p)
	t.Cleanup(func() { removeBuckets(t, dst, p) })
	ctx := context.Background()
	must(t, dst.c.MakeBucket(ctx, f.plain, minio.MakeBucketOptions{}))
	put(t, dst.c, f.plain, "theirs.txt", []byte("pre-existing"), minio.PutObjectOptions{})

	caps, err := probe.Run(ctx, dst.c)
	if err != nil {
		t.Fatal(err)
	}
	cp := copier.Copier{Src: src.c, Dst: dst.c, Caps: caps, Opts: copier.Options{StateDir: t.TempDir()}}
	if _, err := cp.Bucket(ctx, inventoryOf(t, src, f.plain)); err == nil || !strings.Contains(err.Error(), "--into-existing") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	cp.Opts.IntoExisting = true
	if _, err := cp.Bucket(ctx, inventoryOf(t, src, f.plain)); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.c.StatObject(ctx, f.plain, "theirs.txt", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("the target's own object was touched: %v", err)
	}
	_ = time.Now
}
