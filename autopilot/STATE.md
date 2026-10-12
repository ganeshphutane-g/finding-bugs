# State

Current project: #2 bucketmove (leave MinIO safely: inventory, target check, copy all versions, verify)
Phase: build (milestones 1–5 of 6 done)
Repo: projects/bucketmove/ inside finding-bugs (own repos blocked, see below)
Spec: projects/bucketmove/SPEC.md

## Next step

Milestone 6 (release): README with real captured output (inventory, check against
Garage and RustFS, copy --verify, resume), LICENSE (MIT, Ganesh Phutane), CHANGELOG,
`make dist`; a large-volume test (a few thousand keys, a >64 MiB object, many
versions of one key) and a Ctrl-C/resume test through the CLI; then two
independent full reviews (copier/journal/resume correctness and data-loss paths
first; endpoint secrets handling) and fix findings; run the suite twice; mark done.

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
- Docker Hub pulls can fail with 429 (rate limit). Tests must not depend on pulling
  images: build tiny test images locally (e.g. `FROM scratch` with a static file).

## Finished projects

1. **docker-mop** (projects/docker-mop, v1.0.0 at commit 50c1a1e). Docker CLI plugin:
   `plan`/`prune` (exact prune preview, verified against Docker's real prune on both
   image stores) and verified `volume copy`/`rename`. Before making it public the
   owner should: read the README, try `make install` and `docker mop plan` on their
   own machine, check that GitHub Actions ran green on the push, and decide whether
   to give it its own repo (recommended, so `go install` and releases work).

## Blockers for the owner

- Creating new repos fails: the Claude GitHub app gets "403 Resource not accessible
  by integration". To give docker-mop its own repo, create an empty private repo
  `ganeshphutane-g/docker-mop` on GitHub and give the Claude GitHub app access to it
  (GitHub settings > Applications > Claude > Repository access). The next run will
  move the project there with its history.
- Pushing git tags is refused by this environment's git proxy (branch pushes work).
  To mark the release, create tag `docker-mop/v1.0.0` on commit 50c1a1e from the
  GitHub web UI (Releases > Draft a new release), or let the run tag it in the new
  repo once it exists.
