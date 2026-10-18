# State

Current project: #8 pushprobe (a remote probe agent: runs checks inside other
networks and pushes results to Uptime Kuma push monitors and Gatus external
endpoints). Phase: build. Spec: projects/pushprobe/SPEC.md. Research round 9 is
in IDEAS.md.

## Next step

Milestones 1 and 2 are done (run 8), with tests:
- config, the http/tcp/dns/tls/disk/memory checks, `check` and `validate`;
- delivery (internal/deliver): Kuma and Gatus senders, a Route per check and
  target that keeps only the newest undelivered result, drops results older
  than the interval, backs off on transient errors, stops on permanent ones
  (wrong token, deleted monitor), and never puts a token in an error or log;
- `pushprobe run`: scheduler with a random start delay, SIGHUP reload (a bad
  file keeps the old configuration), `listen:` for /healthz and /metrics, one
  log line per change of state;
- a real-server test (TestRealServers): up and down in both Kuma and Gatus, and
  Kuma marking a silent push monitor down ("No heartbeat in the time window").
- kumafile's sio, kuma and testkit packages are copied in (internal/), with
  PUSHPROBE_* variable names; testkit also starts Gatus.

Milestone 3 is done (run 8):
- `pushprobe sync` (internal/kumasync). It creates the group and the missing
  push monitors, adopts a monitor by name in the group (taking its token for a
  ${VAR} without a value), and keeps the heartbeat interval at check interval +
  worst-case run + 10s. It never deletes or pauses, and keeps fields it does not
  manage. Tokens go to pushprobe.env (0600). Every command reads pushprobe.env
  next to the file (or --env-file); the environment wins, and an unreadable
  default env file is skipped (systemd passes it in). Tested against a real Kuma.
- the ping check (unprivileged ping sockets, else raw); it works as non-root in
  Docker;
- the exec check (needs allow_exec: true; no shell; only PATH, HOME, LANG and TZ
  are passed on);
- the Dockerfile (distroless, non-root, 17 MB; CI builds it, since `go mod
  download` cannot get through this sandbox's proxy, so it was tested here
  vendored);
- contrib/pushprobe.service (DynamicUser, CAP_NET_RAW only, hardening;
  `systemd-analyze verify` is clean).
- go.mod stays at go 1.24: x/net v0.50.0 and x/sys v0.41.0 are the newest that
  allow it. Use GOTOOLCHAIN=local when adding dependencies, because `go get`
  silently raised go.mod to 1.26 once.

Milestone 4 (release):
- README with a real demo (`check` output, a Kuma screenshot is not possible, so
  show the sync plan and the run log);
- CHANGELOG;
- two full reviews (correctness and delivery; security and usability), with
  findings fixed;
- CI green;
- mark it done in STATE and record it under Finished projects.

Tags: every Makefile now takes its version from `<project>/vX.Y.Z` tags only, so
the owner's v1.0.0 tags should be named that way (docker-mop/v1.0.0 already is).

Verified on 2026-10-18 (details in SPEC.md): the Kuma 2.5.6 push API (GET/POST,
404 for unknown tokens, any status accepted) and the Gatus external-endpoint API
(Bearer token, 401, at least one ordinary endpoint required). Still to check in
the tests: a missed push turning a Kuma push monitor down.

For a Kuma test server, use kumafile's internal/testkit approach (setup-database,
then the Socket.IO "setup" call); copy it into pushprobe. A Gatus test server needs
a config with at least one ordinary endpoint.

The Docker daemon here is 29.8.2 (API 1.56). Start it with `sudo nohup dockerd >
/tmp/claude-0/dockerd.log 2>&1 &`.

Uptime Kuma test server, should a later project need one: `docker run -d -p
127.0.0.1:3901:3001 louislam/uptime-kuma:2`, then POST /setup-database
{"dbConfig":{"type":"sqlite"}}, wait about 20 s, and call setup(user, pass)
until needSetup is false (an early call is lost while the server restarts). A
probe script with the bundled socket.io-client works inside the container
(`docker exec X node script.js`).

Gitea test server, should a later project need one: `docker run -d --name gt1 -p
127.0.0.1:3001:3000 -e GITEA__security__INSTALL_LOCK=true -e
GITEA__database__DB_TYPE=sqlite3 gitea/gitea:1.24`, then `docker exec -u git gt1
gitea admin user create --admin ...`. Use `--noproxy '*'` with curl for 127.0.0.1.
Forgejo cannot be pulled here (codeberg.org is blocked); it runs fine in GitHub
Actions as codeberg.org/forgejo/forgejo:16.

