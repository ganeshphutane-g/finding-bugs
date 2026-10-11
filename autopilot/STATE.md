# State

Current project: #1 docker-mop (Docker CLI plugin: prune preview + volume rename/copy)
Phase: build (milestones 1–4 of 5 done)
Repo: building in projects/docker-mop/ inside finding-bugs (own repo blocked, see below)
Spec: projects/docker-mop/SPEC.md

## Next step

Milestone 5 (release):
1. README.md: what/why (link the two moby issues), install (Makefile + manual copy to
   ~/.docker/cli-plugins), quick start with real example output, command reference,
   safety guarantees and known limits (build cache not previewed; local volumes
   without driver options only; image totals are lower bounds), how tests work.
2. LICENSE (MIT, Ganesh Phutane), CHANGELOG.md (1.0.0), Makefile `dist` target that
   cross-builds linux/darwin/windows amd64+arm64 with checksums.
3. Full review for bugs and security issues (use the code-review approach on the
   whole project), fix findings, rerun all tests on both image stores.
4. Tag: since the project still lives in finding-bugs, tag `docker-mop/v1.0.0`
   (Go's nested-module tag form). If the owner has created the docker-mop repo by
   then, move the project there first (git subtree split keeps the history).
5. Mark the project done in STATE and start research round 2.

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
