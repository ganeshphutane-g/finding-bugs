# kumafile

Keep your Uptime Kuma configuration in a file. `kumafile export` writes the
monitors, groups, notifications, tags, maintenance windows, status pages, proxies,
Docker hosts and remote browsers of an Uptime Kuma 2.x instance to readable YAML.
`kumafile diff` shows how the instance differs from the file, and `kumafile apply`
makes it match. Use it to keep the configuration in git and review changes, to
rebuild an instance, or to copy monitors from one instance to another.

Uptime Kuma (92k★) keeps all of this in its database, and the web interface is the
only way to change it. Configuring monitors from a file
([uptime-kuma#1354](https://github.com/louislam/uptime-kuma/issues/1354), open since
2022, 91 👍) has been called too hard for the current design. Import and export
([#6045](https://github.com/louislam/uptime-kuma/issues/6045), 28 👍) is not
planned either, and the old JSON backup is gone in 2.0. kumafile uses the same
Socket.IO API as the web interface, so it works remotely and with SQLite or MariaDB,
and needs nothing installed next to Kuma.

## What it looks like

```console
$ export KUMAFILE_URL=http://127.0.0.1:3910 KUMAFILE_USERNAME=admin KUMAFILE_PASSWORD=...
$ kumafile export
Wrote kumafile.yaml: 6 monitors, 1 notification, 2 tags, 1 maintenance window, 1 status page.
Wrote 2 secret(s) to kumafile.env (readable only by you; keep it out of git).
```

`kumafile.yaml` holds only what differs from what the web interface would set, with
references by name and secrets taken out:

```yaml
version: 1
tags:
  - name: prod
    color: '#059669'
  - name: home
    color: '#2563eb'
notifications:
  - name: Ops Telegram
    type: telegram
    default: true
    settings:
      telegramBotToken: ${KUMA_NOTIFICATION_OPS_TELEGRAM_TELEGRAMBOTTOKEN}
      telegramChatID: "-1001234567"
      telegramSendSilently: false
monitors:
  - name: Website
    type: group
    notifications: [Ops Telegram]
  - name: Home lab
    type: group
  - name: Homepage
    group: Website
    type: http
    url: https://example.org
    expiryNotification: true
    notifications: [Ops Telegram]
    tags: [prod]
  - name: API health
    group: Website
    type: keyword
    url: https://api.example.org/health
    keyword: '"status":"ok"'
    interval: 30
    retryInterval: 30
    maxretries: 2
    authMethod: basic
    basic_auth_pass: ${KUMA_MONITOR_API_HEALTH_BASIC_AUTH_PASS}
    basic_auth_user: monitor
    notifications: [Ops Telegram]
    tags: [prod]
  - name: NAS
    group: Home lab
    type: ping
    hostname: 192.168.1.20
  - name: Pi-hole DNS
    group: Home lab
    type: dns
    hostname: example.org
    dns_resolve_server: 192.168.1.2
maintenance:
  - title: Sunday patching
    description: OS updates on the home lab
    strategy: recurring-weekday
    timeRange:
      - hours: 3
        minutes: 0
      - hours: 4
        minutes: 0
    weekdays: [7]
    monitors: [Home lab / NAS]
status_pages:
  - slug: status
    title: Example status
    description: Is the website up?
    groups:
      - name: Website
        monitors: [Website / Homepage, Website / API health]
```

Edit the file (here: the API check every 60 seconds instead of 30, and a printer
added), then:

```console
$ kumafile diff edited.yaml
Comparing edited.yaml with http://127.0.0.1:3910 (Uptime Kuma 2.5.6):
  ~ monitor Website / API health
      interval: 30 -> 60
      retryInterval: 30 -> 60
  + monitor Home lab / Printer (ping)
1 to create, 1 to change, 0 to delete.

$ kumafile apply edited.yaml
...
Apply these changes to http://127.0.0.1:3910? [y/N] y
~ monitor Website / API health
+ monitor Home lab / Printer
Done: the instance matches the file.
```

The output above is from a real run against Uptime Kuma 2.5.6.

## Install

```sh
make build        # or, once published: go install github.com/ganeshphutane-g/kumafile@latest
```

kumafile is one static binary. It is tested against Uptime Kuma 2.0.0 and 2.5.6.

## Usage

```
kumafile export [-o FILE] [--secrets FILE] [--with-secrets]
kumafile diff   [FILE] [--prune]
kumafile apply  [FILE] [--prune] [-y]
kumafile login  [-o FILE]
```

FILE defaults to `kumafile.yaml`; `-` reads standard input (apply then needs `-y`).

**Login.** Credentials come from the environment, never from the command line:
- `KUMAFILE_URL`;
- `KUMAFILE_USERNAME` and `KUMAFILE_PASSWORD` (or `KUMAFILE_PASSWORD_FILE`);
- with two-factor login, also `KUMAFILE_TOTP` (a one-time code) or
  `KUMAFILE_TOTP_SECRET` (or `_FILE`), from which kumafile makes the codes.

Uptime Kuma allows about 20 logins a minute, for everyone together, and each
one-time code works only once. For scripts and CI, log in once with
`kumafile login -o token` and use `KUMAFILE_TOKEN_FILE=token` from then on. The
token stays valid until the password changes.

**Secrets.** Passwords, tokens, keys, webhook addresses, connection strings and
credential-carrying headers are written as `${KUMA_...}` and saved in a separate
env file (mode 0600, next to the YAML).
- The env file works with `set -a; . kumafile.env` and with docker compose's
  `env_file`. Values containing both a `'` and a backtick read differently in the
  two.
- `diff` and `apply` fill the references from that file, or from environment
  variables whose names start with `KUMA_`. A file cannot read any other variable,
  so a change to it cannot send, say, your password somewhere.
- Secrets are never printed: `diff` says `(secret, changed)`.
- `--with-secrets` keeps them in the YAML (then also 0600). Use it for copying, as
  in `kumafile export -o - --with-secrets | kumafile apply --url B -y -`.

**What apply does:**
- It creates and updates objects in dependency order, and only deletes with
  `--prune`.
- `--prune` deletes only within the sections the file has: a file with just
  `monitors:` never deletes notifications.
- Monitors are matched by group and name. A monitor that moved to another group is
  changed in place, keeping its history.
- Unknown fields (`ulr`, "did you mean url?") and monitor types the server does not
  have are refused before anything is written.
- Afterwards it reads the instance again and says whether it now matches.

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | done, or `diff` found no differences |
| 1 | an error, or you answered no |
| 2 | bad usage |
| 3 | `diff` found differences |
| 130 | interrupted |

## How the file works

- **Defaults are left out.** A field missing from the file means what the web
  interface would set (60-second interval, `200-299`, and so on). Remove a line to
  reset it. Fields kumafile does not know, such as one added by a later Kuma, are
  written when set and left alone when missing.
- **Names are identity.** Monitors are identified as `Group / Name`, status pages by
  slug, proxies by `protocol://host:port`, everything else by name, all
  case-insensitive. Two monitors with the same place are refused rather than
  guessed at.
- **Groups come first, then what they contain.** Notifications, tags, proxies and
  Docker hosts are referred to by name.
- **Status pages** keep their groups, the monitors shown, the "show URL" setting
  and custom links. Uploaded logos travel inside the file as PNG data.
- **Maintenance windows** keep only the settings their strategy uses.

## Limits

- **Not copied:** heartbeat history, incidents, users, API keys (their secrets
  cannot be re-created), general settings, and the "published" switch of status
  pages (the API cannot change it).
- **Paused groups:** a monitor inside one is reported as paused whatever its own
  setting, which the API does not show. kumafile leaves that setting alone.
- **Retry interval 0:** monitors created through the API can have it, but the
  server refuses to set it. Such a file applies to a new instance, but cannot lower
  an existing monitor to 0.
- **No 1.x:** Uptime Kuma 1.x has a different API and is not supported.

## Roadmap

- Selecting what to export or apply (`--only monitors`, by tag or group).
- Status page incidents and general settings.
- A container image for scheduled `apply` runs.

## Development

```sh
make test         # unit tests
make test-server  # also against real Uptime Kuma servers in Docker (KUMAFILE_IMAGE picks the version)
```

CI runs the whole suite against Uptime Kuma 2.0.0 and the latest 2.x.

## License

MIT
