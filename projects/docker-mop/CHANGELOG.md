# Changelog

## 1.0.0 — 2026-10-12

First release.

- `docker mop plan`: shows what `docker system|container|image|volume|network prune`
  would remove, with sizes, for the same flags and filters (`--all`, `--volumes`,
  `until`, `label`, `label!`). Table or JSON output. Verified against Docker's real
  prune on both the containerd and the classic image store.
- `docker mop prune`: removes exactly the planned objects after confirmation, never
  with force, and skips anything that changed since the plan.
- `docker mop volume copy` and `docker mop volume rename`: copy a volume through the
  container archive API with no image pulls, verify every file, and only then remove
  the old volume (rename). Optional `--compose-project` relabelling.
- Connects like the docker CLI: `DOCKER_HOST`, `DOCKER_CONTEXT`, contexts, TLS, and
  docker's global `--context`/`-H` flags.
