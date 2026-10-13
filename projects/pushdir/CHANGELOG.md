# Changelog

## 1.0.0 — 2026-10-14

First release.

- Watches a folder tree (Linux inotify), including folders created later, and pushes
  changes to any rclone remote in batches once files have stopped changing. Events
  are queued without blocking, so long uploads do not make the kernel drop events.
- Full copy at start; full checks every 6 hours and whenever events were lost (with
  watches re-added for folders that may have been missed).
- rclone filter flags after `--` apply to batches too, chosen by rclone's own rules.
- Deletions only with `--delete`, held until removals stop, counted only for files
  that are on the remote, with an hourly `--max-delete` budget and a pre-count before
  every full sync. Deletions pause when the folder looks unmounted, emptied or
  re-pointed; pushdir stops, deleting nothing, if the folder itself is removed or
  moved. `--backup-dir` keeps deleted and overwritten files, renames included.
- Renamed large files become remote-side moves (or copies without `--delete`) when
  size, time and content hash match.
- Retries with backoff; stops with an error on problems no retry fixes (unknown
  remote, usage or config errors). Refuses destinations that overlap the folder,
  relative destinations, and a second instance on the same folder.
- Passwords in connection-string remotes are redacted in logs and `/status`.
- `/status` and `/metrics` with `--listen`; systemd unit and Dockerfile.
