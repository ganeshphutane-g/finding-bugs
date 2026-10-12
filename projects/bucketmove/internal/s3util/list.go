package s3util

import (
	"context"

	"github.com/minio/minio-go/v7"
)

// ListVersions lists every version of every object, and delete markers,
// under prefix, in the server's order (for S3: by key, newest version first).
//
// Servers without ListObjectVersions (Garage) get a plain listing instead:
// each object is then its only, current version, with an empty version ID.
// versioned reports which of the two happened.
func ListVersions(ctx context.Context, c *minio.Client, bucket, prefix string, withMetadata bool) (objects <-chan minio.ObjectInfo, versioned func() bool) {
	out := make(chan minio.ObjectInfo)
	usedVersions := true
	go func() {
		defer close(out)
		first := true
		for obj := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{
			Prefix: prefix, Recursive: true, WithVersions: true, WithMetadata: withMetadata,
		}) {
			if first && obj.Err != nil && NotSupported(obj.Err) {
				usedVersions = false
				break
			}
			first = false
			if !send(ctx, out, obj) {
				return
			}
		}
		if usedVersions {
			return
		}
		for obj := range c.ListObjects(ctx, bucket, minio.ListObjectsOptions{
			Prefix: prefix, Recursive: true, WithMetadata: withMetadata,
		}) {
			if obj.Err == nil {
				obj.IsLatest = true
			}
			if !send(ctx, out, obj) {
				return
			}
		}
	}()
	return out, func() bool { return usedVersions }
}

func send(ctx context.Context, out chan<- minio.ObjectInfo, obj minio.ObjectInfo) bool {
	select {
	case out <- obj:
		return true
	case <-ctx.Done():
		return false
	}
}
