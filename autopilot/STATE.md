# State

Current project: #6 kumafile (config as code for Uptime Kuma 2.x: export, diff,
apply)
Phase: build. Spec: projects/kumafile/SPEC.md (written in run 6).
Repo: projects/kumafile/ inside finding-bugs (creating repos is blocked).

## Next step

Milestone 1 is done (run 6):
- internal/sio is a Socket.IO client;
- internal/kuma does login (password, TOTP, JWT, no auth) and reads all lists;
- `kumafile export` writes YAML plus a 0600 .env with the secrets;
- tests pass on Kuma 2.0.0 and 2.5.6.

Milestone 2 is done (run 6):
- the baseline (what a UI-created monitor looks like, plus per-type tweaks;
  internal/config/defaults.go) trims the export;
- Load reads a file and expands ${NAME} ($${ is a literal);
- Diff compares the file with the instance, which is loaded through its own
  export, so both sides look alike; `kumafile diff` exits 0 in sync, 3 on
  differences;
- references are checked; secrets are never printed.

Milestone 3 (apply) is built (run 6):
- `kumafile apply [FILE] [--prune] [-y]` creates and updates in dependency order
  and deletes in reverse;
- it re-reads the instance afterwards and reports anything the server did not
  take;
- the round trip A -> B is exact; a second apply changes nothing; prune, 16
  monitor types and 2FA (the token is reused for the check) are tested.
Two full reviews (correctness; security/usability) were started at about 01:20
IST on 2026-10-17. If their findings are not in LOG.md, re-run them.

Then milestone 4: fix the review findings, a README from a real demo, CHANGELOG,
CI green on 2.0.0 and 2.x, v1.0.0.

Notes from building apply:
- saveStatusPage(slug, config, imgDataUrl, groups) needs imgDataUrl = config.icon
  (null crashes the server).
- statusPageList is not re-pushed after addStatusPage: read state on a fresh
  connection.
- Notification settings are a JSON text on the server.
- kafkaProducerSaslOptions is exported as JSON text when it is a secret.

Facts checked on Uptime Kuma 2.5.6 (2026-10-17):
- No REST API for config. Socket events:
  - monitors: add, editMonitor, deleteMonitor(id, deleteChildren);
  - tags: addTag, editTag, deleteTag, addMonitorTag/editMonitorTag/deleteMonitorTag
    (tagID, monitorID, value);
  - notifications: addNotification(obj, id|null), deleteNotification;
  - maintenance: addMaintenance, editMaintenance, addMonitorMaintenance,
    addMaintenanceStatusPage, deleteMaintenance;
  - status pages: saveStatusPage, addStatusPage, deleteStatusPage;
  - addProxy/deleteProxy, addDockerHost/deleteDockerHost,
    addRemoteBrowser/deleteRemoteBrowser.
- Login: `login {username,password,token}` returns {ok, token(JWT)} or
  {tokenRequired}; `loginByToken(jwt)`. A fresh server needs `POST
  /setup-database {"dbConfig":{"type":"sqlite"}}`, then `setup(user, pass)`.
- After login the server pushes monitorList, maintenanceList, notificationList
  (config is a JSON string), proxyList, dockerHostList, apiKeyList,
  remoteBrowserList and statusPageList.
- A monitor object has 125 fields. Secret fields come after the
  `includeSensitiveData` marker in Monitor.toJSON. Server-derived fields: id, path,
  pathName, childrenIDs, weight, maintenance, forceInactive, dns_last_result,
  includeSensitiveData.
- editMonitor copies 116 named fields; notificationIDList, accepted_statuscodes and
  active are handled separately (pause/resume for active).
- The JSON backup is gone in 2.x: no settings page and no server handler.
- A test server: `docker run -d -p 127.0.0.1:3901:3001 louislam/uptime-kuma:2`, then
  POST setup-database and wait about 15 s. A probe script with the bundled
  socket.io-client works inside the container (node).

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
