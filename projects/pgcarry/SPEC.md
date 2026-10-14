# pgcarry: spec

## Problem

The official `postgres` Docker image cannot upgrade a database between major versions.
Change `postgres:14` to `postgres:17` and the container stops with "database files
are incompatible with server". The 18 image fails differently, because it moved the
data directory (checked on the real images, 2026-10-14):

| Image | PGDATA | Volume |
|---|---|---|
| up to 17 | `/var/lib/postgresql/data` | `/var/lib/postgresql/data` |
| 18 and later | `/var/lib/postgresql/18/docker` | `/var/lib/postgresql` |

Both failures send people to
[docker-library/postgres#37](https://github.com/docker-library/postgres/issues/37),
"Upgrading between major versions?". It has been open since 2014 and has 278 👍. The
existing tools:
- tianon/docker-postgres-upgrade (1,386★) calls itself a proof of concept, and its
  images carry only the official Debian builds, so no PostGIS, pgvector or Alpine.
- pgautoupgrade (1,191★) swaps in an image that upgrades the files in place.

Self-hosters run Postgres inside dozens of Compose apps (Nextcloud, Immich, Gitea,
Authentik, Paperless…). They need an upgrade that cannot lose their data. PostgreSQL
14 support ends in November 2026 (to be confirmed on postgresql.org before quoting).

## Who it is for

Self-hosters and small teams running Postgres in Docker Compose, usually one database
per app, from megabytes to tens of gigabytes. They know `docker compose`, not
`pg_upgrade`.

## What v1 does

`pgcarry plan [-p PROJECT_DIR] [SERVICE] --to VERSION` reports and changes nothing:
- **Finds the service:** reads the project with `docker compose config`, picks the
  service that runs Postgres (or the one named), its image, volume and environment.
- **Reads the current cluster:**
  - major version (`PG_VERSION`), databases and sizes, extensions per database,
    roles;
  - encoding, locale and collation provider, data checksums, auth method.
- **Checks the target image:** starts it briefly, without your data, to list the
  extensions it provides and its defaults (data checksums are on by default from
  18).
- **Gives a verdict:** blockers (an extension missing from the target image, not
  enough disk for a dump plus a second copy, a version or tag it cannot work out) and
  the exact changes it would make to the Compose file.

`pgcarry upgrade [-p DIR] [SERVICE] --to VERSION [--image IMAGE] [--yes]` runs:
1. **Stops the apps** that use the database (services that depend on it, or share its
   network and point at it). The database service itself stays up. Plans and asks
   first.
2. **Dumps:**
   - roles and globals with `pg_dumpall --globals-only`;
   - each database with `pg_dump -Fc`.

   It uses the *new* version's client tools, connecting to the old server, as the
   PostgreSQL docs recommend. Dumps go to a backup folder on the host and are checked
   with `pg_restore --list`, plus a checksum file.
3. **Stops the old database service.** Its volume is never written by pgcarry and is
   kept for rollback.
4. **Creates a new volume**, carrying Compose's labels, and initialises a cluster with
   the target image. It uses the same encoding, locale and checksum setting, and the
   service's own `POSTGRES_USER`/`POSTGRES_DB`/password settings.
5. **Restores** the globals, then each database, with errors stopping the run.
6. **Verifies:**
   - every table's exact row count, every sequence's value, and the list of schemas,
     tables, views, functions, extensions and roles, old against new;

   Any difference stops the upgrade before the switch.
7. **Switches:**
   - edits the Compose file in place, touching only the image tag and the volume
     line, and keeping a copy of the original;
   - for 18 and later, the volume moves to `/var/lib/postgresql`;
   - shows the diff and asks first;
   - starts the service and waits for it to be healthy, then starts the apps again.

`pgcarry rollback [-p DIR]` puts the original Compose file back and starts the old
service on its untouched volume. `pgcarry cleanup` removes the old volume and the
dumps, after confirmation.

It refuses rather than guesses:
- the image tag comes from a variable or an override file, and pgcarry cannot edit
  it safely (it then prints the manual steps instead);
- the data is in a bind mount it cannot read;
- the service is a replica;
- there is not enough disk;
- there is a tablespace outside the volume.

Every step is logged, and an interrupted run can be resumed or rolled back.

## Non-goals for v1

- In-place `pg_upgrade --link` (fast for large clusters, but the user's own images
  rarely carry both versions). Planned for later, as an option.
- Downgrades, replicas and high-availability setups, Kubernetes, non-Docker installs.
- Upgrading extensions' own versions beyond what restore does (`ALTER EXTENSION …
  UPDATE` is reported, not run).

## Tech choices

- **Go**, running the `docker` command-line tool (which Compose users always have),
  so Docker contexts, remote hosts and streaming large dumps work as they do for the
  user. Compose files are read with `docker compose config --format json`.
  They are edited by position, using gopkg.in/yaml.v3 nodes, so comments and
  formatting stay intact.
- **All Postgres work runs inside containers** of the user's own images: no Postgres
  on the host.
- **Tests run real Postgres images** in Docker and compare data before and after:
  - versions 13→17, 14→18 and 16→18;
  - Debian and Alpine images, PostGIS and pgvector;
  - several databases, roles with passwords, a custom `POSTGRES_USER`;
  - a non-default locale, checksums on and off;
  - an interrupt at every step.

## Milestones

1. **Skeleton:** Engine client, Compose discovery, `plan` with the full report.
2. **Copy:** dump, check the dump, new volume, initialise with matching settings,
   restore, verify (`upgrade --no-switch` works end to end).
3. **Switch:** Compose edit by position with diff and confirm, start and health
   check, restart the apps, `rollback`, `cleanup`, resume after interrupt.
4. **Matrix:** versions, variants, extensions, locales, a sizeable database (≈1 GB),
   interrupt tests.
5. **Release:** README with real output, full reviews, LICENSE, CHANGELOG, v1.0.0.
