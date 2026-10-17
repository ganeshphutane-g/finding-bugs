# Changelog

## 1.0.0 — 2026-10-18

First release.

- `docker edit CONTAINER` changes a container's published ports, environment,
  mounts, restart policy, labels, image and networks by making it again exactly as
  it was, with only the changes. Works as a Docker CLI plugin or on its own.
- A plan of every setting that will differ, including the new image's defaults,
  before asking; `--dry-run` stops there. Secret-looking values are hidden.
- Exact recreation, checked against `docker inspect` for many container shapes:
  anonymous volumes are reattached with their data, every network keeps its
  aliases, fixed IPs and a set MAC address, automatic hostnames and MACs stay
  automatic, and image defaults are not frozen in.
- A safe swap: the old container is stopped and renamed, the new one must run (or
  become healthy) within `--wait`, and any failure or Ctrl-C puts the old one
  back. `--keep-old` keeps it afterwards.
- Refuses, with the reason, when something would be lost: files changed outside
  volumes (unless `--discard-changes`), `--rm` containers, Compose containers,
  and containers others depend on (`--network container:`, `--link`).
- A restart policy change alone is made in place.
- A tag that moved keeps the image the container runs, unless `--image` asks
  for the new one.
- Connects like the docker CLI: `DOCKER_HOST`, `DOCKER_CONTEXT`, contexts, TLS, and
  docker's global `--context`/`-H` flags.
