# Changelog

## 1.0.0 — 2026-10-15

First release.

- `plan`:
  - finds the Postgres service and the Compose files it was started with;
  - reads the cluster (databases, sizes, extensions, locales, roles, settings,
    checksums);
  - test-starts the new image to check extensions and locales;
  - lists blockers and shows the exact Compose change.
- `upgrade`:
  - stops the dependent apps and restarts the old database without a network;
  - dumps with the new version's tools and initialises a matching cluster in a new
    volume or folder;
  - carries config files and settings, restores and verifies (row counts,
    sequences, objects, databases, roles and password hashes);
  - edits the Compose file by position, checks the result and starts the service
    and apps.
- `--no-switch` rehearses the whole copy and puts the old service back.
- `rollback` returns to the old version and its untouched data, and recovers a run
  that was killed. `cleanup` removes the old data once the new version serves the
  app.
- Every failure, Ctrl-C or closed terminal puts everything back.
