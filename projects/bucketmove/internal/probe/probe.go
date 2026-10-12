// Package probe finds out what an S3 server really supports by trying each
// feature on a scratch bucket, instead of trusting what it claims.
package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/s3util"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// Feature names, in report order.
const (
	Versioning   = "versioning"
	DeleteMarker = "delete markers"
	ObjectTags   = "object tags"
	Metadata     = "user metadata and headers"
	Multipart    = "multipart upload"
	OddKeys      = "unicode and special keys"
	BucketTags   = "bucket tags"
	Lifecycle    = "lifecycle rules"
	Policy       = "bucket policy"
	CORS         = "CORS"
	ObjectLock   = "object lock"
	Retention    = "object retention"
	LegalHold    = "legal hold"
)

// Order lists every feature in the order reports show them.
var Order = []string{Versioning, DeleteMarker, ObjectTags, Metadata, Multipart, OddKeys,
	BucketTags, Lifecycle, Policy, CORS, ObjectLock, Retention, LegalHold}

// Result is whether a feature works, and if not, what went wrong.
type Result struct {
	Supported bool   `json:"supported"`
	Detail    string `json:"detail,omitempty"`
}

// Capabilities maps feature names to results.
type Capabilities map[string]Result

// Supports reports whether feature works.
func (c Capabilities) Supports(feature string) bool { return c[feature].Supported }

// Run probes the server behind c. It creates and removes scratch buckets
// named bucketmove-probe-*.
func Run(ctx context.Context, c *minio.Client) (Capabilities, error) {
	caps := Capabilities{}
	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	name := "bucketmove-probe-" + hex.EncodeToString(id)

	if err := c.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
		return nil, fmt.Errorf("cannot create a scratch bucket on the target (bucketmove needs to create buckets): %w", err)
	}
	defer cleanup(ctx, c, name)

	p := prober{ctx: ctx, c: c, bucket: name, caps: caps}
	p.versioning()
	p.objectTags()
	p.metadata()
	p.multipart()
	p.oddKeys()
	p.bucketTags()
	p.lifecycle()
	p.policy()
	p.cors()

	lockName := name + "-lock"
	if err := c.MakeBucket(ctx, lockName, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
		for _, f := range []string{ObjectLock, Retention, LegalHold} {
			caps[f] = Result{Detail: "creating a bucket with object lock failed: " + short(err)}
		}
	} else {
		defer cleanup(ctx, c, lockName)
		lp := prober{ctx: ctx, c: c, bucket: lockName, caps: caps}
		lp.objectLock()
	}
	return caps, nil
}

type prober struct {
	ctx    context.Context
	c      *minio.Client
	bucket string
	caps   Capabilities
}

func (p prober) set(feature string, err error, detail string) {
	if err != nil {
		p.caps[feature] = Result{Detail: short(err)}
		return
	}
	if detail != "" {
		p.caps[feature] = Result{Detail: detail}
		return
	}
	p.caps[feature] = Result{Supported: true}
}

func (p prober) put(key string, body []byte, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return p.c.PutObject(p.ctx, p.bucket, key, bytes.NewReader(body), int64(len(body)), opts)
}

