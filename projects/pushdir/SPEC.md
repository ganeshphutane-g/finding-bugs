# pushdir: spec

## Problem

rclone copies files to more than 70 kinds of storage, but it only runs when you run
it. People who want a folder backed up as it changes (photos from a phone, documents,
a camera's recordings, a database's dump folder) ask for a watch mode. That request
has been open since 2015, has 213 👍 and is labelled IMPORTANT
([rclone/rclone#249](https://github.com/rclone/rclone/issues/249)). The workarounds are
cron jobs that rescan everything every few minutes, or small shell scripts around
`inotifywait` that miss events, upload half-written files and break on renames.

## Who it is for

Self-hosters, NAS owners and small teams who already use rclone and want changes
pushed within seconds, reliably, without writing their own watcher.

## What v1 does

`pushdir [OPTIONS] LOCAL_DIR REMOTE:PATH [-- extra rclone flags]`

- **Starts with a full copy** of the folder (`rclone copy`), so anything changed
  while pushdir was not running is picked up. No state file is needed.
- **Watches the whole tree** with inotify, including folders created later.
- **Batches changes**: waits for a quiet moment (default 2 s, at most 30 s), then
  uploads the batch with one rclone call (`--files-from-raw`, `--no-traverse`).
- **Never uploads half-written files**: a file is sent only once its size and
  modification time have stopped changing.
- **Deletions are opt-in.** By default nothing is ever deleted on the remote. With
  `--delete`, local deletions are mirrored, optionally into a `--backup-dir` on the
  remote instead of being removed.
- **Guards against disasters**: refuses to mirror deletions when the folder looks
  unmounted or emptied (the root vanished, or more than `--max-delete` files went in
  one batch); it stops and says so instead.
- **Renames** of large files become server-side moves (`rclone moveto`) when the
  remote supports them, instead of a delete plus a full upload.
- **Recovers**: failed batches are retried with backoff; a periodic full check
  (default every 6 h) and any lost-event signal from the kernel (queue overflow)
  trigger a full `rclone copy` so nothing stays missed.
- **Reports**: one log line per batch; optional `--listen` HTTP endpoint with
  `/status` (JSON) and `/metrics` (Prometheus).
- Uses the user's normal rclone setup (`rclone.conf`, remotes, filters, bandwidth
  limits); extra flags after `--` go to every rclone call.

## Non-goals for v1

- Two-way sync (that is `rclone bisync`).
- Watching the remote for changes.
- macOS and Windows file watching (Linux inotify only in v1).
- Reimplementing rclone's filter rules: they are passed to rclone, which applies them.

## Tech choices

- **Go**, with `fsnotify` for inotify. rclone is run as a separate program, so any
  rclone version and configuration works and pushdir does not link rclone's code.
- **Tests run real rclone** (built from source here) against local folders and a
  remote served by `rclone serve` (WebDAV or SFTP), including failure injection
  (remote down), slow writers, renames, mass deletion and event-queue overflow.

## Milestones

1. **Skeleton**: CLI, rclone runner, initial full copy, test harness with real rclone.
2. **Watch and push**: recursive watcher, new folders, batching, write-stability
   check, batch uploads.
3. **Deletes and renames**: `--delete`, `--backup-dir`, `--max-delete`, unmounted-root
   guard, rename detection with server-side moves.
4. **Resilience and status**: retries with backoff, periodic and overflow resyncs,
   graceful shutdown, `/status` and `/metrics`.
5. **Release**: README with real output, systemd unit, Dockerfile, full review,
   LICENSE, CHANGELOG, v1.0.0.
