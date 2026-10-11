# State

Current project: #1 docker-mop (Docker CLI plugin: prune preview + volume rename/copy)
Phase: build (milestones 1–2 of 5 done)
Repo: building in projects/docker-mop/ inside finding-bugs (own repo blocked, see below)
Spec: projects/docker-mop/SPEC.md

## Next step

Milestone 3: `docker mop prune [what] [flags]`. Show the plan, ask for confirmation
(or --yes / -y / -f), then delete exactly the planned items one by one in Docker's
order (containers, networks, volumes, images), never with force. Skip and report
anything that is now in use or gone (409/404). For images use DELETE
/images/{id}?noprune=false without force; for "only untagged" items untag each tag.
Report space freed. Integration tests: plan then prune leaves exactly the same
state as Docker's own prune would (reuse the world() fixture), and a race test where
an object comes into use between plan and prune.

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
