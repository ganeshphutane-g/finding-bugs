# State

Current project: #1 docker-mop (Docker CLI plugin: prune preview + volume rename/copy)
Phase: build (milestone 1 of 5 done)
Repo: building in projects/docker-mop/ inside finding-bugs (own repo blocked, see below)
Spec: projects/docker-mop/SPEC.md

## Next step

Milestone 2: `docker mop plan` for containers, images, volumes and networks, with
filters (until, label, label!=), sizes, table and JSON output. Integration tests must
create real objects, run the plan, then run Docker's real prune and compare the
deleted IDs with the plan. Skip integration tests cleanly when no daemon is reachable.

## Notes for runs

- The Docker daemon is not running at session start. Start it with
  `sudo dockerd > /tmp/dockerd.log 2>&1 &` and wait a few seconds.
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
