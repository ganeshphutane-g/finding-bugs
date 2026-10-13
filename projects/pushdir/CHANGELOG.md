# Changelog

## 1.0.0 — 2026-10-14

First release.

- Watches a folder tree (Linux inotify), including folders created later, and pushes
  changes to any rclone remote in batches once files have stopped changing.
- Full copy at start; full checks every 6 hours and whenever the kernel drops events.
- Deletions only with `--delete`, held until removals stop, with an hourly
  `--max-delete` budget, a pre-count before every full sync, and a guard that pauses
  deletions when the folder looks unmounted or emptied. `--backup-dir` keeps deleted
  and overwritten files.
- Renamed large files become remote-side moves (or copies without `--delete`).
- Retries with backoff; `/status` and `/metrics` with `--listen`.
- systemd unit and Dockerfile.
