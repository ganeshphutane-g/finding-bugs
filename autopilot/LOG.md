# Run log

Newest first.

## 2026-10-12 (IST) — run 1

- Research round 1 done (3 parallel searches); 13 candidates scored in IDEAS.md.
- Chosen: docker-mop, a Docker CLI plugin to preview prunes and rename/copy volumes
  (moby#30623 391 👍, moby#31154 576 👍, no established tool).
- Wrote SPEC.md. Milestone 1 done: dependency-free Engine API client (unix/TCP/TLS,
  DOCKER_HOST, contexts), plugin metadata, `docker mop version`, tests, CI, Makefile.
  Verified it installs and runs as a real `docker mop` plugin.
- Blocker: cannot create new repos (403); building in projects/docker-mop/ for now.
- Milestone 2 done: `docker mop plan` (containers, images, volumes, networks,
  system; until/label/label! filters; table and JSON). Integration tests compare the
  plan with Docker's real prune on the same state: 11 scoped cases + 8 unscoped +
  2 classic-builder cases, all identical on both the containerd and classic image
  stores. Bugs found and fixed on the way: hidden intermediate images, order-dependent
  parent handling, and containerd ignoring parent links.
- Found that Docker removes networks still used by stopped containers; the plan now
  warns about it.
- Milestone 3 done: `docker mop prune` shows the plan, asks (or --yes), removes
  exactly the planned objects without force, and skips anything that changed:
  a container started, a tag moved to a new image, a volume recreated or taken into
  use. docker-mop prune leaves the same state as Docker's prune in every case, on
  both image stores.
- Milestone 4 done: `docker mop volume copy|rename` with file-by-file verification
  (path, type, mode incl. setuid/sticky, owner, size, sha256, link targets, mtimes),
  label copy, --compose-project relabelling, and refusals for unsafe cases (in use,
  driver options, target exists). No image pulls: an empty local helper image and
  never-started containers. Tested with awkward data on both image stores.
- Milestone 5 done: two independent full reviews; fixed 6 issues in volume copy
  (race with a concurrently created target, hidden leftover helpers, stale Compose
  labels, xattr/dir-time checks, Ctrl-C leftovers, one fewer full read) and 9 in
  plan/prune (deleting an image that was tagged after the plan, config-only networks,
  until=0 and date parsing vs Docker, daemon clock/time zone, https without cert
  checks, docker global flags, API<1.42 volume rules, image edge cases). Added tests
  for each. README with real captured output, LICENSE, CHANGELOG, `make dist`.
  Full suite green on both image stores and in GitHub Actions. The v1.0.0 tag could
  not be pushed (git proxy refuses tags); noted as an owner step.
- Project #1 is done. Next run: research round 2.

## 2026-10-12 — setup

Playbook and state files created. Daily runs start after 12:30 AM IST.
Next: research for project #1.
