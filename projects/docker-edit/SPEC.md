# docker-edit: spec

## Problem

Docker cannot change a container after it is created, apart from a few resource
limits (`docker update`). To add a port mapping, change an environment variable,
mount a folder or switch the restart policy, you have to remove the container and
run it again with every original option retyped correctly. Get one wrong (a
network alias, a fixed IP, an anonymous volume, a label) and the service breaks or
loses data.

Evidence (checked 2026-10-18 through GitHub and web search):
- [moby/moby#21282](https://github.com/moby/moby/issues/21282), "Make docker update
  support port management as well": 50 👍, open since 2016-03.
- [moby/moby#8838](https://github.com/moby/moby/issues/8838), "How to set an
  environment variable on an existing container?": 104 👍.
- The Stack Overflow question "How do I assign a port mapping to an existing Docker
  container?" (19335444). Guides keep copying it (Baeldung, Better Stack,
  Edureka), and the answer is always "recreate it".
- Upstream says this is by design: a container's configuration is fixed when it is
  created.

Existing tools and why they fall short:
- **runlike** (2.9k★) and **docker-autocompose** (2.6k★) print a `docker run`
  command or a Compose file, which you then fix up and run by hand. Neither handles
  image defaults, anonymous volumes or several networks, nor stops, swaps or rolls
  back.
- **Portainer** can duplicate and edit a container, but only in its web UI.
- Small attempts named docker-recreate (andreccosta 2★, jeffrey4l 8★, snvfk1n 0★)
  recreate without editing and have no rollback.

## What v1 does

`docker edit CONTAINER [CHANGES] [--dry-run] [-y]`, a Docker CLI plugin (or a
standalone `docker-edit`).

**Changes:**
- `--publish 8080:80`, `--unpublish 80`;
- `--env K=V`, `--unset-env K`;
- `--mount`/`--volume SRC:DST[:ro]`, `--unmount DST`;
- `--restart POLICY`;
- `--label K=V`, `--unlabel K`;
- `--image REF` (recreate on another image or tag, which is how most people
  "update" a container);
- `--network NAME[:alias,...]`, `--disconnect NAME`.

**Plan first.** It shows exactly what changes, as a diff of the settings, and asks
first (or `--yes`). `--dry-run` stops there.

**Exact recreation.** Everything not changed stays exactly as it was:
- the command, entrypoint, user, working directory and healthcheck;
- ports, binds and named volumes;
- anonymous volumes (reattached, so their data stays);
- tmpfs, devices, capabilities, security options and sysctls;
- ulimits, resource limits, the restart policy and log settings;
- every network with its aliases, fixed IPs and MAC address;
- DNS settings, extra hosts and labels.

Settings that came from the image are not frozen into the container, so `--image`
picks up the new image's defaults. An auto-generated hostname stays automatic.

**Safe swap:**
1. Stop the old container and rename it `NAME.old-<time>`.
2. Create and start the new container under the original name.
3. Wait until it runs (and is healthy, if it has a healthcheck).
4. On any failure, remove the new container, give the old one its name back and
   start it again.

The old container is removed only after success, or kept with `--keep-old`.

**Refusals, with the reason:**
- Files changed in the container's writable layer (they would be lost; `docker
  diff` shows them) unless `--discard-changes`.
- `--rm` containers, which are removed when stopped, so there would be nothing to
  roll back to.
- Containers managed by Docker Compose (edit the Compose file instead).
- Containers that others join with `--network container:NAME`.

## Non-goals for v1

- Compose projects and Swarm services.
- Podman: maybe later, since its API is mostly compatible.
- Changing things `docker update` already changes in place; docker-edit uses
  `docker update` for those instead of recreating.

## Tech choices

- **Go**, one static binary, and no dependencies beyond the standard library. It
  talks to the Docker Engine API directly over unix, TCP or TLS sockets, with
  DOCKER_HOST and contexts, using the client written for docker-mop.
- **Tests** run against a real Docker daemon:
  - a matrix of container shapes is created, recreated with no change, and
    `docker inspect` compared field by field (only the ID, times and state may
    differ);
  - then each edit;
  - then failures at every step, checking that the original comes back.

## Milestones

1. **Exact recreation:** the Engine client, inspect to a create request (image
   defaults, anonymous volumes, networks, hostname), and `docker edit NAME
   --dry-run` with no changes showing nothing to do. The no-change recreation is
   proven exact on the shape matrix.
2. **Edits:** every change flag, the settings diff, and the `docker update` fast
   path.
3. **Safe swap:** stop, rename, create, start, health wait and rollback; the
   writable-layer, `--rm`, Compose and network-container checks; interrupt
   handling.
4. **Release:** CI, README with real output, two full reviews, LICENSE,
   CHANGELOG, v1.0.0.
