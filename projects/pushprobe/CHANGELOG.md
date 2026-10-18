# Changelog

## 1.0.0 — 2026-10-19

First release.

- `pushprobe run` runs checks on their intervals inside networks your monitoring
  can't reach. It pushes each result to an Uptime Kuma push monitor (up or down,
  a message, the round-trip time) and/or a Gatus external endpoint.
  - Only the newest undelivered result waits while a server is down.
  - Retries back off.
  - A wrong token or a deleted monitor is not retried.
  - SIGHUP reloads the file and the env file; a broken file keeps the old
    configuration.
  - Optional `/healthz` and Prometheus `/metrics`.
- Checks:
  - http(s): status, text, headers, redirects; headers are never sent to
    another host;
  - tcp;
  - ping (unprivileged ping sockets, else raw);
  - dns: straight to a named server, or through the system's resolver;
  - tls expiry;
  - disk and memory use;
  - exec: opt-in with `allow_exec`, no shell, a minimal environment.
- `pushprobe check` runs every check once and sends nothing. `validate` checks
  the file, with line numbers and suggestions for typos.
- `pushprobe sync` sets up the push monitors in Uptime Kuma 2.x:
  - it creates a group per site and the missing monitors;
  - it adopts existing monitors by name;
  - it keeps the heartbeat interval long enough that a slow run never shows as
    missed;
  - it saves new tokens in `pushprobe.env` (0600). It never deletes or pauses a
    monitor, and keeps settings it does not manage.
- `${NAME}` variables, limited to `PUSHPROBE_*`, `KUMA_*` and `GATUS_*`, from the
  environment or `pushprobe.env`. Tokens never appear in logs or output.
- A 17 MB distroless Docker image (non-root, no extra capabilities for ping), and
  a hardened systemd unit.
- Tested against real Uptime Kuma 2.5.6 and Gatus servers. The tests cover:
  - up and down results in both servers;
  - Kuma marking a silent push monitor down by itself;
  - sync and run working together.
