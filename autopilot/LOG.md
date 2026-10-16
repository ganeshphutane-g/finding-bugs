# Run log

Newest first.

## 2026-10-17 (IST) — run 6

- Project #6 kumafile, from spec to v1.0.0 in one run.
  - Research re-checked: uptime-kuma#1354 has 91 👍 and #6045 has 28 👍. 2.x has
    no backup and no REST API for config, so the tool talks to Kuma over its own
    Socket.IO API.
  - Name: "kumactl" is taken (Kong).
  - Milestones:
    - a Socket.IO client;
    - login (password, TOTP, JWT);
    - export with a baseline, so files only list what differs from the web
      interface's defaults;
    - load and diff;
    - apply, with a check of the result.
- Two full reviews against real servers found about 25 problems, all fixed with
  tests. The worst:
  - an apply that hung forever on Kuma 2.0.0;
  - secrets missed for many notification providers, and printed by diff;
  - no timeouts;
  - a file could read any environment variable (now only KUMA_*);
  - prune too broad;
  - export overwriting files;
  - YAML that could not be read back;
  - Kafka SASL, custom links, logos, paused groups and null tags.
- The README shows a real demo run; CHANGELOG written.
- Tests pass locally and in CI (aa90445) on Kuma 2.0.0 and the latest 2.x.
- Research round 7 started at the end of the run; if it is not in IDEAS.md,
  redo it.
- Next: research round 7 (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - v1.0.0 tags have to be made in the web UI;
  - optionally, give projects their own repos.

## 2026-10-16 (IST) — run 5

- issuemove (project #5) built and finished:
  - milestones 1–3: plan, copy, resume, references, dependencies, cross-instance;
  - CI on Gitea 1.24 and the official Forgejo 16 image (the tests were confirmed to
    really run in the Forgejo job).
- Two full reviews: correctness/data loss, and security/usability (run against real
  Gitea containers). Fixed, each with a regression test:
  - fake markers planted in the target or the source could divert a move;
  - labels, milestone and assignees were silently dropped when the author could not
    write to the target;
  - a rerun could close copies, lost reactions and reverted attachment links;
  - authors who could not see a private target made the move fail halfway;
  - a shared token was sent to both instances;
  - a mistyped `--label` selected every issue;
  - cross-instance name matching could be forged (now opt-in with `--match-users`,
    and mentions are quietened);
  - plus redirects, retries, Ctrl-C at the prompt, the plan wording and exit codes.
- I checked by mutation that the new tests fail when each fix is removed.
- README from a real demo run (acme/website -> acme/docs), CHANGELOG, Roadmap.
- Research round 6 done: 6 candidates scored in IDEAS.md.
  - Chosen: config as code for Uptime Kuma v2 (uptime-kuma#1354, 91 👍; the
    maintainers have declined a readable export), provisionally; the evidence is to
    be re-checked before the spec.
  - Runner-up: Nginx Proxy Manager → Caddy/Traefik.
- Next: spec for project #6 (see STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - the v1.0.0 tags have to be created in the web UI (the git proxy refuses tags);
  - optionally, create own repos for the projects.

## 2026-10-15 (IST) — run 4

- Found that the finding-bugs repo is PUBLIC (the owner thought it was private).
  Recorded under owner blockers; visibility not changed (playbook rule).
- pgcarry milestones 1–4 done and pushed:
  - plan, the upgrade rehearsal, the switch with a by-position Compose edit,
    rollback, cleanup;
  - a version matrix 13→17, 14/15/16/17→18, Alpine, pgvector, ICU, a password
    file, trust auth.
- Found by testing:
  - 18 catalogs NOT NULL constraints;
  - pending ALTER SYSTEM settings live only in postgresql.auto.conf;
  - pg_isready makes no attempt without a user name under an arbitrary uid;
  - a service that only uses the postgres image for tools was taken for the
    database.
- Two independent reviews found 19 issues. All confirmed ones are fixed with tests.
  The serious ones:
  - writes from undeclared clients were lost (now the dump comes from the old server
    restarted without a network);
  - -f from another folder or different files copied the wrong data (now the project
    comes from the container's labels, and the mount is checked);
  - databases with datallowconn=false were silently skipped;
  - volumes with driver_opts were recreated without them;
  - PGPASSWORD was visible in ps and in errors;
  - a database named "dbname=x" redirected pg_dump;
  - SIGHUP or kill -9 left everything down (now the manifest comes first and
    rollback recovers);
  - list settings were mangled;
  - the wrong backup was picked by name.
- Measured: a 1.1 GB database rehearses in 51 s (dump 14 s, restore 21 s, verify
  2 s).
- A verification review confirmed all 11 fixes. It found 3 new high-severity
  issues in the new code, now fixed with tests:
  - starting the service by hand during the dump put two servers on one data folder
    (now detected and stopped, and the plan puts "run pgcarry rollback" first after
    a crash);
  - a rehearsal copy the user had switched to could be deleted (now it is replaced
    only if its cluster ID, last checkpoint and clean-shutdown state are
    unchanged);
  - recovery could remove a bind copy in use (now it checks every container's
    mounts and only acts after the old service serves).

  Also fixed: a lock for two commands on one upgrade, the Created flag being
  recorded late, env files, temp files and a hint's SQL.
- pgcarry v1.0.0 finished at df3dbb4; the full suite with -race is green locally.
- CI green on df3dbb4.
- Research round 5 done. Chosen project #5: a Gitea/Forgejo issue mover (gitea#453,
  open since 2016, no tool exists). The restic pull orchestrator dropped to 19/25:
  Zerobyte now backs up SFTP sources, and pull adds little over append-only.
- Next run: verify the Gitea/Forgejo API facts on real containers, write SPEC.md,
  start building.

## 2026-10-14 (IST) — run 3

- Owner has not created the docker-mop or bucketmove repos yet.
- Research round 3: chose pushdir (rclone watch mode; rclone#249, 213 👍, open since
  2015, labelled IMPORTANT; existing watchers have 0–10 stars). Runners-up recorded.
- pushdir milestones 1–2 and most of 3–4 built and tested with real rclone. Found
  and fixed while testing: rclone's --max-delete deletes up to the limit before
  stopping (so pushdir pre-counts), and a per-batch delete cap let slow mass
  deletions through (now a quiet period plus an hourly budget).
- pushdir finished (v1.0.0). Two independent reviews found real bugs, all fixed
  with tests: any rclone filter after `--` broke every batch (rclone refuses filters
  with --files-from-raw; batches are now chosen by an rclone listing with the user's
  rules in rclone's order, checked against `rclone copy` for 14 combinations);
  replacing the watched folder wiped the remote (now pushdir stops); a look-alike
  file could be "renamed" over (now hash-checked); renames skipped --backup-dir;
  atomic saves used up the delete budget; a blocked event queue could hang shutdown
  and lose new folders after an overflow; files with future timestamps spun the CPU;
  passwords in connection strings were logged and served on /status; overlapping or
  relative destinations were accepted; unknown remotes retried forever; a second
  Ctrl-C orphaned rclone. Docker image checked end to end; systemd unit untested.
- Research round 4 (two parallel searches; key counts re-checked on GitHub). Chosen
  project #4: pgcarry, a Compose-aware PostgreSQL major-version upgrader
  (docker-library/postgres#37, 278 👍, open since 2014; the 18 image's own error
  points there). SPEC.md written; Postgres image layouts verified on real images.
  Runner-up for round 5: restic pull-mode orchestrator (restic#299, 333 👍).

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