func (p prober) versioning() {
	if err := p.c.EnableVersioning(p.ctx, p.bucket); err != nil {
		p.set(Versioning, err, "")
		p.set(DeleteMarker, nil, "needs versioning")
		return
	}
	v, err := p.c.GetBucketVersioning(p.ctx, p.bucket)
	if err != nil || !v.Enabled() {
		p.set(Versioning, err, "versioning could be turned on but is not reported as enabled")
		p.set(DeleteMarker, nil, "needs versioning")
		return
	}
	a, err1 := p.put("v", []byte("one"), minio.PutObjectOptions{})
	b, err2 := p.put("v", []byte("two"), minio.PutObjectOptions{})
	if err1 != nil || err2 != nil {
		p.set(Versioning, firstErr(err1, err2), "")
		return
	}
	versions, markers, err := p.listVersions("v")
	switch {
	case err != nil:
		p.set(Versioning, err, "")
	case len(versions) != 2 || a.VersionID == "" || b.VersionID == "" || a.VersionID == b.VersionID:
		p.set(Versioning, nil, fmt.Sprintf("accepted the setting but kept %d version(s) of an object written twice", len(versions)))
	default:
		// Old versions must be readable, not just listed.
		obj, err := p.c.GetObject(p.ctx, p.bucket, "v", minio.GetObjectOptions{VersionID: a.VersionID})
		var buf bytes.Buffer
		if err == nil {
			_, err = buf.ReadFrom(obj)
			obj.Close()
		}
		if err != nil || buf.String() != "one" {
			p.set(Versioning, err, "old versions are listed but cannot be read back")
		} else {
			p.set(Versioning, nil, "")
		}
	}
	if !p.caps.Supports(Versioning) {
		p.set(DeleteMarker, nil, "needs versioning")
		return
	}
	if err := p.c.RemoveObject(p.ctx, p.bucket, "v", minio.RemoveObjectOptions{}); err != nil {
		p.set(DeleteMarker, err, "")
		return
	}
	versions, markers, err = p.listVersions("v")
	switch {
	case err != nil:
		p.set(DeleteMarker, err, "")
	case markers != 1 || len(versions) != 2:
		p.set(DeleteMarker, nil, fmt.Sprintf("deleting a versioned object left %d delete marker(s) and %d version(s), expected 1 and 2", markers, len(versions)))
	default:
		p.set(DeleteMarker, nil, "")
	}
}

func (p prober) listVersions(key string) (versions []string, markers int, err error) {
	for obj := range p.c.ListObjects(p.ctx, p.bucket, minio.ListObjectsOptions{Prefix: key, WithVersions: true, Recursive: true}) {
		if obj.Err != nil {
			return nil, 0, obj.Err
		}
		if obj.Key != key {
			continue
		}
		if obj.IsDeleteMarker {
			markers++
		} else {
			versions = append(versions, obj.VersionID)
		}
	}
	return versions, markers, nil
}

func (p prober) objectTags() {
	want := map[string]string{"bucketmove": "probe", "k2": "v 2"}
	if _, err := p.put("tagged", []byte("t"), minio.PutObjectOptions{UserTags: want}); err != nil {
		p.set(ObjectTags, err, "")
		return
	}
	got, err := p.c.GetObjectTagging(p.ctx, p.bucket, "tagged", minio.GetObjectTaggingOptions{})
	if err != nil {
		p.set(ObjectTags, err, "")
		return
	}
	p.set(ObjectTags, nil, compareMaps(want, got.ToMap(), "tags"))
}

func (p prober) metadata() {
	opts := minio.PutObjectOptions{
		ContentType:        "text/x-bucketmove",
		CacheControl:       "max-age=123",
		ContentDisposition: `attachment; filename="probe.txt"`,
		ContentEncoding:    "identity",
		ContentLanguage:    "en",
		UserMetadata:       map[string]string{"Probe-Key": "probe value"},
	}
	if _, err := p.put("meta", []byte("m"), opts); err != nil {
		p.set(Metadata, err, "")
		return
	}
	st, err := p.c.StatObject(p.ctx, p.bucket, "meta", minio.StatObjectOptions{})
	if err != nil {
		p.set(Metadata, err, "")
		return
	}
	var lost []string
	check := func(name, want, got string) {
		if want != got {
			lost = append(lost, name)
		}
	}
	check("Content-Type", opts.ContentType, st.ContentType)
	check("Cache-Control", opts.CacheControl, st.Metadata.Get("Cache-Control"))
	check("Content-Disposition", opts.ContentDisposition, st.Metadata.Get("Content-Disposition"))
	check("Content-Language", opts.ContentLanguage, st.Metadata.Get("Content-Language"))
	check("user metadata", "probe value", st.UserMetadata["Probe-Key"])
	if len(lost) > 0 {
		p.set(Metadata, nil, "not kept: "+strings.Join(lost, ", "))
		return
	}
	p.set(Metadata, nil, "")
}

