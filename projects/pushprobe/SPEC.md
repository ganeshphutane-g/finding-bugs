# pushprobe: spec

## Problem

Uptime Kuma and Gatus check services from the one machine they run on. Services in
other places, such as a home LAN, a branch office, a customer site, a VPC or
behind NAT, can't be reached from there. Today the options are:
- open those services to the internet;
- run a second Kuma or Gatus at each site, with its own alerts and its own UI;
- write a cron script that curls a push URL, with no timeouts, retries, history
  or way to see which checks exist.

Evidence (checked 2026-10-18 in GitHub search):
- [uptime-kuma#84](https://github.com/louislam/uptime-kuma/issues/84) "Remote
  Executors": 411 👍, open since 2021-07, updated 2026-08-14. Uptime Kuma has
  92k★. Duplicates #528, #390 and #2609 were closed as not planned. #819
  (resource heartbeats) has 63 👍.
- [gatus#64](https://github.com/TwiN/gatus/issues/64) "Distributed Deployment
  Configuration": 88 👍, open since 2020-12. Gatus has 12k★.
- Upstream: no executor or agent PR in uptime-kuma. Kuma 2.1's Globalping monitor
  uses community-hosted public probes (linuxiac, globalping.io blog); those give
  outside vantage points but don't check inside private networks (inference).
- Existing tools: tobias-carlbom/uptime-kuma-agent 20★ (Python, host health
  only), kuma-scout 16★, t0mer/uptimekuma-agent 6★ (heartbeat only),
  uptime-koguma 1★. None of them reports to Gatus, and none has retries,
  timeouts, a dry run or setup of the monitors.

## Verified behaviour (2026-10-18, Uptime Kuma 2.5.6 and Gatus latest)

- **Kuma push monitors:** `GET` or `POST /api/push/<token>?status=up|down&msg=…&ping=…`
  answers 200 `{"ok":true}`. An unknown token gets 404 `{"ok":false,"msg":"Monitor
  not found or not active."}`. Kuma accepts any status or ping value without
  complaint, so pushprobe must send only well-formed values. A push monitor goes
  down when no push arrives within its interval (from Kuma's documentation; check
  it in the integration tests).
- **Gatus external endpoints:**
  `POST /api/v1/endpoints/<group>_<name>/external?success=true|false&error=…&duration=150ms`
  with `Authorization: Bearer <token>` answers 200. A wrong token gets 401
  `invalid token`. The result is stored with its error and duration. A config
  with only external endpoints is refused ("at least one endpoint or suite"), so
  the README should say this.

## What v1 does

One static binary, `pushprobe`, configured by one YAML file:

```yaml
defaults: {interval: 60s, timeout: 10s, retries: 1}
targets:
  kuma: {url: https://kuma.example.com}        # push tokens per check
  gatus: {url: https://status.example.com, token: ${GATUS_TOKEN}}
checks:
  - name: nas web
    http: {url: http://192.168.1.10:5000, expect_status: [200, 301], contains: "Synology"}
    kuma: {push_token: ${KUMA_NAS_TOKEN}}
  - name: printer
    tcp: {address: 192.168.1.20:9100}
    gatus: {endpoint: office_printer}
  - name: router dns
    dns: {server: 192.168.1.1, query: nas.lan, type: A, expect: [192.168.1.10]}
  - name: vpn cert
    tls: {address: vpn.example.com:443, min_days_left: 14}
  - name: disk
    disk: {path: /, max_used_percent: 90}
```

**Checks:** http(s), tcp, ping (ICMP, falling back to unprivileged UDP ping or
TCP), dns, tls certificate expiry, disk and memory use, and `exec` (a command's
exit code, off unless enabled).

**Reporting:** each result goes to a Kuma push monitor (status, a short message,
ping in ms) and/or a Gatus external endpoint (success, error, duration). Failed
deliveries are retried with backoff, and buffered for a bounded time while the
server can't be reached. A delivery is never mixed up with a failed check: when
the probe can't reach Kuma, Kuma marks the monitor down on its own, which is
correct.

**Commands:**
- `pushprobe run`: the agent. It handles SIGHUP (reload), has a /healthz and
  optional /metrics, logs one line per state change, and never logs secrets.
- `pushprobe check`: every check once, results printed, nothing sent (dry run).
- `pushprobe sync`: creates or updates the Kuma push monitors for the checks
  (named `<prefix> <check>`, interval and retries matched to the check's, in a
  group), using kumafile's Socket.IO client, and writes their tokens to an env
  file. Optional: tokens can also be pasted in by hand.
- `pushprobe validate`: config errors with line numbers, unknown keys, missing
  variables.

**Safety and secrets:**
- `${VAR}` reads only `PUSHPROBE_*`, `KUMA_*` and `GATUS_*` variables, as
  kumafile does.
- Tokens never appear in output.
- TLS is verified by default; `insecure: true` applies to one check only.
- `exec` stays off unless the config enables it.

**Packaging:** a Docker image (distroless, non-root, with `NET_RAW` noted for
ICMP), a systemd unit, and release binaries for linux, macOS and windows on
amd64 and arm64.

## Non-goals for v1

- A UI or result storage: Kuma and Gatus already have both.
- Pulling config from the server, or remote control.
- Other targets (Healthchecks.io, Prometheus Pushgateway), though delivery is an
  interface so they can be added later.

## Tech choices

- Go, using the standard library plus a YAML parser (the same one kumafile uses).
  kumafile's Socket.IO client is copied in for `sync`.
- Tests:
  - every check against local servers (httptest, a TCP listener, a DNS server in
    the test, certificates with chosen expiry dates);
  - delivery against real `louislam/uptime-kuma:2` and `twinproduction/gatus`
    containers: up, down, a missed push turning the monitor down, retries while
    the server is stopped, `sync` creating and updating monitors.

## Milestones

1. **Core:** config and validation, the check runner (intervals, timeouts,
   retries, jitter), http, tcp, dns, tls, disk and memory checks, `check`.
2. **Delivery:** Kuma and Gatus senders with retry and buffering, `run`, reload,
   /healthz, /metrics; integration tests against real servers.
3. **Setup:** `sync` for Kuma, ping, `exec`, the Docker image, the systemd unit.
4. **Release:**
   - README with a real demo;
   - two full reviews (correctness; security and usability);
   - CI;
   - CHANGELOG.
