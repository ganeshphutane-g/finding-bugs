# Changelog

## 1.0.0 — 2026-10-22

First release.

- `sync` makes Reed-Solomon parity for new repository files (each checked
  against its SHA-256 name first), and retires parity for pruned ones into
  `removed/`, kept for 7 days and brought back if the file returns.
- `check` reads what is due (or all, or a sample) and reports damage, what
  can be repaired, damaged parity, missing and uncovered files.
- `repair` rebuilds damaged files, proves them against their names, keeps the
  damaged copy in `quarantine/`, and puts the file back with its mode and
  owner in one rename; `--dry-run` and `--out`.
- `status` shows coverage, sizes and the last runs.
- Interleaved block groups: at the default 10%, any 10 bad 4 KiB blocks per
  group, or a 1.6 MiB run, in a 16 MiB pack.
- Guards: not a repository (an unmounted disk), mass removal, a changed or
  foreign config, a lock against concurrent runs.
- systemd units and timers in `contrib/`.
- Tested end to end with restic 0.18.1 and rest-server 0.14.0.
