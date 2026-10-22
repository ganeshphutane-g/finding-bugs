# pgcarry

Move the PostgreSQL database of a Docker Compose app to a new major version
(14 → 18, 16 → 17, …) **without risking the data**. pgcarry makes a fresh copy for
the new version, checks it against the original, changes the Compose file and starts
the app on the copy. The old data stays exactly as it was, so you can go back.

The official `postgres` image cannot do major upgrades. Change `postgres:14` to
`postgres:17` and the container stops with "database files are incompatible with
server". From 18 the image also keeps its data in a different folder, and it refuses
to start on an old mount. Both errors point to
[docker-library/postgres#37](https://github.com/docker-library/postgres/issues/37),
"Upgrading between major versions?", which has been open since 2014 (278 👍). The
usual answer is a page of manual `pg_dump`/`pg_restore` steps that are easy to get
subtly wrong. pgcarry is those steps done carefully.

## What it looks like

`plan` checks everything and changes nothing:

```console
$ cd /srv/demo/notes
$ pgcarry plan --to 18
Project notes, service db (postgres:14)
Data: volume notes_pgdata, mounted at /var/lib/postgresql/data
Now: PostgreSQL 14.24 (Debian 14.24-1.pgdg13+2), 35.8 MiB in 2 database(s), data checksums off
  notes                 10.9 MiB  UTF8, en_US.utf8, extensions: pg_trgm 1.6
  postgres               8.4 MiB  UTF8, en_US.utf8
New: postgres:18 (PostgreSQL 18), data at /var/lib/postgresql/18/docker
Free space next to the data: 12.4 GiB

What changes:
  - data checksums are turned on (the new image's default)
  - the service's image becomes postgres:18
  - data moves to a new volume notes_pgdata_pg18 mounted at /var/lib/postgresql; the old volume notes_pgdata is kept unchanged for rollback
  - services that depend on db are stopped during the upgrade and started again: app

The Compose file change for the switch:
  --- /srv/demo/notes/compose.yaml
  +++ /srv/demo/notes/compose.yaml (after)
  @@
       depends_on: [db]
     db:
  -    image: postgres:14
  +    image: postgres:18
       environment:
         POSTGRES_USER: notes
  @@
         POSTGRES_DB: notes
       volumes:
  -      - pgdata:/var/lib/postgresql/data
  +      - pgdata_pg18:/var/lib/postgresql
   
   volumes:
  +  pgdata_pg18:
     pgdata:

Nothing blocks the upgrade. Nothing has been changed.
```

`upgrade` does it (this is the same project; `app` takes 10 s to stop because it is
a `sleep` standing in for a real app):

```console
$ pgcarry upgrade --to 18
...
stopping the apps that use the database ...
  stopping app
  done in 10.4s
stopping the database ...
  done in 400ms
starting the old database without a network, so nothing else can write to it ...
  done in 400ms
recording what the database holds ...
  done in 2.4s
dumping roles and databases ...
  notes: 0 MiB, 12 entries
  postgres: 0 MiB, 2 entries
  template1: 0 MiB, 2 entries
  done in 2.9s
stopping the old database again ...
  done in 200ms
creating the new data volume ...
  done in 0s
starting PostgreSQL 18 on it ...
  done in 3.7s
restoring roles and databases ...
  notes
  postgres
  template1
  done in 1.7s
verifying the copy ...
  notes: 1 tables, 25000 rows match
  postgres: 0 tables, 0 rows match
  template1: 0 tables, 0 rows match
  done in 2.7s
stopping the new database ...
  done in 300ms
switching the service to the new data ...
  done in 1.2s

Done: db now runs PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2) on notes_pgdata_pg18.
Started again: app.
The old data (notes_pgdata) is kept unchanged, and the original Compose file is saved in /root/.local/state/pgcarry/notes/db-20261014-203532.
To go back:           pgcarry rollback --backup-dir /root/.local/state/pgcarry/notes/db-20261014-203532
When you are happy:   pgcarry cleanup --backup-dir /root/.local/state/pgcarry/notes/db-20261014-203532  (removes the old data)
```

Once the app has run well for a while:

```console
$ pgcarry cleanup
This removes the old data volume notes_pgdata for good. db keeps running on notes_pgdata_pg18; the dumps stay in /root/.local/state/pgcarry/notes/db-20261014-203532.
Remove the old data? [y/N] y
Removed notes_pgdata.
The old volume's entry under volumes: in the Compose file is no longer used; you can delete it.
```

For a database of 1.1 GB (3 million rows), a rehearsal took 51 seconds on the test
machine: 14 s to dump, 21 s to restore, 2 s to verify. Your app is down for
about that long. Run `pgcarry upgrade --no-switch` first to measure it: that is a
full rehearsal. It copies and verifies, then starts everything again on the old data.

## Install

You need Docker with the Compose plugin (`docker compose`). Tested on Linux; macOS and
Windows with Docker Desktop should work but have not been tested.

```sh
make build        # or, once published: go install github.com/ganeshphutane-g/pgcarry@latest
```

Run it from the project's folder. It finds the Postgres service and the Compose files
it was started with, or takes `-p NAME`, `-f FILE` and `--project-directory DIR` as
`docker compose` does.

## Commands

| Command | What it does |
|---|---|
| `pgcarry plan --to 18` | Report what an upgrade would do and what stands in the way. Changes nothing. |
| `pgcarry upgrade --to 18` | Copy, verify, switch the service over. Asks first (`-y` to skip). |
| `pgcarry upgrade --to 18 --no-switch` | Rehearse: copy and verify, then start everything again on the old data. |
| `pgcarry rollback` | Put the original Compose file back and start the old version on its untouched data. Also recovers a run that was killed halfway. |
| `pgcarry cleanup` | Remove the old data once the new version serves the app. The dumps stay. |

Other options:
- `--image IMAGE` picks the new image when pgcarry cannot work it out. It knows the
  official `postgres` tags (Alpine stays Alpine), `pgvector/pgvector` and
  `timescale/timescaledb`. Name the SERVICE when a project has more than one Postgres.
- `--backup-dir DIR` sets where dumps go. The default is
  `~/.local/state/pgcarry/PROJECT/SERVICE-TIME`.

Exit codes: 0 done, 1 error, 2 usage, 3 blocked by the plan, 130 interrupted
(everything put back).

## How it keeps your data safe

- **The old data is never changed.** pgcarry only reads it, through the old version's
  own server. The copy goes into a new volume (or a new folder next to a bind
  mount). Rollback is always possible until you run `cleanup`.
- **Nothing can write during the copy.** The apps that depend on the database are
  stopped. The database is then restarted for the dump **with no network at all**, so
  even a client pgcarry does not know about cannot write a row that would be missed.
  A test runs an undeclared app that inserts rows continuously; every row the old
  server accepted is on the new one.
- **The dump is taken with the new version's tools**, as the PostgreSQL docs advise,
  one `pg_dump` archive per database plus the roles. Each archive is read back before
  anything else happens.
- **The new cluster matches the old one:**
  - same encoding and locale (libc, ICU or builtin), same users and passwords (the
    same password hashes);
  - settings from the data folder's config files (`postgresql.conf`,
    `postgresql.auto.conf`), `pg_hba.conf`, `pg_ident.conf`, and template1's own
    settings;
  - the service's command (for example `-c shared_preload_libraries=…`) and its
    numeric `user:`.
