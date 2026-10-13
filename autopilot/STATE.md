# State

Current project: #4, Compose-aware PostgreSQL major-version upgrader (name to
be decided in the spec; "pgshift" is taken)
Phase: spec
Repo: projects/<name>/ inside finding-bugs until the owner creates repos.
Research: IDEAS.md, round 4.

## Next step

Write SPEC.md. Verify on real images (Docker Hub) before relying on any fact:
- the Postgres 18 image's PGDATA / volume layout change;
- what pg_upgrade needs (both versions' binaries; tianon images);
- the alpine vs debian collation/locale difference (dump/restore is the safe default
  across them);
- the PostgreSQL 14 end-of-life date on postgresql.org.

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
