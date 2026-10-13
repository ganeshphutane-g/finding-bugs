# Run log

Newest first.

## 2026-10-14 (IST) — run 3

- Owner has not created the docker-mop or bucketmove repos yet.
- Research round 3: chose pushdir (rclone watch mode; rclone#249, 213 👍, open since
  2015, labelled IMPORTANT; existing watchers have 0–10 stars). Runners-up recorded.
- pushdir milestones 1–2 and most of 3–4 built and tested with real rclone. Found
  and fixed while testing: rclone's --max-delete deletes up to the limit before
  stopping (so pushdir pre-counts), and a per-batch delete cap let slow mass
  deletions through (now a quiet period plus an hourly budget).

## 2026-10-13 (IST) — run 2

- Owner has not created the docker-mop repo yet; it stays in projects/docker-mop.
- Research round 2 (re-validated round-1 runners-up + new signals). Chosen project #2:
  bucketmove — leave MinIO (archived April 2026, images gone) without losing object
  versions, tags, locks or policies. Verified no version-preserving migration tool
  exists and that Garage silently lacks several features.
- Test servers working locally: MinIO built from source (4 drives), Garage,
  SeaweedFS, RustFS. Script: projects/bucketmove/scripts/test-servers.sh.
- Milestones 1–3 done: endpoints (URLs, env, mc aliases, MC_HOST_*), `inventory`,
  `check` (real feature probe + plain-words loss report, exit 3 on loss). Found on
  real servers: Garage has no versioning/tags/policy/object lock/ListObjectVersions
  and silently drops object tags on upload; MinIO has no per-bucket CORS.
- Milestones 4–5 done: `copy` (all versions oldest first, delete markers, headers,
  metadata, tags, retention, legal holds, lifecycle/policy/CORS/tags/default
  retention, journal with crash-safe resume, refuses on loss or existing buckets)
  and `verify` (version-by-version with SHA-256). Integration tests copy a seeded
  MinIO to Garage, SeaweedFS and RustFS and verify every version; resume and
  existing-bucket tests pass. Bugs found and fixed: objects written before
  versioning ("null" version) would have been copied as the newest version;
  SeaweedFS quirks (see STATE notes).
- Milestone 6 done: scale test (2,500 keys, 50-version key, 80 MiB multipart) on all
  targets; a Ctrl-C test through the CLI found a resume bug (an earlier version was
  adopted as a lost upload, silently dropping a version) — fixed, with a test that
  crashes at every point of a key's history. Two independent reviews: fixed secret
  leaks in URL errors, version order on RustFS, duplicate versions on retry, lock and
  default-retention handling, storage classes, journal repair, stricter verify,
  missing losses in check (existing bucket without lock, encryption/notifications/
  replication, unreadable settings), mc alias details, CLI interrupt handling.
  README with real output, LICENSE, CHANGELOG, `make dist`. Project #2 done.

## 2026-10-12 (IST) — run 1

- Research round 1 done (3 parallel searches); 13 candidates scored in IDEAS.md.
- Chosen: docker-mop, a Docker CLI plugin to preview prunes and rename/copy volumes
  (moby#30623 391 👍, moby#31154 576 👍, no established tool).
- Wrote SPEC.md. Milestone 1 done: dependency-free Engine API client (unix/TCP/TLS,
  DOCKER_HOST, contexts), plugin metadata, `docker mop version`, tests, CI, Makefile.
  Verified it installs and runs as a real `docker mop` plugin.
- Blocker: cannot create new repos (403); building in projects/docker-mop/ for now.
- Milestone 2 done: `docker mop plan` (containers, images, volumes, networks,
  system; until/label/label! filters; table and JSON). Integration tests compare the
  plan with Docker's real prune on the same state: 11 scoped cases + 8 unscoped +
  2 classic-builder cases, all identical on both the containerd and classic image
  stores. Bugs found and fixed on the way: hidden intermediate images, order-dependent
  parent handling, and containerd ignoring parent links.
- Found that Docker removes networks still used by stopped containers; the plan now
  warns about it.
- Milestone 3 done: `docker mop prune` shows the plan, asks (or --yes), removes
  exactly the planned objects without force, and skips anything that changed:
  a container started, a tag moved to a new image, a volume recreated or taken into
  use. docker-mop prune leaves the same state as Docker's prune in every case, on
  both image stores.
- Milestone 4 done: `docker mop volume copy|rename` with file-by-file verification
  (path, type, mode incl. setuid/sticky, owner, size, sha256, link targets, mtimes),
  label copy, --compose-project relabelling, and refusals for unsafe cases (in use,
  driver options, target exists). No image pulls: an empty local helper image and
  never-started containers. Tested with awkward data on both image stores.
- Milestone 5 done: two independent full reviews; fixed 6 issues in volume copy
  (race with a concurrently created target, hidden leftover helpers, stale Compose
  labels, xattr/dir-time checks, Ctrl-C leftovers, one fewer full read) and 9 in
  plan/prune (deleting an image that was tagged after the plan, config-only networks,
  until=0 and date parsing vs Docker, daemon clock/time zone, https without cert
  checks, docker global flags, API<1.42 volume rules, image edge cases). Added tests
  for each. README with real captured output, LICENSE, CHANGELOG, `make dist`.
  Full suite green on both image stores and in GitHub Actions. The v1.0.0 tag could
  not be pushed (git proxy refuses tags); noted as an owner step.
- Project #1 is done. Next run: research round 2.

## 2026-10-12 — setup

Playbook and state files created. Daily runs start after 12:30 AM IST.
Next: research for project #1.
