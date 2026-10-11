# State

Current project: #1 docker-mop (Docker CLI plugin: prune preview + volume rename/copy)
Phase: build (milestones 1–3 of 5 done)
Repo: building in projects/docker-mop/ inside finding-bugs (own repo blocked, see below)
Spec: projects/docker-mop/SPEC.md

## Next step

Milestone 4: `docker mop volume copy SRC DST` and `docker mop volume rename SRC DST`.
Plan (copy approach already proven by hand on this daemon):
- Helper image: `docker import` of an empty tar (POST /images/create?fromSrc=-),
  tagged e.g. docker-mop-helper:1, created on demand. No registry pulls.
- Create (never start) a container from it with SRC at /from, another with DST at
  /to (Cmd ["/nonexistent"] is fine because it never runs). Stream
  GET /containers/{a}/archive?path=/from/. into PUT /containers/{b}/archive?path=/to.
- Verify: read both archives back and compare every entry (path, type, mode,
  uid/gid, size, sha256 of content, symlink target). Remove helper containers after.
- DST gets SRC's driver, driver options and labels (refuse non-local drivers);
  refuse if DST exists unless --into-existing (must be empty).
- rename = copy + verify + remove SRC; refuse if any container (running or stopped)
  uses SRC and list them. Optional --compose-project to rewrite
  com.docker.compose.project/volume labels.
- Test with non-root-owned files, symlinks, empty dirs, a large file, and hard links.

## Notes for runs

- The Docker daemon is not running at session start. Start it with
  `sudo dockerd > /tmp/dockerd.log 2>&1 &` and wait a few seconds.
- Integration tests: `MOP_INTEGRATION=1 go test ./...` (label-scoped, safe) or
  `MOP_INTEGRATION=destructive` (unscoped; only on a throwaway daemon like this one).
- The classic image store prunes differently (parent images). To test it, stop
  dockerd and start one with `--feature containerd-snapshotter=false` and a separate
  `--data-root`; the classic-builder tests (DOCKER_BUILDKIT=0) only bite there.
- Docker Hub pulls can fail with 429 (rate limit). Tests must not depend on pulling
  images: build tiny test images locally (e.g. `FROM scratch` with a static file).

## Finished projects

None yet.

## Blockers for the owner

- Creating new repos fails: the Claude GitHub app gets "403 Resource not accessible
  by integration". To give docker-mop its own repo, create an empty private repo
  `ganeshphutane-g/docker-mop` on GitHub and give the Claude GitHub app access to it
  (GitHub settings > Applications > Claude > Repository access). The next run will
  move the project there with its history.
