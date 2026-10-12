package copier

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/ganeshphutane-g/bucketmove/internal/endpoint"
	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/s3util"
	"github.com/minio/minio-go/v7"
)

func liveClient(t *testing.T, env string) *minio.Client {
	t.Helper()
	if os.Getenv("BUCKETMOVE_INTEGRATION") == "" || os.Getenv(env) == "" {
		t.Skip("needs BUCKETMOVE_INTEGRATION and " + env)
	}
	ep, err := endpoint.Resolve(os.Getenv(env), "to", endpoint.Env{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	c, err := ep.Client(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func versionsOf(t *testing.T, c *minio.Client, bucket, key string) (n int, markers int) {
	t.Helper()
	objects, _ := s3util.ListVersions(context.Background(), c, bucket, key, false)
	for o := range objects {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		if o.Key != key {
			continue
		}
		if o.IsDeleteMarker {
			markers++
		} else {
			n++
		}
	}
	return n, markers
}

func wipe(c *minio.Client, bucket string) {
	ctx := context.Background()
	objects, _ := s3util.ListVersions(ctx, c, bucket, "", false)
	for o := range objects {
		if o.Err == nil {
			_ = probe.RemoveVersion(ctx, c, bucket, o.Key, o.VersionID)
		}
	}
	_ = c.RemoveBucket(ctx, bucket)
}

// TestResumeAfterCrashAtEveryPoint crashes the copy after each "started"
// record in turn, before the write happens, then resumes. Versions have the
// same size, so only correct bookkeeping gets the history right.
func TestResumeAfterCrashAtEveryPoint(t *testing.T) {
	src := liveClient(t, "BUCKETMOVE_TEST_MINIO")
	var dst *minio.Client
	for _, env := range []string{"BUCKETMOVE_TEST_RUSTFS", "BUCKETMOVE_TEST_SEAWEEDFS"} {
		if os.Getenv(env) != "" {
			dst = liveClient(t, env)
			break
		}
	}
	if dst == nil {
		t.Skip("needs a versioned target")
	}
	ctx := context.Background()
	id := make([]byte, 3)
	rand.Read(id)
	bucket := "rs" + hex.EncodeToString(id)
	if err := src.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wipe(src, bucket) })
	if err := src.EnableVersioning(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	// History of "k": v0, v1, marker, v2, marker, v3 (all bodies 2 bytes).
	steps := []string{"v0", "v1", "-", "v2", "-", "v3"}
	for _, s := range steps {
		if s == "-" {
			if err := src.RemoveObject(ctx, bucket, "k", minio.RemoveObjectOptions{}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := src.PutObject(ctx, bucket, "k", bytes.NewReader([]byte(s)), 2, minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	caps, err := probe.Run(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.Take(ctx, src, inventory.Options{Buckets: []string{bucket}})
	if err != nil {
		t.Fatal(err)
	}

	for crashAt := 0; crashAt < len(steps); crashAt++ {
		t.Run(fmt.Sprintf("crash_before_%d", crashAt), func(t *testing.T) {
			wipe(dst, bucket)
			state := t.TempDir()
			n := 0
			crash := errors.New("simulated crash")
			cp := Copier{Src: src, Dst: dst, Caps: caps, Opts: Options{StateDir: state, Workers: 1},
				beforeWrite: func(Entry) error {
					if n == crashAt {
						n++
						return crash
					}
					n++
					return nil
				}}
			if _, err := cp.Bucket(ctx, inv[0]); !errors.Is(err, crash) {
				t.Fatalf("expected the simulated crash, got %v", err)
			}
			cp.beforeWrite = nil
			if _, err := cp.Bucket(ctx, inv[0]); err != nil {
				t.Fatal(err)
			}
			v, m := versionsOf(t, dst, bucket, "k")
			if v != 4 || m != 2 {
				t.Fatalf("after resume the target has %d versions and %d markers, want 4 and 2", v, m)
			}
		})
	}
	wipe(dst, bucket)
}
