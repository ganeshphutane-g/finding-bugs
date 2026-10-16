# Changelog

## 1.0.0 — 2026-10-17

First release.

- `export`:
  - writes monitors (every type), groups, notifications, tags with values,
    maintenance windows, status pages (groups, links, logos), proxies, Docker
    hosts and remote browsers to YAML;
  - leaves out what matches the web interface's defaults;
  - refers to objects by name;
  - puts secrets in a separate 0600 env file as `${KUMA_...}`.
- `diff`:
  - shows what `apply` would create, change (field by field) or delete;
  - never prints a secret;
  - checks references, field names and monitor types.
- `apply`:
  - makes the instance match, in dependency order;
  - keeps fields it does not manage;
  - changes moved monitors in place;
  - deletes only with `--prune`, and only in the sections the file has;
  - reads the instance back to confirm the result.
- `login`: prints a token, which saves logins (Uptime Kuma limits them) and
  one-time codes.
- Works over Uptime Kuma's own Socket.IO API, remotely, with SQLite or MariaDB.
  Every connection and call has a timeout.
- Tested against Uptime Kuma 2.0.0 and 2.5.6: a copy to an empty instance gives
  the same file, and a second apply changes nothing.
