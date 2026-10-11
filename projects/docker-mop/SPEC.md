# docker-mop: spec

## Problem

`docker system prune`, `docker image prune -a` and `docker volume prune` delete things
with no way to see the list first. People have asked for a dry run since 2017
([moby#30623](https://github.com/moby/moby/issues/30623), 391 👍). Losing a volume by
accident means losing data.

Docker also cannot rename or copy a volume
([moby#31154](https://github.com/moby/moby/issues/31154), 576 👍). Renaming a Compose
project or tidying volume names means hand-written `docker run … cp -a` tricks that
need an image pulled from Docker Hub and give no check that the copy is complete.

## Who it is for

Anyone who runs Docker on a laptop, CI runner or small server and wants to free disk
space or reorganise volumes without fear of deleting the wrong thing.

## What v1 does

Installed as a Docker CLI plugin, so it runs as `docker mop …`.

- `docker mop plan [what] [flags]`: shows what the matching prune would remove,
  with sizes and the total space it would free. `what` is `system` (default),
  `containers`, `images`, `volumes` or `networks`. Flags mirror Docker's own:
  `-a/--all`, `--volumes` (for system), `--filter until=…`, `--filter label=…`,
  `--filter label!=…`, `--format table|json`.
- `docker mop prune [what] [flags]`: shows the plan, asks for confirmation (or
  `--yes`), then deletes **exactly the items shown**, one by one, and nothing else.
  Anything that came into use since the plan is skipped and reported, never forced.
- `docker mop volume copy SRC DST`: creates DST with the same driver and labels,
  copies all data, then verifies the copy file by file (path, type, mode, owner,
  size, content hash, link targets, times, file capabilities). Docker Compose labels
  are only set with `--compose-project`. Volumes with driver options are refused:
  their data can live outside Docker (NFS, bind devices, tmpfs), so a "copy" could
  point at the same storage.
- `docker mop volume rename SRC DST`: copy and verify, then remove SRC. Refuses if any
  container (running or stopped) uses SRC, and lists those containers.
- Works with `DOCKER_HOST`, `DOCKER_CONTEXT` and the current Docker context.

## Non-goals for v1

- Build cache. Docker exposes no way to predict exactly what `builder prune` removes,
  and a guess would break the "what you see is what gets deleted" promise.
- Swarm services, Podman, Windows containers.
- Volumes that use non-local drivers or driver options (refused with a clear
  message).

## Tech choices

- **Go, standard library only.** Talks to the Docker Engine API directly over the unix
  socket or TCP. One static binary, no dependency updates to chase, fast to start.
- **No image pulls.** Volume copy uses a tiny empty image created locally with
  `docker import` and the container archive API, so it works offline and is never
  hit by Docker Hub rate limits.
- **Real-daemon tests.** Integration tests run against a real Docker daemon and check
  the plan against what Docker's own prune actually deletes.

## Milestones

1. **Skeleton**: Go module, CLI plugin metadata, Engine API client (unix and TCP,
   `DOCKER_HOST`), `docker mop version`, unit tests, CI workflow.
2. **Plan**: `docker mop plan` for containers, images, volumes and networks, with
   filters, sizes, table and JSON output. Integration tests compare the plan with a
   real prune on the same state.
3. **Prune**: confirmation, deleting exactly the planned items, skip-and-report for
   items that changed, a summary of space freed.
4. **Volume copy and rename**: helper image, archive copy, verification, rename
   safety checks, label and option preservation.
5. **Release**: Docker context support, README, install instructions, Makefile
   cross-builds, CHANGELOG, full review, `v1.0.0` tag.
