# packparity: spec

Repair data for restic repositories: Reed-Solomon parity kept beside the
repository, so a pack damaged by bit rot or a bad sector can be rebuilt
exactly, and the rebuilt file is proven correct before it is put back.

## Why

- restic#256 "Add redundancy for data stored in the repository" (44 👍, open
  since 2015) and restic#804 "Implement error correcting code (reed-solomon)?"
  (18 👍) are still open; borg#225 (43 👍) asks the same of borg.
- restic forum users run hand-written `par2create` loops over `data/` after
  each backup ("How to create par2 parity files for a Restic repository").
  Those loops do not know which files are new, leave parity behind after a
  prune, and do not check a repair against the file's name.
- `restic repair packs` only salvages the intact blobs of a damaged pack; the
  rest is gone unless the source files still exist.

The maintainers' advice is a second copy of the repository (3-2-1), and that
stays the better protection. packparity is for the cases where a second copy
is slow to make or to restore from, or where one copy is all there is. It
does not help when the whole disk is lost, and it is not needed on ZFS or
btrfs with redundancy (they repair from their own copies).

## What restic gives us (verified, restic 0.18.1)

- A repository is `config`, `keys/`, `snapshots/`, `index/`, `data/xx/` and
  `locks/`.
- Every file except `config` is named by the SHA-256 of its contents, so any
  damage is found exactly, without the password, and a repair can be proven
  correct by hashing it.
- Files are written once and never changed (restic makes them read-only);
  they are only added (backup, `key add`) and deleted (prune, forget,
  `key remove`).
- rustic uses the same format; rest-server stores the same tree on disk.

## Commands

```
packparity sync   REPO PARITY   # parity for new files, removal for gone ones
packparity check  REPO PARITY   # find damage; say what can be repaired
packparity repair REPO PARITY [ID...]   # rebuild damaged files, prove, put back
packparity status REPO PARITY   # coverage, sizes, last sync and check
```

REPO is a local path: a local repository, rest-server's directory for one
repository, or a mounted SFTP or network share. PARITY is a directory,
ideally on another disk.

### sync

- Lists the repository. A file without parity is read, its SHA-256 checked
  against its name, and its parity written (temporary file, fsync, rename).
- A file whose hash does not match its name gets no parity: it is reported as
  damaged (exit 4), as is a file that cannot be read; the rest go on. A
  mismatching file younger than 10 minutes is skipped instead (it may still
  be being written) and retried by the next sync.
- `config` is not named by its hash: its hash is recorded with its parity. A
  changed `config` is reported (exit 3) and new files are still covered;
  `--accept-config` takes the new one (after `restic migrate`, for example).
  A damaged `config.par` is made again from the current config.
- Parity for a file that is gone is moved to `PARITY/removed/` and deleted
  after `--keep-removed` (default 7 days); it is brought back if the file
  comes back (a disk that was briefly missing).
- Guards:
  - REPO must look like a repository (`config`, `keys/`, `data/`), or sync
    refuses (an unmounted disk looks like an empty folder);
  - PARITY must already be set up, unless `--init` (an unmounted parity disk
    must not be started again on the root disk);
  - PARITY must share files with REPO (two repositories never share one),
    or every command refuses: it is another repository's;
  - if `data/`, `index/` or `keys/` has no files left where files had
    parity, retiring is held back (exit 3) without `--allow-mass-removal`;
  - sync, check and repair hold `PARITY/lock`, waiting up to `--wait` for
    another run; status takes no lock.
- `--redundancy N` (percent, default 10) applies to new parity; each parity
  file records its own settings, so changing it later needs no rebuild.

### check

- `--read new` (default): hashes files whose parity has not been verified
  since `--every` (default 30 days), oldest first; `--read all`; `--read
  N%` for a random sample.
- For each damaged file it says which blocks are bad and whether parity can
  rebuild them. Missing files and damaged parity files are reported too.
- Exit 0 clean, 3 damage that `repair` can fix, 4 damage it cannot.

### repair

- For each damaged file (named IDs, or all that check finds):
  - rebuilds it from its good blocks and the parity;
  - checks the result's SHA-256 against its name (or the recorded hash for
    `config`); nothing is written unless it matches;
  - moves the damaged file to `PARITY/quarantine/` and puts the rebuilt one
    in its place (temporary file in the same directory, fsync, same mode,
    rename).
- `--dry-run` rebuilds and proves without writing. `--out DIR` writes the
  rebuilt files to DIR instead of the repository.
- A missing file cannot be rebuilt (parity covers damage within a file); the
  report names it and the restic commands to recover what it held
  (`restic repair index`, `restic repair snapshots`, or a copy from another
  repository).

### status

Files covered and not, parity size against repository size, removed parity
waiting, last sync and the age of the oldest verification.

## Parity layout

- `PARITY/<dir>/<first two hex>/<id>.par` for `data`, `index`, `snapshots`
  and `keys`; `PARITY/config.par`.
- The file is cut into blocks (4 KiB, smaller for files under 4 KiB). Block
  `i` goes to group `i mod G`, so a run of consecutive bad blocks (a bad
  sector run) costs each group only one or two blocks. Each group has `k`
  data blocks and `m` Reed-Solomon parity blocks, with `m/k` the redundancy.
- The parity file holds a header (format version, file size, block size, `k`,
  `m`, `G`, the file's SHA-256), a 16-byte SHA-256 prefix per data block and
  per parity block (to find bad blocks), checksums of the header and of the
  block table, and the parity blocks. A layout a parity file claims is
  checked against what packparity would make for its file size, so a
  crafted file cannot make it allocate or index out of range.
- With the default 10%: a 16 MiB pack has 4096 blocks in 41 groups of 100
  plus 10 parity blocks each; any 10 bad blocks per group (a run of up to
  410 blocks: 1.6 MiB on block boundaries) are repairable, at about 10.4%
  extra space.

## Out of scope for v1

- Remote backends (S3, B2, rclone, rest-server over HTTP): run packparity
  where the files are.
- Borg and Kopia (different formats; Kopia has its own error correction).
- Rebuilding whole lost files at low redundancy (cross-file parity).
- Decrypting anything: packparity never needs the password.

## Tests

- Unit: the block and group layout; encode and rebuild for bursts, flips and
  random scatter up to `m` per group and one past it; damaged and crafted
  parity files, with a fuzz test; tiny and empty files; `config`; every
  guard.
- End to end with real restic and rest-server (both installed in CI with `go
  install`):
  - back up, sync, flip bytes and zero whole runs in packs, index and
    snapshot files; `restic check --read-data` fails, `packparity check`
    exits 3, `repair` fixes, `restic check --read-data` passes, and a restore
    matches the source;
  - damage beyond reach: exit 4, nothing written;
  - forget + prune, then sync moves the removed parity; the mass-removal and
    not-a-repository guards;
  - sync during a running backup.

## Milestones

1. Layout and Reed-Solomon encode/rebuild with tests.
2. sync with its guards; status.
3. check and repair.
4. End-to-end tests with restic and rest-server, CI.
5. README, CHANGELOG, LICENSE, systemd timer, two reviews.
