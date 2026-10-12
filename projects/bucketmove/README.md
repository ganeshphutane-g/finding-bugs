# bucketmove

Move your S3 buckets off MinIO (or any S3 server) **without losing object versions
or settings by accident**. bucketmove shows what your buckets use, tests what the new
server really supports, tells you in plain words what would be lost, copies every
version of every object in order, and then proves the copy is identical.

MinIO's open-source server is archived (maintenance mode from December 2025, archived
in April 2026) and its images are no longer published, so many people are moving to
Garage, SeaweedFS, RustFS, Ceph or a cloud provider. The usual tools for the move
(`rclone`, `mc mirror`) copy only the current version of each object. Old versions,
delete markers, object tags, locks and bucket policies stay behind, and some targets
cannot store them at all. Usually you find out after the old server is gone.

## What it does

```console
$ bucketmove inventory --from oldminio
oldminio: 4 buckets, 13 objects, 6 old versions, 2 delete markers, 19.0 MiB in all versions.

BUCKET           OBJECTS  OLD VERSIONS  DELETE MARKERS  SIZE (ALL VERSIONS)  FEATURES
demo-configured  2        0             0               12 B                 1 lifecycle rule, bucket policy, bucket tags
demo-history     5        6             2               19.0 MiB             versioning, 1 tagged
demo-locked      2        0             0               12 B                 versioning, object lock (default GOVERNANCE 1 days), 2 under retention, 1 on legal hold
demo-plain       4        0             0               4.0 KiB              1 with metadata
```

`check` tries every feature on the target with a scratch bucket (it does not trust
what the server claims) and says what each bucket would lose:

```console
$ bucketmove check --from oldminio --to garage
garage supports: user metadata and headers, exact content type, multipart upload, unicode and special keys, lifecycle rules, CORS.
It does not support:
  - versioning (Unimplemented action: PutBucketVersioning)
  - delete markers (needs versioning)
  - object tags (Unimplemented action: GetObjectTagging)
  - storage classes (an object stored as REDUCED_REDUNDANCY came back as STANDARD)
  - bucket tags (Unimplemented action: PutBucketTagging)
  - bucket policy (Unimplemented action: PutBucketPolicy)
  - object lock (Unimplemented action: GetObjectLockConfiguration)
  - object retention (Unimplemented action: GetObjectLockConfiguration)
  - legal hold (Unimplemented action: GetObjectLockConfiguration)

demo-configured: WOULD LOSE DATA OR SETTINGS
  - bucket tags would be lost
  - the bucket policy would be gone (check who can read and write the bucket)
demo-history: WOULD LOSE DATA OR SETTINGS
  - 6 old versions and 2 delete markers would be lost: the target keeps only the latest version of each object
  - object tags on 1 version would be lost
demo-locked: WOULD LOSE DATA OR SETTINGS
  - versioning would be off: future overwrites and deletes could not be undone
  - object lock would be gone: nothing would stop objects being deleted or overwritten
  - 2 versions under retention would lose it
  - 1 legal hold would be dropped
...
4 of 4 buckets would lose something. 'bucketmove copy' refuses to start unless you pass --accept-loss.
```

`copy` copies every version, oldest first, so the target's version history has the
same order. `--verify` then compares each bucket version by version, content hashes
included:

```console
$ bucketmove copy --from oldminio --to rustfs --verify
Copying 4 buckets from oldminio to rustfs (journal: .bucketmove/127.0.0.1_19000-to-127.0.0.1_19100-04e2fee7)
demo-configured: 2 versions and 0 delete markers copied (12 B), 0 already done; settings: lifecycle, policy, bucket tags, in 100ms.
  verified: 2 keys, 2 versions identical.
demo-history: 11 versions and 2 delete markers copied (19.0 MiB), 0 already done, in 600ms.
  verified: 6 keys, 13 versions identical.
demo-locked: 2 versions and 0 delete markers copied (12 B), 0 already done; settings: default retention, in 100ms.
  verified: 2 keys, 2 versions identical.
demo-plain: 4 versions and 0 delete markers copied (4.0 KiB), 0 already done, in 100ms.
  verified: 4 keys, 4 versions identical.
```

### Commands

