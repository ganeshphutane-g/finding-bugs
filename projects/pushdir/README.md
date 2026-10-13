# pushdir

Keep a folder backed up **as it changes**. pushdir watches a folder and sends every new,
changed or renamed file to any [rclone](https://rclone.org) remote within seconds:
S3, Backblaze B2, Google Drive, a NAS over SFTP or WebDAV, and the 70+ other kinds of
storage rclone supports.

rclone only runs when you run it. A watch mode is one of its oldest open requests
([rclone/rclone#249](https://github.com/rclone/rclone/issues/249), open since 2015,
213 👍). The usual workarounds have problems. A cron job rescans everything every few
minutes. Shell scripts around `inotifywait` miss events, upload half-written files,
re-upload renamed files and break on odd file names. pushdir is the missing piece:
it uses your own rclone, remotes and config, and adds the watching.

## What it looks like

```console
$ pushdir --delete --listen 127.0.0.1:9090 /srv/demo/Photos nas:photos -- --exclude '*.part'
2026-10-13 20:13:50 status on http://127.0.0.1:9090/status
2026-10-13 20:13:51 watching /srv/demo/Photos (2 folders), sending to nas:photos
2026-10-13 20:13:51 full check of /srv/demo/Photos
2026-10-13 20:13:51 full check done
2026-10-13 20:13:55 sent 2 files
2026-10-13 20:14:07 deleted 1 file
2026-10-13 20:14:19 error: deletions paused: the watched folder is empty (unmounted?); nothing more will be deleted on the remote until pushdir is restarted
```

In that run:
- **20:13:51:** pushdir copied the 3 existing photos.
- **20:13:55:** it sent a new photo and a photo in a newly created folder. The
  `IMG_0005.jpg.part` written at the same time was skipped because of the filter.
- **20:14:07:** a photo was deleted locally and pushdir deleted it on the remote.
- **20:14:19:** the folder was emptied, which looks like a failed mount. pushdir refused
  to mirror that, and the remote kept all 4 photos.

`/status` showed this (fetched just before the folder was emptied):

```json
{
  "local": "/srv/demo/Photos",
  "remote": "nas:photos",
  "state": "idle",
  "watched_folders": 3,
  "pending_files": 0,
  "pending_deletes": 0,
  "files_sent": 2,
  "files_deleted": 1,
  "batches": 1,
  "failed_batches": 0,
  "full_resyncs": 1,
  "last_success": "2026-10-13T20:14:07.16474183Z",
  "started_at": "2026-10-13T20:13:50.990411267Z"
}
```

`/metrics` serves the same numbers for Prometheus (`pushdir_files_sent_total`,
`pushdir_failed_batches_total`, `pushdir_deletions_paused`,
`pushdir_last_success_timestamp_seconds`, …).

## Install

pushdir needs Linux and rclone (tested with rclone v1.75.2).

```sh
make build                 # or, once published: go install github.com/ganeshphutane-g/pushdir@latest
```

**Docker:** the image contains both pushdir and rclone:

```sh
docker build -t pushdir .
docker run -d --name photos \
  -v /srv/photos:/data:ro -v ~/.config/rclone:/config/rclone \
  pushdir /data b2:my-photos
```

## Usage

```
pushdir [OPTIONS] LOCAL_DIR REMOTE:PATH [-- RCLONE_FLAGS...]
```

`REMOTE:PATH` is an rclone remote (`b2:bucket/photos`) or an absolute folder.

| Option | Default | Meaning |
|---|---|---|
| `--delete` | off | Mirror local deletions to the remote |
| `--backup-dir PATH` | | Keep deleted and overwritten remote files here (same remote, e.g. `b2:photos-old`) |
| `--max-delete N` | 100 | Pause deletions if more than N remote files would be deleted within an hour |
| `--quiet DURATION` | 2s | Wait this long after the last change before sending |
| `--max-delay DURATION` | 30s | Never hold changes longer than this |
| `--settle DURATION` | 2s | Send a file only after it has stayed unchanged this long |
| `--resync DURATION` | 6h | Full check this often (0: never) |
| `--listen ADDR` | | Serve `/status` and `/metrics`, e.g. `127.0.0.1:9090` |
| `--rclone PATH` | `rclone` on PATH | The rclone program |

Everything after `--` goes to rclone: `--config`, `--bwlimit 5M`, `--transfers 8`,
backend flags, and filters.

## How it works

- **Start-up:** a full `rclone copy`. Anything that changed while pushdir was stopped is
  picked up, so no state file is needed.
- **Watching:** inotify watches the whole tree, including folders created later; files
  already inside a new folder are found too.
- **Batches:** pushdir waits for a quiet moment, then sends the batch with one rclone
  call that names the exact files (`--files-from-raw`), so the remote is not listed.
- **Half-written files** are not sent. A file goes only after its size and modification
  time have stayed the same for `--settle` and no new write has come in. A file that
  keeps changing is sent after 10 minutes anyway, and sent again as it changes.
- **Retries:** a failed batch is retried with backoff, from 1 s up to 5 minutes.
- **Full checks:** run every `--resync`, and whenever the kernel drops events (pushdir
  then also re-adds watches for folders it may have missed).
- **Stopping:** Ctrl-C or SIGTERM lets the running rclone finish cleanly. A second
  Ctrl-C stops at once and kills rclone.

### Deletions are opt-in and guarded

Without `--delete`, pushdir never deletes anything on the remote. With it:

- **Removals are held** until removals have stopped for 10 s, so a burst is judged as a
  whole.
- **Only files that are on the remote count.** The temporary files of an editor's
  "write then rename" save never reached the remote, so they don't count.
- **An hourly budget:** if more than `--max-delete` files would be deleted within an
  hour, deletions pause until pushdir is restarted. Every full sync counts its
  deletions first, so it never deletes part of a large batch before stopping.
- **The folder is checked before every deletion.** Deletions pause if it is empty, on a
  different disk than at start (unmounted), unreadable, or (for a symlink) pointing
  somewhere new.
- **If the watched folder is removed or moved away, pushdir stops** and deletes
  nothing. Under systemd it is restarted on the new folder. That restart makes a normal
  start-up sync, still within `--max-delete`.
- **`--backup-dir`** keeps every deleted or overwritten file in another folder of the
  same remote, renames included.

### Renames

A large file (1 MiB or more) that appears under a new name is moved on the remote
(`rclone moveto`) instead of uploaded again. Without `--delete`, it is copied
remote-side instead (`copyto`). A rename is assumed only when all of these hold:
- a file just removed has the same size and modification time on the remote;
- it is the only such candidate;
- its content hash (md5 or sha1, whatever the remote has) matches the new local
  file.

Otherwise the file is simply uploaded. Remotes without hashes always upload.

### Filters

rclone filter flags after `--` (`--exclude`, `--include`, `--filter`, the `-from`
variants, `--min-size`/`--max-size`, `--min-age`/`--max-age`, `--exclude-if-present`,
`--ignore-case`, metadata filters) apply to everything pushdir sends and deletes.

rclone refuses to combine filter flags with the exact file lists pushdir uses for
batches. So pushdir lets rclone pick each batch's files: it runs one rclone listing
with your filter rules in the order rclone itself applies them, then sends only those
files. The tests compare that choice with what `rclone copy` itself copies, for 14
filter combinations and file names full of glob characters.

Not accepted:
- `--files-from*`: pushdir chooses the files.
- `--delete-excluded`: it would delete files outside the `--max-delete` count.
- Filters set as `RCLONE_EXCLUDE`-style environment variables: pass them after `--`
  instead.

### Safety checks at start

pushdir refuses to start in these cases, each with a message saying what to fix:
- the destination overlaps the watched folder;
- the destination is relative or empty;
- a named remote is missing from rclone's config;
- another pushdir already watches the same folder.

While running, rclone errors that no retry can fix stop pushdir with exit code 1:
- usage errors;
- a missing config section;
- a `--backup-dir` the remote can't use;
- rclone's "fatal" exit 7.

Network trouble is retried indefinitely.

Passwords inside connection-string remotes (`:sftp,host=…,pass=…:dir`) are shown as
`***` in logs and `/status`. `/status` has no login. Bind it to `127.0.0.1` (pushdir
warns when it is reachable from other machines).

## Running as a service

`contrib/pushdir@.service` is a systemd template, one instance per folder. Settings go
in `/etc/pushdir/NAME.env`; see the comments in the file. Under systemd, logs go to the
journal without pushdir's own timestamps.

Note: the unit file has not been tested on a real systemd host yet. pushdir's behaviour
under it (SIGTERM, restarts, journal output) has only been tested by running the
program directly.

```sh
sudo systemctl enable --now pushdir@photos
journalctl -u pushdir@photos -f
```

The service user needs to read rclone's config. The default is that user's
`~/.config/rclone/rclone.conf`; or put `--config /path` in `PUSHDIR_RCLONE_FLAGS`.

## Limits

- **Linux only** (inotify).
- **Watch limits:** each folder uses one inotify watch. For very large trees, raise
  `fs.inotify.max_user_watches`; pushdir says so when it runs out.
- **Symlinks** inside the folder are not watched. If you pass `-l` or `-L` to rclone,
  changes to links arrive with the next full check.
- **Unusual file names:** names with line breaks, other control characters or invalid
  UTF-8 cannot go through rclone's file lists. They are sent by a full check, which
  pushdir starts as soon as it sees one.
- **Unchanged-looking rewrites:** pushdir decides *when* to send and rclone decides
  *whether* a file differs. A file rewritten with the same size and modification time,
  or on a remote without modification times, may be skipped by rclone the way
  `rclone copy` would skip it. Add `--checksum` after `--` if that matters.
- **Large first copies:** the first copy of a huge tree takes as long as `rclone copy`
  does. Changes made meanwhile are queued and sent afterwards.

## Development

```sh
make rclone   # builds rclone v1.75.2 into .bin/ if it is not installed
make test     # go vet, then all tests with -race against the real rclone
```

The tests run real rclone against local folders, a WebDAV server started with
`rclone serve webdav` (including an outage in the middle), and real inotify. They cover:
- half-written files;
- mass and trickling deletions, and an emptied or replaced folder;
- renames, including a different file that only looks like one;
- atomic saves, filters and unusual names;
- clock-skewed files, secrets in remotes, and shutdown under load.

## License

MIT, see [LICENSE](LICENSE).