## Notes for runs

- The Docker daemon is not running at session start. Start it detached with
  `sudo nohup dockerd > /tmp/claude-0/dockerd.log 2>&1 &` and wait a few seconds
  (a plain `&` inside a compound command dies with the shell).
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

4. **pgcarry** (projects/pgcarry, v1.0.0 at commit df3dbb4). Safe major-version
   upgrades of Postgres in Docker Compose (docker-library/postgres#37).
   - **Commands:** plan; upgrade (dump from the old server restarted without a
     network, a matching new cluster in a new volume or folder, verified
     inventories, Compose edit by position, a cluster-identity check);
     `--no-switch` rehearsal; rollback (including recovery of a killed run);
     cleanup.
   - **Reviews:** two full reviews and one verification review; 19 + 7 findings
     fixed with tests.
   - **Tests:** Postgres 13–18, Debian/Alpine, pgvector, ICU, binds, user:,
     override files, and interrupts at every step.
   - **Owner checks before going public:** read the README; rehearse on a copy of a
     real app (`--no-switch`); CI green.

5. **issuemove** (projects/issuemove, v1.0.0 at commit e7c8051). Move issues between Gitea/Forgejo repositories, on one
   instance or across two (go-gitea/gitea#453, open since 2016, 79 👍).
   - **What it copies:** comments, labels, milestone, assignees, due date, state,
     attachments with relinked URLs, reactions, dependencies (both ways), and
     rewritten references.
   - **Authorship:** with an admin token, posts as the original authors (Sudo).
   - **Safety:** plan first; resumable without duplicates; markers trusted only
     from the token's user or the author.
   - **Reviews:** two full reviews (correctness; security/usability), with about 25
     findings fixed, each with a test.
   - **CI:** green on Gitea 1.24 and Forgejo 16.
   - **Owner checks before going public:** read the README; try `plan` and then a
     `move` of one test issue on their own instance.

6. **kumafile** (projects/kumafile, v1.0.0 at commit aa90445). Config as code for
   Uptime Kuma 2.x (uptime-kuma#1354, 91 👍; the JSON backup is gone in 2.0).
   - **Commands:** `export` (readable YAML, defaults left out, secrets in a 0600
     env file), `diff`, `apply` (dependency order, prune per section, moved
     monitors changed in place, the result checked) and `login` (token).
   - **How:** Kuma's own Socket.IO API, with a client written here, so it works
     remotely and with MariaDB.
   - **Reviews:** two full reviews (correctness; security/usability), with about 25
     findings fixed and regression tests.
   - **Tests:** a copy to an empty instance is exact; a second apply changes
     nothing; 2FA; 16 monitor types; tested on Kuma 2.0.0 and 2.5.6, locally and
     in CI.
   - **Owner checks before going public:** read the README; export your own
     instance and run `diff` (it should say in sync); try `apply` on a test
     instance first.

7. **docker-edit** (projects/docker-edit, v1.0.0 at commit cdc7019). Change an
   existing container's ports, env, mounts, restart policy, labels, image or
   networks (moby#21282, 50 👍; moby#8838, 104 👍).
   - **How:** the container is made again from its full `docker inspect`, with only
     the changes; a plan first; a swap with rollback (failure, unhealthy, Ctrl-C);
     image defaults are not frozen in; restart-only changes in place.
   - **Refuses** when something would be lost: changed files outside volumes,
     `--rm`, paused, Compose, `--network container:` and `--link` dependents, an
     image VOLUME left empty.
   - **Reviews:** two full reviews (correctness and data loss; security and
     usability), 20 findings, all fixed. Two only in part, the rest documented
     as limits: secrets in command lines are shown; --tlscacert/--tlscert/--tlskey
     are refused. Each fix has a regression test except the message for a
     failed rollback.
   - **Tests:** exact recreation of 9 container shapes, interrupts at every step,
     every edit; green on both image stores locally and in CI.
   - **Owner checks before going public:** read the README; try `--dry-run` and
     then an edit on a throwaway container on your own machine.

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
- Same for bucketmove, pushdir, pgcarry and issuemove (one empty private repo each,
  with the Claude app given access), if they should live on their own.
- Same for pushdir: an empty private repo `ganeshphutane-g/pushdir` with the Claude
  app given access lets the next run move it there (its README already uses that
  module path for `go install`).
- Pushing git tags is refused by this environment's git proxy (branch pushes work).
  To mark the release, create tag `docker-mop/v1.0.0` on commit 50c1a1e from the
  GitHub web UI (Releases > Draft a new release), or let the run tag it in the new
  repo once it exists.
