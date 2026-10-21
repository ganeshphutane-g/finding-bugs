# Changelog

## 1.0.0 — 2026-10-22

First release.

- `sync` makes Reed-Solomon parity for new repository files (each checked
  against its SHA-256 name first), and retires parity for pruned ones into
  `removed/`, kept for 7 days and brought back if the file returns.
  `--init` starts a parity directory.
- `check` reads each file once within `--every` (spread by name), or all, or
  a sample. It reports damage and whether it can be repaired, damaged
  parity, a changed config, and missing, unreadable and uncovered files.
- `repair` rebuilds damaged files, proves them against their names, keeps the
  damaged copy in `quarantine/`, and puts the file back with its mode and
  owner in one rename. It also makes damaged parity again. `--dry-run` and
  `--out`.
- `status` shows coverage, sizes, known damage and the last runs, without a
  lock.
- Interleaved block groups: at the default 10%, any 10 bad 4 KiB blocks per
  group, or a 410-block run, in a 16 MiB pack.
- Guards:
  - not a repository (an unmounted disk);
  - an unset parity directory without `--init`;
  - another repository's parity;
  - an emptied `data/`, `index/` or `keys/`;
  - a changed config;
  - a waiting lock against concurrent runs;
  - crafted parity files.
- `--json` for every command; systemd units and timers in `contrib/`.
- Tested end to end with restic 0.18.1 and rest-server 0.14.0, after two
  full reviews (correctness and data safety; security and operations).
