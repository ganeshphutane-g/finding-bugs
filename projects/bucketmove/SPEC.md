# bucketmove: spec

## Problem

MinIO, the most popular self-hosted S3 server (61k stars), is archived: maintenance
mode from December 2025, archived in April 2026, and its images are gone from Docker
Hub. Its users are moving to Garage, SeaweedFS, RustFS, Ceph or a cloud provider.

The tools people use for the move (rclone, `mc mirror`) copy only the current version
of each object. Version history, delete markers, object tags, object lock, bucket
policies and lifecycle rules are silently left behind. Some targets (Garage) do not
support several of these at all, and people find out after the old server is gone.

## Who it is for

Anyone running MinIO for backups, app storage or data lakes who has to move off it
without losing history or settings by accident.

## What v1 does

One binary, `bucketmove`, with four commands. Endpoints are given as
`http(s)://ACCESS:SECRET@host:port[?region=...]` URLs, environment variables, or the
aliases already in the MinIO client config (`~/.mc/config.json`), so MinIO users can
type `--from myminio`.

- `bucketmove inventory --from SRC`: per bucket, the number of objects, old versions,
  delete markers and bytes, and which features are in use: versioning, object lock
  (default retention, objects under retention or legal hold), lifecycle rules, bucket
  policy, CORS, bucket and object tags, encryption settings, notifications.
- `bucketmove check --from SRC --to DST`: probes the target with a scratch bucket to
  learn what it really supports (not what it claims), then lists, per source bucket,
  what would be carried over and what would be **lost**, in plain words ("photos: 12,345
  old versions would be lost: the target does not support versioning").
- `bucketmove copy --from SRC --to DST [--bucket NAME ...]`: creates the buckets, applies
  the settings the target supports, and copies every version of every object oldest
  first, so the target's version history has the same order. Delete markers are
  recreated. Content type, user metadata, cache headers and object tags are copied.
  Large objects use multipart uploads. A journal makes the copy resumable: run it
  again and it continues where it stopped. It refuses to start when `check` finds a
  loss, unless `--accept-loss` is given.
- `bucketmove verify --from SRC --to DST`: compares every key's version history between
  source and target: same number of versions, same order, same delete markers, same
  size and SHA-256 of the content, same metadata and tags.

## Non-goals for v1

- Preserving version IDs or timestamps: S3 assigns them on upload. A mapping file
  (old version ID → new version ID) is written instead.
- Users, groups and access policies (IAM). MinIO's admin API differs from every
  target's; this is a candidate for v1.1, starting with a readable export.
- Live sync while the source is still being written to (copy can be re-run; verify
  shows what changed).
- Server-side encryption with customer-provided keys.

## Tech choices

- **Go** with the `minio-go` S3 client (Apache-2.0; it works with any S3 server and
  handles path-style addressing). No MinIO server code is used.
- **Real-server tests**: `scripts/test-servers.sh` starts MinIO (built from source,
  since its images are gone), Garage, SeaweedFS and RustFS locally. Integration tests
  seed a MinIO with versions, delete markers, tags, locks and policies, migrate it to
  each target, and verify.

## Milestones

1. **Skeleton**: Go module, endpoint parsing (URLs, env, mc aliases), S3 client,
   `bucketmove version`, test-server script, CI.
2. **Inventory**: per-bucket features and counts; table and JSON output.
3. **Check**: target capability probe and the loss report.
4. **Copy**: bucket settings, all versions in order, delete markers, metadata, tags,
   multipart, journal and resume, `--accept-loss`, version ID mapping file.
5. **Verify**: version-history comparison with content hashes.
6. **Release**: README with real output, LICENSE, CHANGELOG, full review, v1.0.0.