func (p prober) multipart() {
	body := make([]byte, 5<<20+1024)
	if _, err := rand.Read(body); err != nil {
		p.set(Multipart, err, "")
		return
	}
	if _, err := p.put("multipart", body, minio.PutObjectOptions{PartSize: 5 << 20}); err != nil {
		p.set(Multipart, err, "")
		return
	}
	st, err := p.c.StatObject(p.ctx, p.bucket, "multipart", minio.StatObjectOptions{})
	if err == nil && st.Size != int64(len(body)) {
		p.set(Multipart, nil, fmt.Sprintf("a %d-byte upload came back as %d bytes", len(body), st.Size))
		return
	}
	p.set(Multipart, err, "")
}

func (p prober) oddKeys() {
	keys := []string{"dir/ünïcødé ✓.txt", "a+b=c&d?e#f.txt", "spaces  and\ttab", "deep/a/b/c/d/e/f/g.txt"}
	for _, k := range keys {
		if _, err := p.put(k, []byte("k"), minio.PutObjectOptions{}); err != nil {
			p.set(OddKeys, err, "")
			return
		}
	}
	found := map[string]bool{}
	for obj := range p.c.ListObjects(p.ctx, p.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			p.set(OddKeys, obj.Err, "")
			return
		}
		found[obj.Key] = true
	}
	for _, k := range keys {
		if !found[k] {
			p.set(OddKeys, nil, fmt.Sprintf("key %q was not listed back exactly", k))
			return
		}
	}
	p.set(OddKeys, nil, "")
}

func (p prober) bucketTags() {
	want := map[string]string{"bucketmove": "probe"}
	t, err := tags.NewTags(want, false)
	if err != nil {
		p.set(BucketTags, err, "")
		return
	}
	if err := p.c.SetBucketTagging(p.ctx, p.bucket, t); err != nil {
		p.set(BucketTags, err, "")
		return
	}
	got, err := p.c.GetBucketTagging(p.ctx, p.bucket)
	if err != nil {
		p.set(BucketTags, err, "")
		return
	}
	p.set(BucketTags, nil, compareMaps(want, got.ToMap(), "tags"))
}

func (p prober) lifecycle() {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{{
		ID: "bucketmove-probe", Status: "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: "tmp/"},
		Expiration: lifecycle.Expiration{Days: 30},
	}}
	if err := p.c.SetBucketLifecycle(p.ctx, p.bucket, cfg); err != nil {
		p.set(Lifecycle, err, "")
		return
	}
	got, err := p.c.GetBucketLifecycle(p.ctx, p.bucket)
	switch {
	case err != nil:
		p.set(Lifecycle, err, "")
	case got == nil || len(got.Rules) != 1:
		p.set(Lifecycle, nil, "a lifecycle rule was accepted but not returned")
	default:
		p.set(Lifecycle, nil, "")
	}
}

func (p prober) policy() {
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + p.bucket + `/public/*"]}]}`
	if err := p.c.SetBucketPolicy(p.ctx, p.bucket, policy); err != nil {
		p.set(Policy, err, "")
		return
	}
	got, err := p.c.GetBucketPolicy(p.ctx, p.bucket)
	switch {
	case err != nil:
		p.set(Policy, err, "")
	case !strings.Contains(got, "s3:GetObject"):
		p.set(Policy, nil, "a bucket policy was accepted but not returned")
	default:
		p.set(Policy, nil, "")
	}
}

func (p prober) cors() {
	cfg := cors.NewConfig([]cors.Rule{{AllowedOrigin: []string{"https://example.com"}, AllowedMethod: []string{"GET"}, MaxAgeSeconds: 60}})
	if err := p.c.SetBucketCors(p.ctx, p.bucket, cfg); err != nil {
		p.set(CORS, err, "")
		return
	}
	got, err := p.c.GetBucketCors(p.ctx, p.bucket)
	switch {
	case err != nil:
		p.set(CORS, err, "")
	case got == nil || len(got.CORSRules) != 1:
		p.set(CORS, nil, "a CORS rule was accepted but not returned")
	default:
		p.set(CORS, nil, "")
	}
}

