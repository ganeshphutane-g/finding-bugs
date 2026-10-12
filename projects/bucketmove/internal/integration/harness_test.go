// Package integration tests bucketmove against real S3 servers.
//
// Start them with scripts/test-servers.sh, then run:
//
//	set -a; . ./test-servers.env; set +a
//	BUCKETMOVE_INTEGRATION=1 go test ./internal/integration/
//
// Every test uses buckets with a random prefix and removes them afterwards.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/endpoint"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// server is one test server.
type server struct {
	name string
	ep   endpoint.Endpoint
	c    *minio.Client
}

// servers returns the source (MinIO) and the configured targets.
func servers(t *testing.T) (source server, targets []server) {
	t.Helper()
	if os.Getenv("BUCKETMOVE_INTEGRATION") == "" {
		t.Skip("set BUCKETMOVE_INTEGRATION=1 and start scripts/test-servers.sh to run")
	}
	get := func(name, env string) (server, bool) {
		raw := os.Getenv(env)
		if raw == "" {
			return server{}, false
		}
		ep, err := endpoint.Resolve(raw, "from", endpoint.Env{Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		c, err := ep.Client(nil)
		if err != nil {
			t.Fatal(err)
		}
		return server{name: name, ep: ep, c: c}, true
	}
	source, ok := get("minio", "BUCKETMOVE_TEST_MINIO")
	if !ok {
		t.Fatal("BUCKETMOVE_INTEGRATION is set but BUCKETMOVE_TEST_MINIO is not; source test-servers.env")
	}
	for _, s := range []struct{ name, env string }{
		{"garage", "BUCKETMOVE_TEST_GARAGE"},
		{"seaweedfs", "BUCKETMOVE_TEST_SEAWEEDFS"},
		{"rustfs", "BUCKETMOVE_TEST_RUSTFS"},
		{"minio-target", "BUCKETMOVE_TEST_MINIO"},
	} {
		if srv, ok := get(s.name, s.env); ok {
			targets = append(targets, srv)
		}
	}
	return source, targets
}

func prefix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "bm" + hex.EncodeToString(b)
}

// removeBuckets deletes every bucket starting with p on the server, including
// all versions and (governance) locked objects.
func removeBuckets(t *testing.T, s server, p string) {
	t.Helper()
	ctx := context.Background()
	buckets, err := s.c.ListBuckets(ctx)
	if err != nil {
		t.Logf("cleanup %s: %v", s.name, err)
		return
	}
	for _, b := range buckets {
		if !strings.HasPrefix(b.Name, p) {
			continue
		}
		for obj := range s.c.ListObjects(ctx, b.Name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if obj.Err != nil {
				break
			}
			off := minio.LegalHoldDisabled
			_ = s.c.PutObjectLegalHold(ctx, b.Name, obj.Key, minio.PutObjectLegalHoldOptions{VersionID: obj.VersionID, Status: &off})
			_ = s.c.RemoveObject(ctx, b.Name, obj.Key, minio.RemoveObjectOptions{VersionID: obj.VersionID, GovernanceBypass: true})
		}
		if err := s.c.RemoveBucket(ctx, b.Name); err != nil {
			t.Logf("cleanup %s/%s: %v", s.name, b.Name, err)
		}
	}
}

func put(t *testing.T, c *minio.Client, bucket, key string, body []byte, opts minio.PutObjectOptions) minio.UploadInfo {
	t.Helper()
	info, err := c.PutObject(context.Background(), bucket, key, bytes.NewReader(body), int64(len(body)), opts)
	if err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
	return info
}

func del(t *testing.T, c *minio.Client, bucket, key string) {
	t.Helper()
	if err := c.RemoveObject(context.Background(), bucket, key, minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("delete %s/%s: %v", bucket, key, err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// fixture describes the seeded source buckets.
type fixture struct {
	plain, history, locked, configured string
}

// seed fills the MinIO source with buckets that exercise every feature
// bucketmove cares about.
func seed(t *testing.T, s server, p string) fixture {
	t.Helper()
	ctx := context.Background()
	c := s.c
	f := fixture{plain: p + "-plain", history: p + "-history", locked: p + "-locked", configured: p + "-configured"}
	t.Cleanup(func() { removeBuckets(t, s, p) })

	// plain: unversioned, with content types, metadata and awkward keys.
	must(t, c.MakeBucket(ctx, f.plain, minio.MakeBucketOptions{}))
	put(t, c, f.plain, "docs/readme.txt", []byte("hello\n"), minio.PutObjectOptions{
		ContentType: "text/plain", UserMetadata: map[string]string{"Owner": "ops"}, CacheControl: "max-age=60"})
	put(t, c, f.plain, "img/logo.bin", randomBytes(t, 4096), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	put(t, c, f.plain, "empty", nil, minio.PutObjectOptions{})
	put(t, c, f.plain, "unicode/ünï cødé + spaces.txt", []byte("ü"), minio.PutObjectOptions{})

	// history: versions, delete markers, tags and a multipart object.
	must(t, c.MakeBucket(ctx, f.history, minio.MakeBucketOptions{}))
	must(t, c.EnableVersioning(ctx, f.history))
	for i := 1; i <= 3; i++ {
		put(t, c, f.history, "notes.txt", []byte(fmt.Sprintf("version %d\n", i)), minio.PutObjectOptions{ContentType: "text/plain"})
	}
	put(t, c, f.history, "gone.txt", []byte("soon deleted"), minio.PutObjectOptions{})
	del(t, c, f.history, "gone.txt")
	put(t, c, f.history, "back.txt", []byte("first"), minio.PutObjectOptions{})
	del(t, c, f.history, "back.txt")
	put(t, c, f.history, "back.txt", []byte("second"), minio.PutObjectOptions{})
	put(t, c, f.history, "tagged.txt", []byte("tags"), minio.PutObjectOptions{UserTags: map[string]string{"class": "secret", "year": "2026"}})
	big := randomBytes(t, 12<<20)
	put(t, c, f.history, "big.bin", big, minio.PutObjectOptions{PartSize: 5 << 20})
	put(t, c, f.history, "big.bin", big[:7<<20], minio.PutObjectOptions{PartSize: 5 << 20})

	// locked: object lock with a default retention and a legal hold.
	must(t, c.MakeBucket(ctx, f.locked, minio.MakeBucketOptions{ObjectLocking: true}))
	mode, days, unit := minio.Governance, uint(1), minio.Days
	must(t, c.SetObjectLockConfig(ctx, f.locked, &mode, &days, &unit))
	put(t, c, f.locked, "contract.pdf", []byte("%PDF-1.7"), minio.PutObjectOptions{ContentType: "application/pdf"})
	held := put(t, c, f.locked, "evidence.txt", []byte("keep"), minio.PutObjectOptions{})
	on := minio.LegalHoldEnabled
	must(t, c.PutObjectLegalHold(ctx, f.locked, "evidence.txt", minio.PutObjectLegalHoldOptions{VersionID: held.VersionID, Status: &on}))

	// configured: bucket settings without versioning.
	must(t, c.MakeBucket(ctx, f.configured, minio.MakeBucketOptions{}))
	lc := lifecycle.NewConfiguration()
	lc.Rules = []lifecycle.Rule{{
		ID: "expire-tmp", Status: "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "tmp/"},
		Expiration: lifecycle.Expiration{Days: 7},
	}}
	must(t, c.SetBucketLifecycle(ctx, f.configured, lc))
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + f.configured + `/public/*"]}]}`
	must(t, c.SetBucketPolicy(ctx, f.configured, policy))
	// MinIO has no per-bucket CORS (it answers NotImplemented), so the
	// source fixture has none.
	bt, err := tags.NewTags(map[string]string{"team": "web"}, false)
	must(t, err)
	must(t, c.SetBucketTagging(ctx, f.configured, bt))
	put(t, c, f.configured, "public/index.html", []byte("<h1>hi</h1>"), minio.PutObjectOptions{ContentType: "text/html"})
	put(t, c, f.configured, "tmp/scratch", []byte("x"), minio.PutObjectOptions{})

	// Let object timestamps differ from the next test's.
	time.Sleep(10 * time.Millisecond)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		_, file, line, _ := runtime.Caller(1)
		t.Fatalf("%s:%d: %v", filepath.Base(file), line, err)
	}
}
