# packparity

Repair data for [restic](https://restic.net) repositories. packparity keeps
Reed-Solomon parity for every file of a repository in another directory. When
bit rot or a bad sector damages a pack, it rebuilds the file. The result is
proven correct before it is put back, because restic names each file by the
SHA-256 of its contents.

restic finds damage (`restic check --read-data`) but cannot undo it:
`restic repair packs` saves the intact blobs and drops the rest. Users have
asked for redundancy since 2015
([restic#256](https://github.com/restic/restic/issues/256),
[restic#804](https://github.com/restic/restic/issues/804)), and some run
hand-written `par2create` loops over `data/`. packparity does that job
properly:
- it covers only new files;
- it retires parity for pruned ones;
- it checks a rolling share of the repository;
- it never writes a rebuilt file that does not match its name.

```
$ packparity sync backup/repo parity
sync: 10 new files covered (65.8 MiB), 0 retired, 0 brought back, 0 purged, in 818ms

# ... 256 KiB of a pack turns to zeros ...

$ restic -r backup/repo check --read-data
Damaged pack files can be caused by backend problems, hardware problems or bugs in restic. ...
Fatal: repository contains errors

$ packparity check --read all backup/repo parity
check: read 10 files (65.8 MiB) in 624ms
REPAIRABLE      data/d1/d14f2d3a…71fe: 65 of 4278 blocks damaged (260.0 KiB)
Run packparity repair to fix it.

$ packparity repair backup/repo parity
repaired       data/d1/d14f2d3a…71fe: the damaged copy is in parity/quarantine/data/d1/d14f2d3a…71fe.20261021T210826Z
Run restic check --read-data to confirm.

$ restic -r backup/repo check --read-data
no errors were found
```

(Real output, with the IDs shortened.)

## When it helps, and when it does not

A second copy of the repository (`restic copy`, the 3-2-1 rule) is still the
better protection, and packparity does not replace it.

**Where packparity helps:**
- one copy is all you have;
- the second copy is slow or costly to restore from (a remote or cold
  bucket);
- you want damage fixed in place, before the next `prune` makes it worse.

**Where it does not help:**
- **The whole disk is lost.** Keep the parity on another disk to survive
  losing one of the two, but the parity alone cannot rebuild a repository.
- **A file is missing entirely.** Parity rebuilds damage within a file. For
  a missing file, use restic's own repair steps, or a second copy.
- **ZFS or btrfs with redundancy** already repair from their own copies.

## Install

```
go install github.com/ganeshphutane-g/packparity@latest
```

Or `make build` in this directory. It is a single static binary for Linux,
macOS and the BSDs (not Windows).

## Use

```
packparity sync   REPO PARITY              # after each backup and prune
packparity check  REPO PARITY              # weekly: reads what is due
packparity repair REPO PARITY [NAME|ID...] # when check finds damage
packparity status REPO PARITY
```

- **REPO** is the repository's directory: a local repository, rest-server's
  folder for it (`--path` plus the repository name), or a mounted SFTP or
  network share.
- **PARITY** is a directory, best on another disk. It needs about 10% of the
  repository's size.
- packparity never needs the repository password: it reads files and
  hashes, nothing else.

### sync

- Covers new files. Each is read, its SHA-256 checked against its name, and
  its parity written atomically.
- Retires parity for files that are gone (pruned) into `PARITY/removed/`. It
  is purged after `--keep-removed` (default `7d`), and brought back if the
  file comes back.
- A file that does not match its name has no parity to repair it from, so it
  is reported as `DAMAGED` (exit 4). If it is younger than 10 minutes it is
  skipped instead, as it may still be being written; the next sync tries
  again.
- **Refusals:**
  - a REPO that is not a repository (an unmounted disk);
  - more than a quarter of the covered files gone at once (a wrong path or a
    damaged disk, not a prune), unless `--allow-mass-removal`;
  - a `config` that is not the one the parity was made for: it may be
    another repository, or one restic changed. If you ran `restic migrate`,
    pass `--accept-config`.
- `--redundancy N` sets the parity for new files, in percent (default 10).
  Each parity file records its own settings.

### check

- `--read new` (default) reads the files not verified within `--every`
  (default `30d`), oldest first. So a weekly check reads about a quarter of
  the repository each time.
- `--read all` reads everything; `--read 5%` reads a random sample.
- `--json` prints the result as JSON.
- **Exit codes:** 0 clean, 3 damage `repair` can fix, 4 damage it cannot.
- **Also reported:**
  - files with parity that are gone (pruned, or lost: `restic check` will
    say which);
  - files sync has not covered yet;
  - damaged parity files (the data is fine; repair makes the parity again).

### repair

- With no names, repairs what the last check or sync found. Otherwise give
  repository names (`data/ab/ab12…`) or IDs.
- For each file it rebuilds the damaged blocks and checks the result's
  SHA-256 against the name. Only then does it keep the damaged copy in
  `PARITY/quarantine/` and put the rebuilt file in place, with its mode and
  owner, in one rename.
- If too much is damaged, it writes nothing and exits 4.
- `--dry-run` rebuilds and proves without writing. `--out DIR` writes the
  rebuilt files to DIR instead.
- **config** is repaired only when named (`packparity repair REPO PARITY
  config`). A changed config may be restic's doing, not damage.

## How much it can repair

Each file is cut into 4 KiB blocks (smaller files into one block). Block `i`
goes to group `i mod G`, so a run of bad blocks is spread across the groups.
Each group has up to 100 data blocks and Reed-Solomon parity for them.

At the default 10%, a 16 MiB pack (restic's default size) has 41 groups, each
with 100 data blocks and 10 parity blocks. That covers:
- any 10 bad blocks per group;
- one run of up to 1.6 MiB;
- scattered damage of up to 10% of the blocks, if no group gets more than
  10.

Small files (keys, snapshots, small index files) are fully duplicated.

All blocks carry a short hash, so bad blocks are found exactly, including in
the parity itself. A damaged parity block costs its group one repair.

## Schedule it

Run sync after your backup and prune jobs, and check weekly. With systemd,
see [contrib/](contrib/): `packparity-sync.service` with a timer, and
`packparity-check.service` with a weekly timer. Copy `packparity.env` to
`/etc/packparity.env` and set your two paths in it. A failed unit means
damage was found (exit 3 or 4), so your usual alerting on failed units
covers it.

For rest-server, run packparity on the server, against the repository's
directory. Each repository needs its own PARITY directory.

## Tests

`make test` runs the unit tests: the layout, every damage pattern up to the
limit and one past it, short and long files, and damaged parity.

`make test-restic` also runs end to end with real restic and rest-server
(CI uses restic 0.18.1 and rest-server 0.14.0):
- packs, index files, snapshots and config damaged, found, repaired, then
  `restic check --read-data` passes and restores match;
- damage beyond reach, with nothing written;
- prune and the retired parity;
- every refusal;
- a rest-server repository;
- sync running again and again during a 120 MiB backup.

## License

MIT
