package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ganeshphutane-g/bucketmove/internal/copier"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/verify"
	"github.com/minio/minio-go/v7"
)

// TestCopyAtScale copies thousands of keys (more than one listing page), a
// key with many versions and an object larger than one multipart part.
func TestCopyAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	src, targets := copyTargets(t)
	p := prefix(t)
	bucket := p + "-scale"
	ctx := context.Background()
	t.Cleanup(func() { removeBuckets(t, src, p) })
	must(t, src.c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}))
	must(t, src.c.EnableVersioning(ctx, bucket))

	// 2,500 small objects, written in parallel.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := 0; i < 2500; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			put(t, src.c, bucket, fmt.Sprintf("files/%04d/item-%d.json", i/100, i), []byte(fmt.Sprintf(`{"n":%d}`, i)), minio.PutObjectOptions{ContentType: "application/json"})
		}(i)
	}
	wg.Wait()
	// One key with 50 versions and a delete in the middle.
	for i := 0; i < 50; i++ {
		if i == 25 {
			del(t, src.c, bucket, "log.txt")
		}
		put(t, src.c, bucket, "log.txt", []byte(fmt.Sprintf("entry %d\n", i)), minio.PutObjectOptions{})
	}
	// An 80 MiB object: several 16 MiB parts.
	put(t, src.c, bucket, "big/video.bin", randomBytes(t, 80<<20), minio.PutObjectOptions{PartSize: 16 << 20})

	b := inventoryOf(t, src, bucket)
	if b.Objects != 2502 || b.OldVersions != 49 || b.DeleteMarkers != 1 {
		t.Fatalf("unexpected source inventory: %+v", b)
	}

	for _, dst := range targets {
		t.Run(dst.name, func(t *testing.T) {
			t.Cleanup(func() { removeBuckets(t, dst, p) })
			caps, err := probe.Run(ctx, dst.c)
			if err != nil {
				t.Fatal(err)
			}
			cp := copier.Copier{Src: src.c, Dst: dst.c, Caps: caps, Opts: copier.Options{StateDir: t.TempDir(), Workers: 8}}
			st, err := cp.Bucket(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			res, err := verify.Bucket(ctx, src.c, dst.c, bucket, verifyOptions(caps, b))
			if err != nil {
				t.Fatal(err)
			}
			if !res.OK() {
				n := len(res.Diffs)
				if n > 10 {
					n = 10
				}
				t.Fatalf("%d differences, first ones:\n  %s", len(res.Diffs), strings.Join(res.Diffs[:n], "\n  "))
			}
			t.Logf("%+v; verified %d keys, %d versions", st, res.Keys, res.Versions)
		})
	}
}
