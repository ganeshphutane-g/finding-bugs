package s3util

import (
	"context"
	"sort"

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

// NewestFirst orders one key's versions newest first by modification time,
// keeping the server's order for equal times. Servers do not all list
// versions in that order (RustFS lists delete markers first).
func NewestFirst(versions []minio.ObjectInfo) {
	sort.SliceStable(versions, func(i, j int) bool {
		return versions[i].LastModified.After(versions[j].LastModified)
	})
}

func send(ctx context.Context, out chan<- minio.ObjectInfo, obj minio.ObjectInfo) bool {
	select {
	case out <- obj:
		return true
	case <-ctx.Done():
		return false
	}
}

// GetVersion opens one version of an object. The "null" version (written
// before versioning was turned on) is asked for by name, because an empty
// version ID means "the latest", which may be a newer version. Servers that
// reject "null" are asked for the latest only when that is the same thing.
func GetVersion(ctx context.Context, c *minio.Client, bucket string, o minio.ObjectInfo) (*minio.Object, minio.ObjectInfo, error) {
	obj, err := c.GetObject(ctx, bucket, o.Key, minio.GetObjectOptions{VersionID: o.VersionID})
	if err == nil {
		var info minio.ObjectInfo
		if info, err = obj.Stat(); err == nil {
			return obj, info, nil
		}
		obj.Close()
	}
	if o.VersionID != "null" || !o.IsLatest {
		return nil, minio.ObjectInfo{}, err
	}
	obj, err = c.GetObject(ctx, bucket, o.Key, minio.GetObjectOptions{})
	if err != nil {
		return nil, minio.ObjectInfo{}, err
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, minio.ObjectInfo{}, err
	}
	return obj, info, nil
}