- **The copy is verified before the switch:**
  - every table's exact row count and every sequence value;
  - counts of schemas, tables, views, functions, indexes, triggers and constraints;
  - the list of databases;
  - every role's attributes, memberships and password hash.

  Any difference stops the upgrade with the old data back in service.
- **The Compose edit is minimal and checked.** Only the image line, the volume line
  and one new volume line change; comments and formatting stay. After the edit,
  `docker compose config` must show the new image and data, and the started service
  must report the new version and the cluster pgcarry built.
- **It refuses rather than guesses**, before anything is stopped, when:
  - an extension or a locale is missing in the new image (pgcarry test-starts the
    image to check);
  - the data is in an anonymous volume, or a volume with driver options;
  - there is a tablespace or a replica;
  - a database does not accept connections;
  - the disk is short;
  - the running container uses other data or other files than the Compose files say;
  - the image tag comes from a variable (you then get the manual step instead).
- **Interruptions are safe.** Ctrl-C, a closed terminal or any failure puts
  everything back. Even after `kill -9` or a power cut, `pgcarry rollback` finds the
  interrupted run and recovers it. `plan` notices such leftovers too. Recovery never
  removes a copy that any container still uses, and two pgcarry commands never work
  on the same upgrade at once.
- **Do not start the database by hand during an upgrade.** If the service is started
  while pgcarry has the old data open (for the dump), pgcarry notices, stops its own
  copy at once and refuses to go on.
- **A rehearsal copy is reused only if nothing has run on it.** A second rehearsal or
  the real upgrade replaces an earlier rehearsal's copy only when its cluster ID and
  last checkpoint show no server has run on it since. Otherwise pgcarry refuses and
  you decide.

## Limits

- **Dump and restore, not `pg_upgrade --link`.** It works across any versions and
  image variants and with extensions, but takes time and space roughly in proportion
  to the database size. Very large databases (hundreds of GB) are better upgraded
  with `pg_upgrade`.
- **Verification is by counts, not content hashes.** Row counts and sequence values
  catch missing or extra rows. They do not prove every value is identical; that rests
  on pg_dump and pg_restore, PostgreSQL's own tools.
- **Not carried over:**
  - replication slots;
  - privileges (ACLs) on template1 itself (its settings and comment are carried);
  - extension versions (the new image's default version is installed; the plan lists
    the changes).
- **Not supported:** tablespaces, a separate WAL folder, replicas, and volumes on
  other drivers or with driver options.
- **Downtime:** the app is down during the copy. The plan tells you what will stop.
- **Tested with** PostgreSQL 13–18, Debian and Alpine images, and pgvector, on Docker
  29 and Compose v5. Kubernetes and non-Docker installs are out of scope.

## Roadmap

- An optional `pg_upgrade --link` path for very large databases.
- Content checks (hashes per table) on top of counts, as an opt-in.
- Tablespaces and a separate WAL folder.
- Release binaries.

## Development

```sh
make test          # unit tests
make test-docker   # everything, against real Postgres images (about 10 minutes)
```

The Docker tests start real Compose projects and check the results inside the
upgraded databases. They cover:
- official, Alpine and pgvector images;
- version pairs 13→17, 14→18, 15→18, 16→18 and 17→18;
- ICU and unusual locales, a `de_DE` locale under Alpine, a password file, and trust
  authentication;
- bind mounts with a numeric `user:`, and override files;
- odd database names, list settings, and an undeclared writer;
- an injected failure after every step, interrupts, a failed switch, rollback and
  cleanup.

## License

MIT, see [LICENSE](LICENSE).
