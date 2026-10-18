# pushprobe

Monitor what your Uptime Kuma or Gatus can't reach: a NAS on the home LAN, a
printer at the office, a service behind NAT or in another VPC. pushprobe runs
the checks on a small machine *there*, and pushes each result to an Uptime Kuma
push monitor or a Gatus external endpoint. Alerts, history and status pages
stay in the monitoring you already have.

```console
$ pushprobe check
ok    nas web   status 200                                       1ms
FAIL  printer   connection refused                               331µs (2 attempts)
ok    vpn cert  the certificate expires in 27 days (2026-11-15)  37ms
ok    disk      / 81.8% used (7.1 GiB free of 252.0 GiB)         31µs
4 checks: 3 ok, 1 failed. Nothing was sent.
```

## Why

Uptime Kuma checks from the one machine it runs on. "Remote executors" is its
most-wanted feature
([uptime-kuma#84](https://github.com/louislam/uptime-kuma/issues/84), 411 👍,
open since 2021), and Gatus has the same request
([gatus#64](https://github.com/TwiN/gatus/issues/64), 88 👍). Until then,
people:
- open services to the internet so they can be checked;
- run a second Kuma at every site, with its own alerts;
- write a cron job that curls a push URL. That has no timeout, no retries, no
  record of which checks exist, and it says "up" whenever it runs at all.

Both servers can already *receive* results (Kuma's push monitors, Gatus's
external endpoints). pushprobe is the missing half that produces them.

## What it does

- **Checks:**
  - **http(s):** expected status codes, text in the page, headers, redirects
    on or off;
  - **tcp:** a connection is accepted;
  - **ping:** ICMP;
  - **dns:** a record, against an expected answer;
  - **tls:** days until the certificate expires;
  - **disk** and **memory:** percent used;
  - **exec:** a command's exit code; off unless allowed.
- **Delivery:** each result goes to a Kuma push monitor (up or down, a short
  message, the round-trip time) and/or a Gatus external endpoint (success,
  error, duration).
  - While a server can't be reached, only the newest result waits. Kuma and
    Gatus date a result when it arrives, so replaying old ones would mislead.
  - Results older than the check's interval are dropped; a fresh one is due by
    then.
  - Kuma marks a push monitor down by itself when results stop arriving. So if
    the probe, its machine or its network dies, you hear about it.
- **Setup:** `pushprobe sync` creates the push monitors in Kuma for you and saves
  their tokens. It never deletes or pauses a monitor.
- **Running it:**
  - one static binary for Linux, macOS and Windows;
  - a 17 MB Docker image (no shell, not root);
  - a hardened systemd unit;
  - SIGHUP reloads the file, and a broken file keeps the old configuration;
  - `/healthz` and Prometheus `/metrics` are optional.

## Install

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/pushprobe
make build            # ./pushprobe
pushprobe version
```

Docker:

```sh
docker build -t pushprobe .
docker run -d --name pushprobe --restart unless-stopped \
  -v /srv/pushprobe:/etc/pushprobe:ro pushprobe
```

The container runs as a non-root user (uid 65532) and needs no extra
capabilities: ping uses the unprivileged ping sockets Docker allows. Give it
`--network host` if the checks must see the host's own networks exactly as the
host does. The mounted `pushprobe.env` must be readable by uid 65532
(`chown 65532 /srv/pushprobe/pushprobe.env`), or pass the variables with
`--env-file`. Run `sync` on the host, or mount the folder writable for it.

systemd: copy the binary to `/usr/local/bin/`, `contrib/pushprobe.service` to
`/etc/systemd/system/`, and your file to `/etc/pushprobe/pushprobe.yaml`, then
`systemctl enable --now pushprobe`. The unit runs as a throwaway user with only
`CAP_NET_RAW` (for ping), and reads `/etc/pushprobe/pushprobe.env` as root, so
that file can stay `0600`. systemd reads that file only at start: after `sync`
saved new tokens, `systemctl restart pushprobe` (a reload is enough for other
changes).

## Quick start

1. Write `pushprobe.yaml`:

   ```yaml
   targets:
     kuma: {url: "https://kuma.example.com", group: "Site A"}
   checks:
     - name: nas web
       http: {url: "http://192.168.1.10:5000", contains: "DiskStation"}
       kuma: {push_token: "${KUMA_NAS}"}
     - name: printer
       tcp: {address: "192.168.1.20:9100"}
       kuma: {push_token: "${KUMA_PRINTER}"}
   ```

2. Try the checks. Nothing is sent:

   ```sh
   pushprobe check
   ```

3. Create the push monitors in Kuma. sync logs in with your Kuma account, makes
   a token for each `${KUMA_…}` that has no value yet, and saves the tokens in
   `pushprobe.env` next to the file (mode 0600). The monitors go in the group the
   file names. Give each site (each pushprobe) its own group: sync finds
   monitors by name within it, so two sites sharing a group would share a
   "disk" monitor.

   ```console
   $ export PUSHPROBE_KUMA_USERNAME=admin PUSHPROBE_KUMA_PASSWORD=…
   $ pushprobe sync
   https://kuma.example.com (Uptime Kuma 2.5.6), group "Site A":
     + group "Site A"
     + push monitor "nas web" (heartbeat every 92s, a new token, token saved as KUMA_NAS)
     + push monitor "printer" (heartbeat every 92s, a new token, token saved as KUMA_PRINTER)
   Done: 2 monitors changed; 2 tokens saved in pushprobe.env (mode 0600).
   ```

   `--dry-run` shows the same plan and changes nothing. Running sync again
   changes nothing unless the file did.

   Prefer to click? Create a push monitor in Kuma yourself, copy the token from
   its push URL (`…/api/push/<token>`), and put it in `pushprobe.env` as
   `KUMA_NAS=<token>`.

4. Run it. It logs once at start and then only when a check changes state:

   ```console
   $ pushprobe run
   2026-10-18T19:52:52Z pushprobe v1.0.0 started: 2 checks, 2 reporting to Uptime Kuma, 0 to Gatus
   2026-10-18T19:52:53Z nas web: up (status 200)
   2026-10-18T19:52:55Z printer: DOWN (connection refused) after 2 attempts
   ```

## Examples

```yaml
defaults: {interval: 60s, timeout: 10s, retries: 1}   # these are the defaults
listen: "127.0.0.1:9469"                                # /healthz and /metrics

targets:
  kuma: {url: "https://kuma.example.com", group: "Office"}
  gatus: {url: "https://status.example.com", token: "${GATUS_TOKEN}"}

checks:
  - name: intranet
    http:
      url: "https://intranet.lan/login"
      expect_status: [200]
      contains: "Sign in"
      headers: {Authorization: "Bearer ${PUSHPROBE_INTRANET}"}
      insecure: true            # a self-signed certificate, for this check only
    kuma: {push_token: "${KUMA_INTRANET}"}

  - name: printer
    interval: 5m
    tcp: {address: "192.168.1.20:9100"}
    gatus: {endpoint: office_printer}   # the external endpoint's key: group_name

  - name: router
    ping: {host: 192.168.1.1, count: 3}
    kuma: {push_token: "${KUMA_ROUTER}"}

  - name: nas dns
    dns: {server: 192.168.1.1, query: nas.lan, type: A, expect: [192.168.1.10]}
    kuma: {push_token: "${KUMA_DNS}"}

  - name: vpn certificate
    interval: 6h
    tls: {address: "vpn.example.com:443", min_days_left: 21}
    kuma: {push_token: "${KUMA_VPN}"}

  - name: backups disk
    disk: {path: /mnt/backup, max_used_percent: 85}
    kuma: {push_token: "${KUMA_BACKUPS}"}
```

For Gatus, declare the external endpoint in Gatus's own configuration (a
`token`, and a `heartbeat.interval` so missing results count as failures). Its
key is `<group>_<name>`:

```yaml
external-endpoints:
  - {name: printer, group: office, token: "…", heartbeat: {interval: 10m}}
```

Running commands has to be switched on:

```yaml
allow_exec: true
checks:
  - name: backup age
    exec: {command: ["/usr/local/bin/check-backup", "--max-age", "26h"]}
    kuma: {push_token: "${KUMA_BACKUP}"}
```

The command runs without a shell, gets only `PATH`, `HOME`, `LANG` and `TZ` from
pushprobe's environment, and passes with exit code 0. The first line of its
output becomes the message.

## Reference

```
pushprobe run                 run the checks on their intervals and send the results
pushprobe check [NAME...]     run the checks once and print the results; nothing is sent
pushprobe validate            check the file, including where results go
pushprobe sync [--dry-run]    create or update the Kuma push monitors
pushprobe version

  -c, --config FILE     default $PUSHPROBE_CONFIG, else pushprobe.yaml
      --env-file FILE   default pushprobe.env next to the file, if there
```

**Variables.**
- `${NAME}` works in any text in the file. Only variables whose names start with
  `PUSHPROBE_`, `KUMA_` or `GATUS_` can be read, so a file can't send your other
  secrets anywhere.
- They come from the environment or from the env file; the environment wins. An
  empty value counts as none.
- `run` reads the env file again on SIGHUP.
- Inside `{…}`, quote values that use them: `token: "${GATUS_TOKEN}"`.
- `$${` stands for a literal `${`.

**Secrets.**
- Tokens never appear in logs, in `/metrics` or in sync's output.
- A password in a URL is shown as `xxxxx`.
- An http check's headers go only to the address it names. When a redirect
  leads to another host, or from https to http, they are dropped.

**Timing.**
- Intervals are at least 5s, and each check's timeout must be shorter than its
  interval.
- A failed check is tried again `retries` times, 2 seconds apart, before it
  counts as down.
- Checks start at random times within their first 5 seconds, so they don't all
  run at once.
- `sync` sets each Kuma heartbeat interval to the check's interval plus its
  longest possible run plus 10 seconds, so a slow run never shows as a missed
  heartbeat.

**Exit codes:** 0 fine (and `run` when it is stopped), 1 error, 2 bad usage, 3 a
check failed (`check`), 130 interrupted.

## Limits

- Kuma push monitors keep up or down, a message and a time. Response bodies,
  certificate details and the like stay with pushprobe.
- `sync` uses Kuma's own web socket API (as its web interface does). That API
  isn't documented as stable; it's tested against Uptime Kuma 2.x.
- Gatus external endpoints can only be declared in Gatus's own file, so `sync`
  doesn't create them.
- Disk checks need Linux, macOS or FreeBSD, and memory checks need Linux;
  `validate` says so elsewhere.
- Ping needs ping sockets (allowed in Docker, and on hosts where
  `net.ipv4.ping_group_range` includes the user), root, or `CAP_NET_RAW`. On
  Windows it needs Administrator.
- A dns check with a `server` asks that server directly. Without one, it asks
  the system's resolver, which also reads the hosts file.

## Roadmap

- More targets: Healthchecks.io, Prometheus Pushgateway, a plain webhook.
- More checks: SMTP and IMAP banners, SNMP, a database query.
- `sync` for Gatus by writing its external-endpoints block.
- Release binaries and a published image.

## Development

```sh
make test          # vet and unit tests
make test-server   # also against real Uptime Kuma and Gatus servers in Docker
```

The server tests start `louislam/uptime-kuma:2` and `twinproduction/gatus`. They
check up and down in both, and that Kuma marks a silent push monitor down by
itself.

## License

MIT. See [LICENSE](LICENSE).