func (p prober) objectLock() {
	lock, _, _, _, err := p.c.GetObjectLockConfig(p.ctx, p.bucket)
	if err != nil || lock != "Enabled" {
		detail := "a bucket created with object lock does not report it as enabled"
		if err != nil {
			detail = short(err)
		}
		for _, f := range []string{ObjectLock, Retention, LegalHold} {
			p.caps[f] = Result{Detail: detail}
		}
		return
	}
	p.set(ObjectLock, nil, "")

	until := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
	mode := minio.Governance
	info, err := p.put("retained", []byte("r"), minio.PutObjectOptions{Mode: mode, RetainUntilDate: until})
	if err != nil {
		p.set(Retention, err, "")
	} else {
		m, u, err := p.c.GetObjectRetention(p.ctx, p.bucket, "retained", info.VersionID)
		switch {
		case err != nil:
			p.set(Retention, err, "")
		case m == nil || *m != mode || u == nil || !u.Equal(until):
			p.set(Retention, nil, "retention was accepted but not returned as set")
		default:
			// Retention must actually protect the object.
			if err := p.c.RemoveObject(p.ctx, p.bucket, "retained", minio.RemoveObjectOptions{VersionID: info.VersionID}); err == nil {
				p.set(Retention, nil, "an object under retention could be deleted")
			} else {
				p.set(Retention, nil, "")
			}
		}
	}

	held, err := p.put("held", []byte("h"), minio.PutObjectOptions{})
	if err != nil {
		p.set(LegalHold, err, "")
		return
	}
	on := minio.LegalHoldEnabled
	if err := p.c.PutObjectLegalHold(p.ctx, p.bucket, "held", minio.PutObjectLegalHoldOptions{VersionID: held.VersionID, Status: &on}); err != nil {
		p.set(LegalHold, err, "")
		return
	}
	status, err := p.c.GetObjectLegalHold(p.ctx, p.bucket, "held", minio.GetObjectLegalHoldOptions{VersionID: held.VersionID})
	switch {
	case err != nil:
		p.set(LegalHold, err, "")
	case status == nil || *status != minio.LegalHoldEnabled:
		p.set(LegalHold, nil, "a legal hold was accepted but not returned")
	default:
		p.set(LegalHold, nil, "")
	}
}

// cleanup removes a scratch bucket and everything in it. It keeps going
// after an interrupt, and waits out the probe's short retention if the server
// does not allow a governance bypass.
func cleanup(ctx context.Context, c *minio.Client, bucket string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		objects, _ := s3util.ListVersions(ctx, c, bucket, "", false)
		for obj := range objects {
			if obj.Err != nil {
				continue
			}
			_ = RemoveVersion(ctx, c, bucket, obj.Key, obj.VersionID)
		}
		if err := c.RemoveBucket(ctx, bucket); err == nil {
			return
		}
		time.Sleep(6 * time.Second)
	}
}

// RemoveVersion deletes one object version as plainly as the server allows:
// first without extra headers (some servers reject the version and bypass
// options they do not implement), then lifting a legal hold and bypassing
// governance retention.
func RemoveVersion(ctx context.Context, c *minio.Client, bucket, key, versionID string) error {
	if versionID == "null" {
		versionID = ""
	}
	err := c.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{VersionID: versionID})
	if err == nil {
		return nil
	}
	off := minio.LegalHoldDisabled
	_ = c.PutObjectLegalHold(ctx, bucket, key, minio.PutObjectLegalHoldOptions{VersionID: versionID, Status: &off})
	return c.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{VersionID: versionID, GovernanceBypass: true})
}

func compareMaps(want, got map[string]string, what string) string {
	for k, v := range want {
		if got[k] != v {
			return fmt.Sprintf("%s were accepted but not returned exactly", what)
		}
	}
	return ""
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// short trims long server errors to their first line.
func short(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 160 {
		msg = msg[:157] + "..."
	}
	return msg
}
