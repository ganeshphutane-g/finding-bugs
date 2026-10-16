# kumafile: spec

## Problem

Uptime Kuma (92.5k★) keeps every monitor, notification, tag, maintenance window and
status page in its database. The only way to change them is to click through the
web UI. Its JSON backup feature, which was never complete, is gone in 2.0. That leaves
self-hosters with a few unmet needs:

- keeping their monitoring config in git, reviewing changes, and rebuilding an
  instance from a file;
- copying monitors from one instance to another (staging → production, an old
  server → a new one);
- seeing what changed since last week.

Evidence (checked 2026-10-17 through GitHub search):
- [uptime-kuma#1354](https://github.com/louislam/uptime-kuma/issues/1354),
  "Configure monitors using config file": 91 👍, open since 2022-03, updated
  2026-03. A maintainer wrote it is too hard in the current design.
- [uptime-kuma#6045](https://github.com/louislam/uptime-kuma/issues/6045),
  "Add import/export (backup/restore) for settings": 28 👍, open since 2025-08,
  updated 2026-09. A collaborator wrote that a JSON dump will not be maintained
  upstream.
- [lucasheld/uptime-kuma-api#75](https://github.com/lucasheld/uptime-kuma-api/issues/75),
  "This project seems to be abandoned". That Python wrapper (396★) and the Ansible
  collection built on it (189★) target 1.x.

Existing tools and why they fall short:
- **AutoKuma** (909★): creates monitors from Docker labels or files. It cannot export
  an existing instance or show a diff, and it breaks on monitor types it does not
  know.
- **terraform-provider-uptimekuma** (70★): needs Terraform and hand-written HCL for
  every existing monitor. It has no export.
- **replicator-kuma** (31★): copies the whole database between instances. It does not
  make a readable file.
- **KumaCompanion** (46★): a CLI for single actions, not a declarative config.

## What v1 does

`kumafile` is one binary that talks to Uptime Kuma 2.x through the same Socket.IO API
its web UI uses, so it works remotely and with both SQLite and MariaDB.

- **`kumafile export`** writes the instance's config to a readable YAML file:
  - monitors of every type, with their group, notifications, tags (with values),
    proxy, Docker host and remote browser named rather than numbered;
  - groups;
  - notifications;
  - tags;
  - maintenance windows;
  - status pages, with their monitor groups;
  - proxies;
  - Docker hosts and remote browsers.

  Default values are left out so the file stays short. Secrets (passwords,
  tokens, private keys, notification credentials) are replaced by `${NAME}`
  references and written to a separate `.env` file, unless `--with-secrets` is
  given.
- **`kumafile diff FILE`** compares the file with the instance and lists what
  `apply` would create, change (field by field) and, with `--prune`, delete. It
  changes nothing.
- **`kumafile apply FILE`** shows the diff, asks (or `--yes`), and makes the
  instance match. `${NAME}` references are filled from the environment. Objects
  missing from the file are kept unless `--prune` is given. Apply is idempotent:
  running it again changes nothing.
- **Copy between instances:** `kumafile export --url A | kumafile apply --url B -`.

**Identity.** Objects are matched by name, since IDs differ between instances:
- monitors by their group path and name;
- status pages by slug;
- the rest by name.

A file or instance with two objects of the same identity is refused with a clear
message rather than guessed at.

**Login** uses a username and password (and a TOTP code if 2FA is on), or a saved
JWT. Credentials come from the environment, never from the command line. An instance
with authentication disabled needs none.

## Non-goals for v1

- Heartbeat history, uptime statistics, incidents, users, API keys (their secret
  cannot be re-created), and general settings.
- Uptime Kuma 1.x: its API differs, and 2.0 has been out since October 2025.
- Watching the file and applying continuously: run `apply` from cron or CI instead.

## Tech choices

- **Go:** one static binary, which fits self-hosters (download, or `docker run`).
  Dependencies:
  - `github.com/coder/websocket` for the WebSocket transport (the Socket.IO
    framing on top is small and written here);
  - `gopkg.in/yaml.v3` for YAML with stable key order and comments.
- **Tests** run against real Uptime Kuma 2.x containers (louislam/uptime-kuma:2.0
  and :2):
  - a round trip, export → apply on a fresh instance → export, must give the same
    file;
  - a second apply must change nothing;
  - a diff test for each kind of object;
  - every monitor type the server lists.
- **CI** runs the same matrix in GitHub Actions.

## Milestones

1. **Client:** Socket.IO v5 over WebSocket (connect, events, acks, ping), login
   (password, TOTP, JWT, no auth), reading every list, and `kumafile export` with
   secrets split out.
2. **Model and diff:**
   - defaults per monitor type;
   - normalising server values (status codes, JSON fields, IDs to names);
   - identity keys and duplicate detection;
   - `kumafile diff`.
3. **Apply:** create, update and (with `--prune`) delete each kind of object in
   dependency order (tags, notifications, proxies and hosts first, then groups
   parent-first, monitors, maintenance, status pages), with the round-trip and
   idempotence tests.
4. **Release:** CI matrix (2.0, latest 2.x), README with real output, two full
   reviews, LICENSE, CHANGELOG, v1.0.0.
