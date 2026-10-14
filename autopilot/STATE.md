# State

Current project: #4 pgcarry (safe major-version upgrades of Postgres in Docker Compose)
Phase: build, milestone 2 of 5 next
Repo: projects/pgcarry/ inside finding-bugs (own repos blocked, see below)
Spec: projects/pgcarry/SPEC.md. Research: IDEAS.md round 4.

## Next step

Milestone 1 is done: `pgcarry plan` covers
- finding the database service and its data mount (named volume or bind mount);
- reading the cluster (databases, sizes, extensions, locales, roles, checksums, md5
  passwords, slots, tablespaces, settings from the data folder, custom pg_hba
  rules);
- the target image (per-family tag rules, a probe with tmpfs, the extensions it
  has, locales tested by CREATE DATABASE);
- the disk check and the blockers.

The tests use real images (PGCARRY_INTEGRATION=1; about 60 s).

Milestone 2: `upgrade --no-switch`:
- stop the dependent services;
- pg_dumpall --globals-only, then pg_dump -Fc per database, using the new image's
  client against the old server, through a temporary container on the service's
  network or `docker exec` into a helper;
- check the dumps (pg_restore --list) and write checksums to a backup folder;
- stop the old service;
- create the new volume (or folder) with Compose labels;
- initdb through the image with the same encoding, locale and provider as
  template1, with the checksums choice;
- restore the globals, then each database;
- carry the custom settings and pg_hba rules;
- verify row counts, sequences and object lists, writing a journal so it can be
  resumed.

Facts checked on real images (2026-10-14): the 14 and 17 images use PGDATA
/var/lib/postgresql/data (VOLUME the same); 18 and 18-alpine use
/var/lib/postgresql/18/docker (VOLUME /var/lib/postgresql). Running 18 on an old
/var/lib/postgresql/data mount refuses and points to docker-library/postgres#37;
running 17 on 14 data fails "database files are incompatible". The
tianon/postgres-upgrade:14-to-18 image has both binaries (Debian only, no extensions),
so dump/restore with the user's own images is the v1 method. Docker 29.8.2 and
Compose v5.6.0 are available here; postgres images pull from Docker Hub.

## Notes for runs

- The Docker daemon is not running at session start. Start it with
  `sudo dockerd > /tmp/dockerd.log 2>&1 &` and wait a few seconds.
- Integration tests: `MOP_INTEGRATION=1 go test ./...` (label-scoped, safe) or
  `MOP_INTEGRATION=destructive` (unscoped; only on a throwaway daemon like this one).
- The classic image store prunes differently (parent images). To test it, stop
  dockerd and start one with `--feature containerd-snapshotter=false` and a separate
  `--data-root`; the classic-builder tests (DOCKER_BUILDKIT=0) only bite there.
- SeaweedFS quirks found: it lists versions out of key order (verify falls back to
  per-key lookups), guesses Content-Type for application/octet-stream objects in
  unversioned buckets (probe "exact content type"; reported as a change), stores
  "aws-chunked" as Content-Encoding for streaming-signed uploads (copier sends
  plain MD5-checked uploads instead), and needs -volume.max raised for many buckets.
- bucketmove servers: `scripts/test-servers.sh up /tmp/test-servers.env`, then
  `set -a; . /tmp/test-servers.env; set +a; BUCKETMOVE_INTEGRATION=1 go test ./...`.
  MinIO runs with 4 drives (versioning/object lock need erasure coding) and
  MINIO_CI_CD=1. Garage has no versioning, tags, policy, object lock and no
  ListObjectVersions; MinIO has no per-bucket CORS. SeaweedFS and RustFS support all.
- bucketmove test servers: MinIO is not on Docker Hub or reachable registries any
  more; build it with `GOBIN=/tmp/minio-bin go install
  github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c` (about 2.5 minutes).
  Garage (dxflrs/garage), SeaweedFS (chrislusf/seaweedfs) and RustFS (rustfs/rustfs)
  pull from Docker Hub.
- pushdir tests need rclone: `GOBIN=/tmp/claude-0/rclone-bin go install
  github.com/rclone/rclone@v1.75.2` then `RCLONE_BIN=/tmp/claude-0/rclone-bin/rclone
  make test`. Docker builds here cannot reach the Go proxy: to check the image, copy
  the project to a temp folder, run `go mod vendor`, and build with `-mod=vendor`.
- Docker Hub pulls can fail with 429 (rate limit). Tests must not depend on pulling
  images: build tiny test images locally (e.g. `FROM scratch` with a static file).

## Finished projects

1. **docker-mop** (projects/docker-mop, v1.0.0 at commit 50c1a1e). Docker CLI plugin:
   `plan`/`prune` (exact prune preview, verified against Docker's real prune on both
   image stores) and verified `volume copy`/`rename`. Before making it public the
   owner should: read the README, try `make install` and `docker mop plan` on their
   own machine, check that GitHub Actions ran green on the push, and decide whether
   to give it its own repo (recommended, so `go install` and releases work).
2. **bucketmove** (projects/bucketmove, v1.0.0). Leave MinIO safely: `inventory`,
   `check` (real feature probe + loss report), `copy` (all versions in order, delete
   markers, metadata, tags, locks, settings; crash-safe resume), `verify` (version by
   version with SHA-256). Tested against MinIO, Garage, SeaweedFS and RustFS; two full
   reviews fixed. Owner checks before going public: README, try it against their own
   MinIO with a test bucket, GitHub Actions green.

3. **pushdir** (projects/pushdir, v1.0.0 at commit 9d521f9). Watch a folder and push every change to
   any rclone remote within seconds (rclone#249). Batches with settle checks, filters
   applied by rclone's own rules, opt-in deletions with an hourly budget and
   unmount/empty/replaced-folder guards, hash-checked remote-side renames, retries,
   /status and /metrics, systemd unit, Docker image (checked end to end). Two full
   reviews (data loss; ops/security): 9 + 12 confirmed findings fixed, each with a
   test. Owner checks before going public: README, run it on a real folder for a day,
   try the systemd unit (not tested on a real systemd host), GitHub Actions green.

## Blockers for the owner

- The finding-bugs repo itself is PUBLIC (seen 2026-10-15). Everything in it
  (projects, logs, research notes) is visible to anyone. The playbook rules forbid
  changing visibility, so the owner decides: make it private (GitHub > Settings >
  Danger Zone), or keep it public knowingly.

- Creating new repos fails: the Claude GitHub app gets "403 Resource not accessible
  by integration". To give docker-mop its own repo, create an empty private repo
  `ganeshphutane-g/docker-mop` on GitHub and give the Claude GitHub app access to it
  (GitHub settings > Applications > Claude > Repository access). The next run will
  move the project there with its history.
- Same for pushdir: an empty private repo `ganeshphutane-g/pushdir` with the Claude
  app given access lets the next run move it there (its README already uses that
  module path for `go install`).
- Pushing git tags is refused by this environment's git proxy (branch pushes work).
  To mark the release, create tag `docker-mop/v1.0.0` on commit 50c1a1e from the
  GitHub web UI (Releases > Draft a new release), or let the run tag it in the new
  repo once it exists.
