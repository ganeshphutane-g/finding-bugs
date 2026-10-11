# State

Current project: #1 docker-mop (Docker CLI plugin: prune preview + volume rename/copy)
Phase: spec
Repo: ganeshphutane-g/docker-mop (to be created, private)

## Next step

Create the private repo, write SPEC.md with milestones, then start milestone 1.
See autopilot/IDEAS.md for the evidence behind this choice.

## Notes for runs

- The Docker daemon is not running at session start. Start it with
  `sudo dockerd > /tmp/dockerd.log 2>&1 &` and wait a few seconds.
- Docker Hub pulls can fail with 429 (rate limit). Tests must not depend on pulling
  images: build tiny test images locally (e.g. `FROM scratch` with a static file).

## Finished projects

None yet.

## Blockers for the owner

None.
