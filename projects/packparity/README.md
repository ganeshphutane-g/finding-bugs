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
- it reads a share of the repository each week;
- it never writes a rebuilt file that does not match its name.

```
$ packparity sync --init backup/repo /mnt/disk2/parity
sync: 10 new files covered (65.8 MiB), 0 retired, 0 brought back, 0 purged, in 1.818s

# ... 256 KiB of a pack turns to zeros; restic check --read-data fails ...

$ packparity check --read all backup/repo /mnt/disk2/parity
check: read 10 files (65.8 MiB) in 1.305s
REPAIRABLE      data/4b/4b50a9f8…e9d7: 65 of 4123 blocks damaged (260.0 KiB)
Run packparity repair REPO PARITY to fix what can be fixed.

$ packparity repair backup/repo /mnt/disk2/parity
repaired            data/4b/4b50a9f8…e9d7: the damaged copy is in /mnt/disk2/parity/quarantine/data/4b/4b50a9f8…e9d7.20261021T213612Z
Run restic check --read-data to confirm.

$ restic -r backup/repo check --read-data
no errors were found
```

(Real output, with the IDs and paths shortened.)

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

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/packparity
make build        # ./packparity
```

It is a single static binary for Linux, macOS and the BSDs (not Windows).

## Use

```
packparity sync   REPO PARITY              # after each backup and prune (--init the first time)
packparity check  REPO PARITY              # weekly: reads what is due
packparity repair REPO PARITY [NAME|ID...] # when check finds damage
packparity status REPO PARITY              # any time; takes no lock
```

- **REPO** is the repository's directory: a local repository, rest-server's
  folder for it (`--path` plus the repository name), or a mounted SFTP or
  network share. A top-level folder (such as `data/` on another disk) may be
  a symlink.
- **PARITY** is a directory, best on another disk. It needs about 10% of the
  repository's size. Each repository needs its own.
- packparity never needs the repository password: it reads files and
  hashes, nothing else.
- sync, check and repair take PARITY's lock and wait for another run to
  finish (up to `--wait`, default 1 h).

### sync

- **The first time,** pass `--init`: it starts PARITY, which must be missing
  or empty. Without `--init`, a PARITY that is not set up is refused, so an
  unmounted parity disk is not quietly started again on the root disk.
- **New files:** each is read, its SHA-256 checked against its name, and
  its parity written atomically.
- **Gone files (pruned):** their parity is retired into `PARITY/removed/`. It
  is purged after `--keep-removed` (default `7d`), and brought back if the
  file comes back.
- **Files that cannot be covered:** a file that does not match its name, or
  cannot be read, has no parity to repair it from. It is reported as
  `DAMAGED` (exit 4) and the rest go on. A file younger than 10 minutes is
  skipped instead (it may still be being written); the next sync tries
  again.
- **Refusals and hold-backs:**
  - a REPO that is not a repository (an unmounted disk): refused;
  - a PARITY that belongs to another repository (they share no files):
    refused;
  - `data/`, `index/` or `keys/` with none of its files left, where files
    had parity (an unmounted disk or a broken link, not a prune): retiring
    is held back (exit 3) until the folder is back, or you pass
    `--allow-mass-removal`. New files are still covered.
  - a `config` that differs from the one the parity has: exit 3. If you ran
    `restic migrate`, pass `--accept-config`. New files are still covered.
- `--redundancy N` sets the parity for new files, in percent (default 10).
  Each parity file records its own settings.
- `-q` prints only problems; `--json` prints the result.

### check

- **What it reads:**
  - `--read new` (default) reads each file once within `--every` (default
    `30d`). Each file is due at a point between half of that and all of it,
    fixed by its name, so a weekly check reads a share each time rather than
    the whole repository at once.
  - `--read all` reads everything; `--read 5%` reads a random sample.
- **What it reports:**
  - damage, and whether parity can repair it;
  - damaged parity files (the data is fine; repair makes the parity again);
  - a changed config, with the two ways out;
  - files with parity that are gone (pruned, or lost: `restic check` will
    say which);
  - files sync has not covered yet;
  - files that cannot be read, which are reported and do not stop the
    check.
- **Exit codes:** 0 clean, 3 damage `repair` can fix (or a changed config),
  4 damage it cannot.
- `--json` prints the result as JSON.

### repair

- **What it repairs:** with no names, what the last check or sync found.
  Otherwise give repository names (`data/ab/ab12…`), IDs, or ID prefixes of
  8 or more characters (as restic prints them).
- **How:**
  - it rebuilds the damaged blocks and checks the result's SHA-256 against
    the name;
  - only then does it keep the damaged copy in `PARITY/quarantine/` and put
    the rebuilt file in place, with its mode and owner, in one rename
    through the folder's handle;
  - damaged parity is made again at the same time.
- **When it writes nothing:**
  - too much is damaged (exit 4);
  - the file belongs to another user and packparity is not running as that
    user or as root (it would otherwise lose its owner).
- `--dry-run` rebuilds and proves without writing. `--out DIR` writes the
  rebuilt files to DIR instead.
- **config** is repaired only when named (`packparity repair REPO PARITY
  config`): a changed config may be restic's doing, not damage. Repair it
  only if restic cannot open the repository.

### status

Coverage, sizes, removed and quarantined files, the last runs, and the
damage known from the last sync or check. It takes no lock and writes
nothing, so it works during a long run and for a user who can only read
PARITY. It exits 3 when known damage can be repaired, 4 when it cannot.

## How much it can repair

Each file is cut into 4 KiB blocks. Files under 4 KiB are one block, fully
duplicated. Block `i` goes to group `i mod G`, so a run of bad blocks is
spread across the groups. Each group has up to 100 data blocks, plus
Reed-Solomon parity for them.

At the default 10%, a 16 MiB pack (restic's default size) has 41 groups, each
with 100 data blocks and 10 parity blocks. That covers:
- any 10 bad blocks per group;
- one run of up to 410 blocks (1.6 MiB if it starts on a block boundary;
  one block less otherwise);
- scattered damage of up to 10% of the blocks, if no group gets more than
  10.

All blocks carry a short hash, so bad blocks are found exactly, including in
the parity itself. A damaged parity block costs its group one repair.

## Keep PARITY as safe as the repository

PARITY holds full copies of small files, including restic's key files (which
are encrypted with your password). `removed/` keeps them for
`--keep-removed` after `restic key remove`. packparity writes everything
0600 or 0400; protect PARITY like the repository itself.

`quarantine/` keeps every damaged file that repair replaced. Delete them
once `restic check --read-data` is clean.

## Schedule it

[contrib/](contrib/) has systemd units with timers: `packparity-sync`
(daily) and `packparity-check` (weekly).
1. Copy `packparity.env` to `/etc/packparity.env` and set your two paths.
2. Add `RequiresMountsFor=` with those paths to both units.
3. Order the sync after your backup and prune units.
4. Set `User=` if the repository is not root's.

A failed unit means packparity found something (exit 3 or 4) or could not
run (exit 1). Your usual alerting on failed units covers both.

With rest-server and `--private-repos`, run one pair of units per
repository, each with its own PARITY.

## Limits

- **Memory:** a whole file is held in memory while it is covered, checked
  or repaired: at most about 3.3 times the largest pack (about 55 MiB with
  restic's default 16 MiB packs, about 420 MiB with `--pack-size 128`).
- **Speed:** the first sync reads the whole repository, and later ones read
  only new files.
- **Network filesystems:** the lock is a local `flock`, so two machines on
  one share do not exclude each other. Run packparity from one machine.
- **restic running at the same time:** packparity does not read restic's
  locks. A repair during a prune may put back a pack the prune just deleted;
  restic ignores it (it is in no index) and the next prune removes it.
- **Platforms:** Linux, macOS and the BSDs. Not Windows, Solaris or AIX.

## Tests

`make test` runs the unit tests:
- **parity format:** the layout; bursts, single flips and random scatter up
  to the limit and one past it; short and long files; damaged parity;
  crafted parity files; a fuzz test of the parser.
- **sync, check and repair** on hand-made repositories: every refusal and
  hold-back, foreign parity, config changes, stale damage reports, spread
  checks, and short IDs.

`make test-restic` also runs end to end with real restic and rest-server
(CI uses restic 0.18.1 and rest-server 0.14.0):
- packs, index files, snapshots and config damaged, found, repaired, then
  `restic check --read-data` passes and restores match;
- damage beyond reach, with nothing written;
- damaged parity remade;
- prune and the retired parity;
- foreign parity, an unmounted parity disk, an emptied `index/`;
- a symlinked `data/`;
- a rest-server repository;
- sync running again and again during a 120 MiB backup.

## License

MIT