| Command | What it does |
| --- | --- |
| `inventory --from SRC` | Per bucket: objects, old versions, delete markers, sizes, and the features in use (versioning, object lock, retention, legal holds, lifecycle, policy, CORS, tags, metadata, storage classes, encryption, notifications, replication). |
| `check --from SRC --to DST` | Probes the target and lists what each bucket would lose. Exits with 3 if anything would be lost. |
| `copy --from SRC --to DST` | Copies buckets with all versions in order, delete markers, headers, user metadata, tags, storage classes, retention, legal holds, lifecycle, policy, CORS, bucket tags and default retention, as far as the target supports them. Refuses if `check` finds a loss, unless you pass `--accept-loss`. |
| `verify --from SRC --to DST` | Compares buckets version by version: keys, version count and order, delete markers, SHA-256 of the content, headers, metadata, tags, storage class, retention and legal holds. Exits with 1 on any difference. `--quick` compares sizes instead of content. |

All commands take `--bucket NAME` (repeatable or comma-separated) and most take
`--format json`.

### Safety

- **Resumable.** A journal per bucket records each version as it is copied. Run the
  same `copy` command again after a crash, Ctrl-C or network failure and it continues
  where it stopped. It recognises an upload the earlier run finished but did not get
  to record, so versions are neither skipped nor duplicated. Tests crash a copy at
  every point in a key's history and check the result.
- **Refuses rather than guesses.** It will not copy into a bucket it did not create
  unless you pass `--into-existing`, will not run when source and target are the same
  server, and will not continue past a loss without `--accept-loss`.
- **Verifies, then lets you check.** `verify` works on its own, so you can re-run it
  at any time, for example just before switching your applications over.

## What the tests found on real servers

The integration tests run against real servers: MinIO (built from source at commit
7aac2a2c5b7c, the newest the Go module proxy offers), Garage v2.1.0, SeaweedFS 4.48
and RustFS 1.0.1. On those versions:

| | Garage | SeaweedFS | RustFS |
| --- | --- | --- | --- |
| Versioning and delete markers | no | yes | yes |
| Object lock, retention, legal hold | no | yes | yes |
| Object and bucket tags | no (object tags are silently dropped on upload) | yes | yes |
| Bucket policy | no | yes | yes |
| Storage classes | no | no | no |
| Notes | answers "NoSuchBucket" when a bucket simply has no CORS or lifecycle rules | guesses Content-Type for `application/octet-stream` objects and drops `Content-Encoding` in unversioned buckets; lists versions out of key order | lists delete markers before versions |

`check` and `verify` handle each of these quirks, and `check` reports them as losses
or changes. Treat the table as a snapshot: run `bucketmove check` against your own
target.

## Endpoints and credentials

An endpoint can be:

- **An `mc` alias** from `~/.mc/config.json` (or `MC_CONFIG_DIR`), or an
  `MC_HOST_<alias>` variable, exactly as the MinIO client uses them:
  `--from myminio`.
- **A URL**: `--to https://s3.example.com:9000`, with the keys in
  `BUCKETMOVE_TO_ACCESS_KEY` and `BUCKETMOVE_TO_SECRET_KEY` (and `BUCKETMOVE_FROM_*`
  for the source). Add `?region=garage` if the server needs a region.

`https://KEY:SECRET@host` also works, but keys on the command line are visible to other
users of the machine and stay in shell history. Secrets are never printed.

## Install

With Go 1.25 or newer:

```sh
git clone https://github.com/ganeshphutane-g/bucketmove
cd bucketmove
make build        # ./bucketmove
```

`make dist` builds static binaries for Linux, macOS and Windows.

## Limits

- **Version IDs and dates are new.** S3 assigns them on upload. The journal
  (`.bucketmove/…/<bucket>.jsonl`) maps each old version ID to the new one.
- **Not copied:** users, groups and access keys (IAM), default encryption,
  notifications and replication settings. `check` lists these so you can set them up
  on the target. Objects encrypted with customer-provided keys (SSE-C) cannot be read
  and fail the copy.
- **COMPLIANCE retention is copied as is.** On the target it cannot be shortened or
  removed by anyone until it expires, just like on the source.
- **Huge buckets:** every journal entry is held in memory while a bucket is copied,
  and `verify` on servers that list keys out of order looks each key up separately,
  which is slower.

## Development

```sh
make test                       # unit tests
make servers                    # start MinIO, Garage, SeaweedFS, RustFS locally (needs Docker and Go)
make integration                # tests against them
make servers-down
```

## License

MIT. See [LICENSE](LICENSE).
