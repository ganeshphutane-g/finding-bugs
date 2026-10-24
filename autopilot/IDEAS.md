# Ideas

Researched candidates, with evidence. Newest research first.
Scores are 1–5 (higher is better): demand, gap, feasibility, maintenance (5 = light), audience fit.

## Research round 17 — 2026-10-25 IST

One research pass covering two things:
- **Round 16's runners-up again:** composeremote, Compose watch for
  Podman, zfsunlock, and ZFS to S3.
- **Trackers not scanned before:** gluetun, Authelia, Pangolin, Beszel,
  Dokploy, wg-easy, ntfy, rest-server, Grafana Alloy, Technitium, Incus, and
  more of docker/compose.

"verified" means read from a GitHub search result or the page itself on
2026-10-24; "web" means a web-search summary only.

### Chosen (provisional): host-side hooks for Compose through provider services (project #16)

Since v2.36.0 (web), Compose has "provider services". With
`provider: type: <binary>`, Compose runs that binary on the machine Compose
runs on:
- `<binary> compose --project-name P up --opt=val <service>` when the
  project comes up;
- `down` at teardown, and `stop` in reverse dependency order.

Services that depend on the provider start after it, and they get the
values it reports with `setenv`.

The tool is one Go binary that offers declarative host actions:
- run a host command before the dependent services start, and another on
  `down`;
- create bind-mount directories with an owner and mode;
- later, `upload` local files into a named volume. That covers
  composeremote (compose#11867).

**Demand:**
- docker/compose#6736 "Hooks to run scripts on host before starting any
  containers": 225 👍 and 31 ❤️, 2019-05 to 2023-04, closed as "completed".
  The researcher reads the closing comment as declining host hooks; that
  comment has 163 👎 (seen by the researcher). The 👍 count and dates are
  verified.
- #1341 "Hooks/Plugins support to run arbitrary scripts": 90 👍, closed in
  2017 (verified).
- #468 "Add pre/post command option": 35 👍 (seen by the researcher).
- #2781, bind-mount directories created as root: 11 👍 (seen).
- Docker forum threads (web).

**Gap:**
- Compose's own `post_start`/`pre_start` hooks run inside containers.
- People use a Makefile or a wrapper script, which Portainer, Dockge and
  plain `docker compose` bypass.
- The provider plugins that exist do other jobs: docker-vals (38★, secrets)
  and a few 0★ ones. No generic host-hook provider was found.

**Scores:** 4/4/5/4/5 = **22**, as scored by the researcher.

**Check before the spec:**
- the provider protocol in today's Compose: whether a provider that prints
  nothing is accepted, the working directory and project directory, and
  whether DOCKER_HOST and the context reach the provider (needed for
  `upload`);
- that the name is free (`composehook` is taken by an archived 18★ tool);
- what UIs that bundle their own Compose do.

**Risks:**
- The provider protocol is young and may change.
- Hooks fire only on `up` and `down`, not when dockerd restarts containers.
- Host commands driven from a compose file look bad security-wise
  (mitigated: the binary must be installed by the user).

### Runners-up

- **composeremote:** fold it into the pick as its `upload` action.
  - Demand: compose#11867 is the top-voted open issue in docker/compose
    (30 👍).
  - Every upstream PR was closed unmerged.
  - mutagen-compose is frozen on Compose v2.23.3, and its v0.18 is the last
    series (seen by the researcher).
- **zfsunlock:** 18. New finding: ZFS works on GitHub's ubuntu-latest
  runners (zfsutils-linux + modprobe; several repos' CI, seen by the
  researcher). Unattended unlock at boot still needs VM tests, and a bug
  can lock people out of their data.
- **WireGuard on-demand for Linux (wgroam):** 19. Its counted demand is weak
  (netbird#5278 4 👍), but it would reuse tsroam's code.

### Rejected in round 17

- **Compose watch for Podman:** `podman compose` delegates to the Docker
  Compose binary when it is installed, and posts report that `watch` works
  against podman.sock (web). Check in 10 minutes before ever reconsidering.
- **ZFS to S3 raw incrementals:** no counted demand.
- **A gluetun hostname endpoint:** the maintainer is working on it.
- **Reloading containers when config changes:** Compose watch
  `sync+restart` does this.
- **Docker MTU behind an exit node:** a possible tsroam feature (MSS clamp).
- **Host access to Docker's internal DNS:** the same as round 8's runner-up.
- **podman-compose `configs:`:** use Docker Compose v2 on the Podman socket.

## Research round 16 — 2026-10-24 IST

One research pass over trackers not scanned before:
- Immich, Paperless-ngx, Vaultwarden;
- docker/compose, podman-compose, moby/libnetwork;
- more of Tailscale, plus Netbird, ZeroTier, Coolify, Portainer;
- Kopia, borg, zrepl, sanoid, mergerfs, Cockpit;
- systemd-networkd/resolved, Nextcloud desktop/AIO, rclone, Jellyfin;
- sweeps of archived repos.

Not scanned:
- containers/podman and gluetun: the search returned an error;
- the Jellyfin and Pi-hole forums: DNS failed.

"verified" means read from a GitHub search result on 2026-10-24. "web" means
a web-search summary only.

### Chosen: tsroam (was "tsexit"), Tailscale exit-node policy for Linux (project #15)

**Checks done 2026-10-25 (run 14), in a lab with headscale and tailscaled
1.104.2:**
- **Competitors:** kafu-densha/tsexit is an fzf exit-node picker (manual, not
  network-aware), so it does not compete. `tsroam` is free. No Linux tool
  and no upstream PR for an automatic exit node or a kill switch were found.
- **`tailscale set --exit-node=IP`:** returns in about 20 ms. Table 52 gets
  `default dev tailscale0` and also the LAN prefix; the LAN is reachable
  again only with `--exit-node-allow-lan-access`.
- **Exit nodes forward only to public addresses.** A 10.x "internet" host
  is rejected with "acl". Lab hosts on the far side need public-range
  addresses.
- **Leaks:**
  - `kill -9` of tailscaled leaves its ip rules but removes the tun
    routes, so traffic goes straight out of the LAN interface;
  - `tailscale down` leaks by design;
  - a restart restores the exit route in about 1 s.
- **Kill switch:** an nftables output chain that accepts lo, tailscale0,
  mark 0x80000/0xff0000 (tailscaled's own sockets), DHCP (and the LAN when
  allowed), and drops the rest. It blocked the leak with tailscaled killed
  and with `tailscale down`. A restarted tailscaled reconnected through it.
- **PR #18695** (configurable Linux packet marks): still open, last updated
  2026-06-16, so the mark must be configurable.
- **Decision:** a separate tool, with tsdirect's fingerprint code copied in
  and adapted (the same author's code, in this repo).

#### Original notes (provisional pick, round 16)

A daemon that turns the exit node on automatically on networks that are not
one of yours, and off on trusted ones. Trusted networks are recognised by the
same fail-closed fingerprints as tsdirect (gateway MAC by fresh ARP, a bound
TCP probe), and later by Wi-Fi SSID through NetworkManager. Optional:
- a kill switch, so no traffic leaves outside the tailnet while the exit
  node is meant to be on (including while tailscaled restarts);
- later, bypassing the exit node for chosen CIDRs or domains.

**Demand:**

| Issue | Request | 👍 | Open since | Checked |
|---|---|---|---|---|
| tailscale#3302 | automatically use an exit node on untrusted or new Wi-Fi | 127 | 2021-11 (updated 2026-06-22) | verified |
| #7766 | bypass exit nodes for certain domains | 50 | 2023-04 | verified |
| #12228 | split tunnelling in the desktop app | 38 | | not verified |
| #10773 | kill switch while tailscaled updates | 23 | | not verified |
| #13992 | exit node on demand | 20 | | not verified |

**Gap:** no Linux tool was found.
- woopstar/AutoExitNode (1★) is Windows only.
- leolovenet/tailscale-routes (8★) is macOS only.

**Upstream:** no PR was found for an automatic exit node. PR #18695
("configurable Linux packet marks", open) could affect a kill-switch design.

**Scores:** 5/4/4/3/4 = **20**. It ties composeremote. tsexit is chosen
because:
- its demand is several times larger;
- the headscale lab and tsdirect's fingerprint and netlink code already
  exist and are tested;
- a wrong verdict fails safe: the exit node stays on.

**Check before the spec:**
- what `tailscale set --exit-node=` does on Linux, and how fast;
- whether LocalAPI or the CLI is the better control;
- the kill-switch mechanism (nftables with Tailscale's fwmark), and PR
  #18695;
- the name: `tsexit` is taken by kafu-densha/tsexit (0★, shell, created
  2026-04-20, not read). Read it first: it may be a competing tool. Either
  way, pick another name (`tsdirect exit`, a mode of tsdirect, is one
  option);
- whether to build it as a separate tool or as tsdirect's second mode. A
  separate tool means a simpler config; sharing the code means copying
  packages inside this repo, not depending on another module.

**Risks:**
- Much of the demand is from mobile and desktop users on other OSes.
- Bypassing by domain is hard with MagicDNS; it is out of v1.
- Tailscale could ship this itself.

### Runners-up

- **composeremote:** `docker compose up` against a remote DOCKER_HOST with
  local bind-mount, config and secret files uploaded into volumes.
  - **Demand:** docker/compose#11867 "Support remote DOCKER_HOST by copying
    configs and secrets files instead of bind mounting them", 30 👍
    (45 reactions), open since 2024-05-30 (verified). Several upstream PRs
    were closed unmerged (seen by the researcher, not re-checked). There are
    forum reports from Caddy and Docker (web).
  - **Existing tools:** mutagen-io/mutagen-compose (112★, Go, last push
    2025-02, verified) syncs files with Mutagen sessions, so it is partly a
    gap. pardnchiu/PodRun (3★) targets Podman and k3s.
  - **Scores:** 3/3/5/4/5 = 20.
  - **Next round:** read #11867's thread, and what mutagen-compose does with
    remote engines today.
- **zfsunlock** (re-checked):
  - **Demand:** openzfs#6824 has 84 👍 and #12649 has 17 👍 (seen by the
    researcher).
  - **Existing tools:** many small single-initramfs tools.
  - **Scores:** 19. Still: a bug locks people out of their data, and ZFS in
    CI is unproven.
- **Docker macvlan host-access shim:** 19. Still no counted demand for the
  shim itself.
- **Immich WebDAV/SFTP gateway:**
  - **Demand:** immich discussion #1687 has 106 👍.
  - **Existing tools:** immich-sftp-server (33★) and immich-webdav-wrapper
    (25★) exist.
  - **Scores:** 17. Immich's fast-moving API means a heavy maintenance
    burden.
- **ZFS snapshots to S3 with raw incremental chains:** 17.
  - zfsbackup-go (378★) has had no commit since 2023-02.
  - There is no high-👍 issue.
- **Compose watch for Podman:** podman-compose#792 has 104 👍 (seen). 17.
  First check whether `docker compose watch` already works against the
  Podman socket; if it does, there is no gap.

### Rejected in round 16

- **Already covered:**
  - Duplicati monitor (wsj-br/duplistatus);
  - several tailnets at once (Hydrascale, 43★);
  - SMTP to Microsoft Graph (email-oauth2-proxy, SMTP2Graph);
  - DoH in systemd-resolved (dnsproxy, dnscrypt-proxy);
  - an exit node through a commercial VPN (tailguard, 610★);
  - wake-on-demand proxying (Sablier).
- **Too niche, too weak, or needing paid services:**
  - a Corso successor (needs a Microsoft 365 tenant);
  - per-device traffic accounting;
  - Cockpit Docker support;
  - a mergerfs mover;
  - Docker volume quotas;
  - a Swarm real client IP;
  - Paperless CSV export.
- **Being done upstream:** Nextcloud Linux VFS, Immich smart albums, Kopia
  notifications.

## Research round 15 — 2026-10-23 IST

One research pass over about 40 trackers not scanned before (Frigate,
OPNsense, Authentik, Headscale, Caddy, HAProxy, openzfs, Nextcloud, Synapse,
borg, Tailscale, cloudflared, k3s, the Prometheus exporters, fail2ban,
Duplicati and others), plus repository, PR and web searches. The pick's
numbers were re-checked on 2026-10-23.

### Chosen: tsdirect, LAN traffic off Tailscale subnet routes when on that LAN (project #14)

On Linux, routes accepted with `--accept-routes` sit in table 52, behind ip
rules (5210–5270) checked before the main table. So a machine on the LAN
that a subnet router advertises sends its LAN traffic through the tailnet.
That is slow, and it breaks when Tailscale is down.

tsdirect would:
- watch netlink;
- when an accepted prefix is directly connected and the network is
  recognised (gateway MAC, or a probe), add an `ip rule to <prefix> lookup
  main` ahead of Tailscale's rules;
- remove the rule when the machine leaves that network;
- keep the rules in place across tailscaled restarts;
- export /metrics.

**Demand** (checked on 2026-10-23 unless marked web):
- tailscale#1227 "When local route is available to a subnet, bypass tailscale
  subnet relay": 88 👍, open since 2021-01;
- #7947 "accept routes at lower priority on Linux": 38 👍, updated
  2026-10-08;
- #6231 "accepted subnet routes should not have a higher priority than main
  table": 22 👍;
- #3646 "exit-node-allow-lan-access for routed local subnets": 22 👍;
- forum threads on Tailscale, OpenWrt, NixOS and GL.iNet (web), and
  Tailscale's own docs page on overlapping subnet routes (web).

**Upstream:**
- PR #21591 "filter accepted subnet routes" has been open since 2026-09-30,
  with no activity after that day. It adds static allow and deny lists; by
  the reviewer's reading it has no automatic LAN detection.
- Draft #5770 has been stalled since 2023.
- If #21591 merges, tsdirect can drive its deny list instead of ip rules.

**Gap:** no tool found. jarovkipt/lanfirst (0★, 2026-06, no description)
could not be read. The workarounds are a static ip rule (wrong on roaming
Wi-Fi) or a less specific prefix.

**Scores:** 4/4/4/3/5 = **20**.

**Check before the spec:**
- headscale plus tailscaled with kernel TUN in network namespaces (a LAN,
  a subnet router and a client);
- a strict identity check that fails closed;
- PR #21591 again;
- the name: `tsdirect` is free (checked 2026-10-23); "lanfirst" and
  "lanpath" are taken.

**Risks:**
- Linux only, while much of the demand is from Mac and Windows users;
- Tailscale could change its rule priorities;
- upstream could add LAN detection;
- a wrong "on the home LAN" verdict sends traffic onto a foreign LAN.

### Runners-up

- **Exit node on untrusted networks (Linux):** tailscale#3302 126 👍, #13992
  20 👍; 17. Best as a later phase of tsdirect (the same network-identity
  logic).
- **Duplicati report receiver and monitor:** duplicati#3362 22 👍,
  #1808 12 👍; partly covered (dupReport 79★, HA integration). 18.
- **Multi-WAN failover for plain Linux routers:** a clear gap, but weak
  counted demand. 18.
- **Docker macvlan host-access shim:** forum demand only, no counts. 20, but
  fails the rule of three sources with counts.

### Rejected in round 15

- **Busy-only CalDAV mirror:** nextcloud#11214 83 👍, but keeper.sh
  (1,413★) and ical-filter-proxy cover it.
- **Already covered:**
  - fail2ban JSON logs (CrowdSec);
  - lldap forward auth (tinyauth, Authelia);
  - certbot CNAME delegation (lego, acme.sh, acme-dns);
  - mDNS for Prometheus (prometheus-mdns-sd);
  - PBS monitoring (pbs-exporter);
  - ZFS replication monitoring (zrepl, ZFSGuard).
- **Low demand or too large:** container egress policy (Docker firewall
  churn), Docker with dynamic IPv6 prefixes, cloudflared DNS cleanup, Caddy
  metrics, an Authentik operator, Frigate retention by size.

## Research round 14 — 2026-10-22 IST

One research pass over about 30 projects' issue trackers (sorted by 👍), plus
repository, PR and web searches; the pick's numbers re-checked. "seen" means
in a GitHub search result on 2026-10-21/22; "web" means a web-search summary
only. Issue threads could not be read.

### Chosen: wgresolve, keep WireGuard peers with dynamic-DNS endpoints connected (project #13)

A small daemon (wgctrl): when a peer's handshake goes stale or the DNS TTL
runs out, it resolves the endpoint's name again and sets the new address.
Names come from wg-quick confs, systemd-networkd `.netdev` files or its own
config. It also keeps full-tunnel host routes right, resolves outside the
tunnel, prefers A or AAAA as configured, and has /metrics.

**Demand:**
- systemd#9911 "networkd WireGuard netdev: re-resolve DNS peer names every
  now and then (based on DNS TTL)": 51 👍, open since 2018-08, updated
  2025-12-16 (seen);
- a 2022 WireGuard mailing-list request "Configurable Re-Resolving of DNS
  Endpoints" (web);
- forum threads on Netgate, OpenWrt (stale host routes in full-tunnel
  setups), MikroTik, Keenetic, GL.iNet and Teltonika (web);
- six small home-made attempts, 0–1★, the newest from 2026-10-21 (seen).

**Upstream:** systemd PR #21562 "network: wireguard: re-resolve endpoints
periodically" is an open draft since 2021-11, last updated 2026-04-28 (seen).
If it merges, the networkd part loses its point. NetworkManager only waits
for resolution at activation (to verify).

**Gap:** wireguard-tools' contrib `reresolve-dns.sh` covers wg-quick confs
only. It needs bash and cron, and ignores TTLs, routes and DNS through the
tunnel. OpenWrt's watchdog restarts whole interfaces. No maintained daemon
was found.

**Scores:** 3/3/5/5/4 = **20**. Ties round 13's zfsunlock. Chosen because a
bug cannot lock anyone out of their data, and CI is straightforward: two
network namespaces with WireGuard and a DNS server whose record the test
changes.

**Check before the spec:**
- what NetworkManager does today;
- how Debian and Ubuntu ship `reresolve-dns.sh`;
- that the WireGuard module (or wireguard-go) works in the CI container.

**The other side:**
- the contrib script is good enough for simple setups;
- appliances (pfSense, MikroTik), a large part of the forum demand, cannot
  run it;
- the networkd PR could merge.

### Runners-up

- **DNS answers into nftables sets:** AdGuardHome#1191 64 👍, #5136 31 👍,
  unbound#43 15 👍; dnsmasq's `--nftset` and small dnstap tools cover part
  of it, and why #1191 is open despite AdGuard's ipset option is unclear.
  Scores 18.
- **CalDAV calendar published as a secret ICS feed, busy-only:**
  stalwart#1644 32 👍, #2176 15 👍; Radicale added secret links (2026-04).
  Scores 18.
- **Email-to-issue service desk for Gitea/Forgejo:** gitea#6219 50 👍, #6226
  35 👍; spam and privacy risks. Scores 17.
- **Rename a Nextcloud user:** nextcloud#5488 73 👍; high data-loss risk and
  heavy maintenance. Scores 16.

### Rejected in round 14

- AdGuard Home clients by MAC without its DHCP: upstream PR #8248 (NDP) is
  open.
- A shell in a service's systemd sandbox: one source (systemd#28163, 37 👍).
- Already covered:
  - VPN-container restarts (`depends_on: restart`);
  - macvlan DHCP (docker-net-dhcp, 239★);
  - host-to-host image transfer (unregistry, 4,880★);
  - Tailscale Wake-on-LAN (built in);
  - GitHub-to-Gitea mirroring (gitea-mirror, 1,495★);
  - DNS records from labels (dnsweaver);
  - PhotoPrism-to-Immich migration;
  - Portainer on Docker 29.
- Too large or poor fit: rclone Cryptomator, Stalwart webmail, Proxmox
  service discovery (needs Proxmox in CI), Vault-to-OpenBao (no demand).
- kumarest re-checked: PR #3854 is still a draft in the 3.1.0 milestone, so
  it stays set aside.

## Research round 13 — 2026-10-22 IST

One research pass (GitHub search plus web search), then the pick re-checked.
"checked" means seen in a GitHub search result or web result on 2026-10-22.
Issue threads could not be read (search only).

### Chosen: packparity, repair data for restic repositories (project #12)

Builds Reed-Solomon parity in a separate folder for a restic repository's pack
files (written once, named by their SHA-256, so damage is found exactly). It
repairs damaged packs from the parity, never writes into the repository except
to put back a repaired pack on request, and drops parity for packs that a prune
removed. It runs next to rest-server, or on a local or SFTP-mounted repository.

**Demand:**
- restic#256 "Add redundancy for data stored in the repository": 44 👍, open
  since 2015-08 (checked);
- restic#804 "Implement error correcting code (reed-solomon)?": 18 👍, open
  (checked);
- restic forum "How to create par2 parity files for a Restic repository"
  (thread 5149): users run hand-written par2create loops over data/ after each
  backup; and "Snapshot copy and corrupted data recovery" asks for erasure
  coding (checked, web);
- borg#225 "evaluate redundancy / error correction options": 43 👍, open since
  2015-09 (checked): the same need in the neighbouring tool.
- Kopia ships error correction (experimental) and rustic added a hook "to
  generate par2 files": both show the approach is wanted, neither helps restic.

**Upstream:** no parity PR in restic (checked); `restic repair packs` only
salvages the intact blobs and drops the rest. rustic#445 (par2) closed in two
days in 2023, answered with backend hooks.

**Gap:** baharsah/respar2 (0★, empty since 2022) and shell snippets; no tool
does incremental parity, prune cleanup, verification and repair for restic.

**Scores:** 3/4/5/5/4 = **21**.

**Risks and the other side:**
- The maintainers' answer is a second repository (3-2-1, `restic copy`);
  parity is for the single-copy or slow-to-recopy case, and the README must
  say so.
- Pointless on ZFS or btrfs with redundancy; no help if the whole disk is lost.
- v1 is restic and rustic only (borg segments differ).

### Runners-up

- **zfsunlock**, key slots and unattended unlock (passphrase, keyfile, Tang,
  TPM2) for ZFS native encryption: openzfs#6824 84 👍 (search, not re-checked),
  #12649 17 👍, #15186 10 👍. 4/4/4/3/5 = 20. Second because a bug can lock
  people out of their data, and ZFS on CI runners is unproven.
- **shelfsort**, Audiobookshelf library organizer: audiobookshelf#312 51 👍,
  #5054 13 👍, #5009 12 👍; no tool found. 18. Must check that listening
  progress survives a move.
- **bootproof**, Proxmox restore drills: forum demand and paid services, but
  CI needs nested Proxmox and 2026 attempts are appearing. 16.
- **sealkeeper**, TPM/Tang auto-unseal for OpenBao/Vault: vault#10622 82 👍,
  openbao#1200 22 👍; OpenBao's PKCS#11 seal plus tpm2-pkcs11 already covers
  it, painfully. 16.
- **Komodo Compose adoption**: komodo#377 40 👍, one source only. 16.

### Rejected in round 13

- Upstream PRs open: Ollama /metrics (PRs #18508, #16998), Ollama download
  rate limit (#15219), ntfy OIDC (#1971, #1579), Dokploy import/export
  (#5690), Beszel Proxmox stats (#539).
- Already covered: Mastodon backfill (FediFetcher), Outline local auth (OIDC
  providers), Navidrome LDAP (header auth), DMARC report viewers, self-hosted
  mail testers (happyDeliver).
- Too large: Synapse to tuwunel migration.
- Weak or low fit: Harbor regex retention, Backrest multi-repo, Nebula DNS.

## Research round 12 — 2026-10-21 IST

One research pass (GitHub search plus web search), then the top candidates
re-checked for upstream work. "checked" means seen in a GitHub search result on
2026-10-21. Issue threads could not be read (search only), so maintainer
positions are unknown unless a PR shows them.

### Chosen: pullwhy, "why is my docker pull slow or failing?" (project #11)

A CLI that walks a pull the way dockerd resolves it and shows each step:
- registry and mirror choice, from daemon.json (registry-mirrors apply to
  docker.io only), insecure registries and the daemon's proxy settings;
- the auth flow (token endpoint, scope, anonymous or with credentials from the
  CLI's config and helpers);
- manifest and platform resolution;
- redirects to blob storage or a CDN;
- Docker Hub's rate-limit headers (remaining pulls);
- time and speed per layer, and where it stalls or fails.

**Demand:**
- docker/cli#336 "docker pull should have a --verbose option to see all of the
  network calls": 244 👍, open since 2017-07, updated 2026-03-10 (checked);
- docker/cli#4161 "verbose log for docker pull": 33 👍, open (checked);
- Docker forums "Downloading from the registry is slow, what next?", and
  2025–2026 guides that time `curl /v2/` by hand to find auth, TLS or mirror
  delays (search summary);
- Docker Hub's pull limits make "is it the rate limit?" a common question.
  Hub's headers on 2026-10-21 said 100 an hour per IP without login (checked;
  a search summary's "10 an hour" was wrong).

**Upstream:** no verbose-pull PR in docker/cli; the open pull PR #7379 adds
`--dry-run`, which is something else (checked).

**Gap:**
- crane, regctl and skopeo log HTTP when asked, but do not follow dockerd's
  mirror and daemon config.
- `dockerd --debug` logs are coarse and need daemon access.
- No dedicated tool found.

**Scores:** 4/4/4/4/4 = **20**.

**Risks:**
- Fidelity: if pullwhy resolves differently from dockerd, it misleads. Test
  side by side with real `docker pull` against registry:2 with auth, a
  pull-through mirror, a TLS registry and a proxy.
- Both image stores (classic and containerd) must be covered.
- Docker could add `--verbose` itself.

### Candidates set aside after re-checking

- **kumarest** (a REST API for Uptime Kuma 2), 22 on paper. uptime-kuma#118
  "API functionality" has 706 👍 and has been open since 2021-07 (checked). But
  both of the owner's PRs are in the 3.1.0 milestone, as drafts with merge
  conflicts (checked):
  - #3854, "Document the Socket.io API and try to convert it to a HTTP
    request", which resolves #118;
  - #7551, "migrate to better auth api key".

  Upstream is building it. Re-check if 3.1.0 slips a long way.
- **Snapshot-consistent restic backups** (ZFS/btrfs/LVM snapshot, then restic
  on the real paths), 18 (was 21). restic#2092 has 141 👍 and is open (checked),
  but upstream PR #21771, "Allow rewriting snapshot paths with `--path-from` and
  `--path-to`", has been open since 2026-04 (checked). What remains is the
  snapshot orchestration (#3557, 48 👍), which borgmatic does for borg.
- **Syncthing hooks and conflict watcher**, 19:
  - syncthing#5601 hooks: 53 👍, open since 2019 (search summary);
  - #8114, conflict count in the UI: 47 👍;
  - no upstream hook PR found (checked);
  - the other requests are weak.
- **qbitmove** (qBittorrent settings and torrent migration), 17:
  - qBittorrent#6236: 80 👍; #13789: 55 👍 (search summary);
  - autobrr/qui (about 4.7k★) is adding the full preferences UI right now
    (PRs #3240 to #3255, merged 2026-10-17, checked) and has torrent exports in
    its automations. Too close.
- **appdb-move** (app-aware SQLite to Postgres/MariaDB), 19. The evidence is
  failure reports, not requests:
  - vaultwarden#7133, after pgloader;
  - Sonarr#8117.

  Each app needs a profile kept up to date.

### Rejected in round 12

- **Upstream building it:**
  - Gitea project-board automation (PR #30205);
  - Gitea repo-scoped tokens (merged #37306);
  - Headscale OIDC groups (PR #3216);
  - Kuma status-page heartbeat range (PR #7620).
- **Covered:**
  - qBittorrent free-space guard (qui, qBitrr);
  - NPM config as code (rejected in round 2; a Bash CLI and a Terraform
    provider exist);
  - targeted media scans (autopulse);
  - Kometa for Jellyfin (done).
- **Not doable or weak:**
  - several logins for one registry (credential helpers only get the host);
  - a Dockge successor (large, and Komodo exists);
  - Vaultwarden Secrets Manager (9 👍).

## Research round 11 — 2026-10-20 IST

Round 8's bar, across about 40 more repos (backups, proxies, identity, media,
DNS, mail, k8s, databases, observability, CI, secrets, networking). "checked"
means seen in a GitHub search result on 2026-10-19 or 20; the rest is from search
summaries. The pick's numbers and its key technical claim were checked again.

### Chosen: AdGuard Home real-client-IP bridge (project #10, working name xffdns)

A small relay in front of AdGuard Home. It takes plain DNS and DoT, with or
without the PROXY protocol header that HAProxy, Traefik or nginx stream send.
Each query goes on to AdGuard's DoH endpoint with `X-Forwarded-For` set to the
real client. AdGuard's query log, statistics and per-client rules then show the
real device instead of the proxy. This covers Android "Private DNS" (DoT)
behind a reverse proxy.

**Demand:**
- AdGuardHome#2798 "Support PROXY Protocol": 96 👍, open since 2021-03,
  updated 2026-09-17, on a 37.5k★ project (checked);
- #1806 (the same request, 17 👍, open) and #5694 (closed not_planned)
  (checked);
- #1727 "Use EDNS as client IP": 54 👍 (checked);
- a dnsdist mailing-list thread (2025-11) and a Caddy community thread on the
  same problem; an unofficial AdGuard fork adds PROXY v2 (search summary).

**Upstream:** no PROXY-protocol PR in AdGuardHome (checked).

**Gap:** 0xERR0R/dns-proxy (17★) relays DoT to DoH without the client IP; no
other tool found (checked).

**Verified 2026-10-20 with AdGuard Home v0.107.79:**
- with `http.doh.insecure_enabled: true` (not `allow_unencrypted_doh`, which
  this version drops), a DoH query over plain HTTP with `X-Forwarded-For:
  192.168.5.77` from 127.0.0.1 (in the default `trusted_proxies`) is logged as
  client 192.168.5.77, protocol doh; without the header it is logged as
  127.0.0.1;
- queries that fail upstream are not logged, so test with a local rule.

**Scores:** 4/5/5/4/4 = **22**.

**Risks:**
- Per-client rules and blocked services by XFF IP are not yet checked.
- AdGuard could add PROXY protocol itself.
- The audience is the part of AdGuard's users who run it behind a proxy.

### Runners-up

- **Audiobookshelf ↔ Kobo sync**, 18: audiobookshelf#3504 259 👍 (checked).
  The protocol is reverse-engineered, and there is no device to test with.
- **Frigate recordings archiver**, 18: frigate#3673 54 👍 (checked). Overlaps
  pushdir.
- **Prometheus query-limit proxy for Grafana**, 17: grafana#4288 200 👍 (checked);
  other sources are thin and the homelab fit is weak.
- **Grafana orgs/teams as code**, 15: grafana#12119 73 👍; Terraform and Ansible
  cover it.
- Re-checked:
  - grafana#10786 and tailscale#1013 are unchanged.
  - The Alertmanager silence companion is weaker: upstream PRs on muted alerts
    in 2025–2026.

### Rejected in round 11

- **Upstream building it:** Keycloak SCIM, SOPS decrypt-many, editing alert
  messages in place.
- **Covered:** Traefik certificate cleanup, flux build helmrelease, Argo CD
  offline rendering, ABS OPDS.
- **Below 50 👍:** Jellyfin endpoints, size-based retention (VictoriaMetrics,
  Loki), Headscale Funnel, an Authentik operator, Pushgateway TTL.
- **Too large or server-side:** Stalwart webmail, Vault KV symlinks.
- **No 50+ 👍 requests found:** Caddy, NetBird, Woodpecker, Mosquitto,
  Zigbee2MQTT, CoreDNS, Pi-hole, Navidrome, k3s, nginx, MySQL images.

## Research round 10 — 2026-10-19 IST

Round 8's bar, looking past Docker and Kuma this time. "checked" means seen in a
GitHub search result on 2026-10-18 or 19; the rest is from search summaries. The
pick's numbers were checked twice.

### Chosen: alertlines, log lines in Loki alerts (project #9)

A proxy between the Loki ruler and Alertmanager that speaks Alertmanager's v2
alerts API. For each alert it finds the rule's LogQL (from an annotation first,
the ruler's rules API second), fetches the matching log lines for the alert's
window, adds them as an annotation, and passes the alert on. Every Alertmanager
receiver and template keeps working.

**Demand:**
- loki#5844 "Add log message in alert": 107 👍, open since 2022-04, updated
  2026-04-22, on a 29k★ project (checked);
- duplicates #3507, #7823, #10645 (checked);
- at least 5 Grafana community threads (46053, 87113, 49785, 118871, 127073)
  asking for it (search summary).

**Upstream:** no matching PR in grafana/loki (checked). The ruler evaluates metric
queries only.

**Gap:**
- The documented workaround puts part of the line into a label with the pattern
  parser (a Grafana tutorial does this). It gives one field rather than the
  lines, and makes a series per message.
- Tools found: only 0–1★ projects, mostly LLM root-cause bots (checked). A
  repository search for Loki alert enrichment found nothing.

**Scores:** 4/4/4/4/4 = **20** (gap lowered from the researcher's 5 because of
the label workaround).

**Risks:**
- The alert does not carry its query, so it has to be found.
- Log lines can hold secrets or be long: they need a limit and redaction rules.
- Grafana-managed alerts take another path (out of scope for v1).

### Runners-up

- **grafana.com dashboards ready for provisioning** (fill `__inputs`, map data
  sources), 20:
  - grafana#10786, 213 👍, open since 2018 (checked);
  - no tool found.
  - Smaller in scope, and upstream PRs #129182/#129213 (2026-07) touched
    constant inputs.
- **mdnsgate** (mDNS across VPN sites), 19: tailscale#1013 295 👍 (checked);
  mdns-repeater 225★ only bridges interfaces that carry multicast. Device quirks
  and upstream risk.
- **Alertmanager silence companion** (resolved after silence, silence
  notifications), 19: alertmanager#226 112 👍, #730 42 👍 (checked); maintainers
  active on it.
- **kopia hub** (one UI for several repositories), 16: kopia#2976 79 👍;
  kopia-alternate-ui 49★ already does it.
- Re-checked, unchanged: docker cpx (moby#7710, 165), stackmove (docker/cli#1436,
  59). Swarm companion is weaker: docker/cli PR #4863 is still open, and PR
  #7300 is new.

### Rejected in round 10

- **Covered already:** ICS to CalDAV (keeper.sh, vdirsyncer); Grafana export all
  (grafana-backup-tool); Jellyfin/AdGuard exporters (crowded); AdGuard
  redundancy (adguardhome-sync); helm OCI versions (crane, oras).
- **Upstream building it:** Grafana shared variables and playlist variables.
- **Below the bar:** Nextcloud major-version stepping (18), mongo#329 (small).
- **No 50+ 👍 requests found:** borg, k3s, Longhorn, mailcow,
  docker-mailserver, Synapse, wg-easy, Authentik, litestream, Pi-hole,
  Vaultwarden, Home Assistant.

## Research round 9 — 2026-10-18 IST

Round 8's bar. One research pass (GitHub search plus web search); "checked" means
seen in a GitHub search result on 2026-10-18, the rest is from search summaries.
The pick's numbers were checked twice.

### Chosen: pushprobe, a remote probe agent for Uptime Kuma and Gatus (project #8)

A small Go agent run inside other networks or sites. It runs HTTP, TCP, ping, DNS,
TLS-expiry and host checks there and reports each result to an Uptime Kuma push
monitor (or a Gatus external endpoint); optionally it creates those monitors itself
(kumafile's client).

**Demand:**
- uptime-kuma#84 "Remote Executors": 411 👍, open since 2021-07, updated
  2026-08-14, on a 92k★ project (checked);
- gatus#64 "Distributed Deployment Configuration": 88 👍, open since 2020-12,
  updated 2026-09 (checked);
- uptime-kuma#819 (resource-usage heartbeats), 63 👍; duplicates #528, #390 and
  #2609 closed as not planned (checked);
- blog posts with home-made cron agents (search summary).

**Upstream:** no executor or agent PR in uptime-kuma (checked). Kuma 2.1's
Globalping probes are public, so they cannot reach private networks (search
summary; verify before the spec).

**Gap:** small attempts only (checked): tobias-carlbom/uptime-kuma-agent 20★
(Python), kuma-scout 16★ (Python), t0mer/uptimekuma-agent 6★, kuma-sentinel 9★,
uptime-koguma 1★. uptimy/agent (36★) and asura (18★) are Kuma alternatives,
not agents for it.

**Scores:** 5/4/5/4/5 = **23**.

**Risks:** push monitors carry only up/down, a message and a ping time; creating
monitors relies on Kuma's unofficial Socket.IO API (already handled in kumafile);
upstream could still build #84. Named pushprobe ("kumaprobe" is an archived 0★
repo; no exact "pushprobe" repo found).

### Runners-up

- **docker cpx** (a better `docker cp`: globs, several sources, `--chown`,
  container to container, distroless), 21: moby#7710 165 👍 open since 2014,
  #25977 37, #40925 22 (checked); no tool found. Small scope; may fit docker-mop.
- **stackmove** (move a Compose project to another host), 19: docker/cli#1436 59
  👍, moby#25245 60 (closed); docker-vackup 493★ does single volumes only.
- **Swarm companion** (modern Compose files for stack deploy, stack exec/logs,
  rebalance), 18: moby#31101 575 👍, #29133 164 and more (checked), but docker/cli
  PR #4863 (compose-go loading) is open and doco-cd (1.7k★) deploys Swarm stacks.
  Replaces round 8's Swarm rebalancer.
- **Gitea webhook reshaper**, 16: gitea#1089 53 👍; ntfy templates, n8n and
  Node-RED cover much of it.
- **Traefik maintenance toggle**, 16: traefik#41 68, #3520 53; about 8 plugins
  exist.
- Round 8's wildcard DNS sidecar and shared Traefik ACME: unchanged.

### Rejected in round 9

Upstream building it: Alertmanager Block Kit (PR #5592), restic .gitignore (PR
#4227), Authelia admin UI (PR #12654), Headscale WireGuard-only peers (PR #2892).
Covered: Traefik config UI (Mantrae 493★). One source: wildcard credHelpers. Too
large: Gitea proxy registries. Below 50 👍: Nextcloud S3 orphans, InfluxDB 2→3,
MongoDB stepwise upgrades, Compose start order after reboot, Alertmanager silences
in a file. No 👍 data: Syncthing, Immich (forums).

## Research round 8 — 2026-10-18 IST

A stricter bar this time. Each idea needs:
- a user request with 50+ 👍, open for over a year or declined, on a project with
  2k+ stars;
- two more sources;
- upstream not building it;
- no good tool;
- local tests possible.

Four ideas qualified.

### Chosen: docker-edit (project #7)

Change a container in place (ports, env, mounts, restart, labels, image, networks)
by recreating it exactly, with a plan, a safe swap and a rollback.

**Demand:**
- moby#21282 (port changes): 50 👍, open since 2016 (checked);
- moby#8838 (env on an existing container): 104 👍 (checked);
- the Stack Overflow question 19335444, copied by Baeldung, Better Stack and
  Edureka;
- Docker forum threads.

**Upstream:** by design (a container's configuration is fixed when it is created);
no PRs found.

**Gap:**
- runlike (2.9k★) and docker-autocompose (2.6k★) only print a command or a Compose
  file;
- Portainer can do it only in its GUI;
- the docker-recreate attempts have 0–8★.

**Scores:** 4/4/4/3/5 = **20**.

**Risks:** exact recreation is fiddly (image defaults, anonymous volumes, several
networks), and new API fields need tracking.

### Runners-up

- **Swarm rebalancer**, 21: moby#24103, 62 👍, open since 2016; the only attempts
  are 0★ scripts. Swarm is a shrinking niche, and the core action is close to a
  one-liner.
- **Wildcard DNS sidecar for Docker networks**, 20:
  - compose#7493, 85 👍, closed not_planned;
  - moby#43442, 74 👍, open;
  - mageddo/dns-proxy-server (853★) partly overlaps.
- **Shared Let's Encrypt for several Traefik instances**, 17: traefik#3749, 52 👍,
  open since 2018, labelled contributor/wanted (upstream would accept a pull
  request).

### Near-misses in round 8

- autoheal: moby#28400 has 324 👍, but docker-autoheal has 2k★.
- Bind mounts as another user: moby#2259 has 938 👍, but upstream PR #53368 is open.
- Swarm secret rotation: doco-cd does it.
- Prometheus per-series retention: done upstream in 2026-10.
- Traefik cache: souin.
- Container reading its own ports: one source only.
- Keycloak live export: upstream help wanted.
- Image last-used tracking: close to docker-mop.
- Zero-downtime Compose deploys: docker-rollout has 3.4k★.

## Research round 7 — 2026-10-17 IST

One research pass (GitHub search plus web search). "checked" means seen in GitHub
search; everything else is from search summaries.

### Dropped in run 7 (2026-10-18): Grafana alerting as code

Grafana is building it.
- grafana#120686 is open, internal and labelled area/gitsync (updated 2026-09-25).
- Groundwork merged in 2026: "Alerting: Map provenance to ManagerProperties for app
  platform compatibility" (#126737, 2026-07) and the rules.alerting.grafana.app
  k8s-style API (#128579, #130494, #133295).
- "Add ClassicAlerting resource parsing support to Git Sync" (#129688) was opened.
- With terraform-provider-grafana already there, the gap would likely close within
  a release or two.

Also checked: oncall-exit has no user requests on GitHub (0 issues); all the web
results are vendor marketing.

### Originally chosen in round 7: Grafana alerting as code (working name alertfile)

Export Grafana-managed alert rules, contact points, notification policies, mute
timings and templates to YAML, diff them against an instance, apply through the
provisioning API, and copy between instances. Same design as kumafile.

**Demand:**
- [grafana#120686](https://github.com/grafana/grafana/issues/120686) "Alerting:
  Support for GitSync": 166 👍, open since 2026-03, labelled `internal` (checked).
- [grafana#125463](https://github.com/grafana/grafana/issues/125463) "GitSync
  support for rules": 27 👍, 2026-05 (checked).
- [grafanactl#246](https://github.com/grafana/grafanactl/issues/246) "Add support
  for alerting resources": 10 👍 (checked).
- grafana#73437 added a UI export that only goes one way: 33 👍, closed (checked).
- Grizzly is archived (707★, checked).

**Gap:**
- terraform-provider-grafana (513★) means adopting Terraform;
- grafana-backup-tool (946★) only backs up and restores;
- grafanactl (172★) has no alerting;
- file provisioning is one-way and locks the rules in the UI.

**Scores:** 4/3/4/3/4 = **18**.

**Main risk:** #120686 is Grafana's own internal issue, so Grafana may build Git
Sync for alerting itself.

**Before the spec:**
- check the state of Git Sync for alerting and of grafanactl alerting support;
- use Grafana's file-provisioning format, so users are not locked in;
- lead with copying between instances.

If Grafana is close to shipping it, take a runner-up or keep researching.

### Runners-up

- **dnsmove** (convert Pi-hole v6 / AdGuard Home / Technitium), **20**.
  - AdGuardHome#1147, settings import/export: 62 👍, open since 2019 (checked).
  - The sync tools (nebula-sync 1.6k★, adguardhome-sync 1.8k★) only sync the same
    product.
  - Demand for converting between products is thin.
- **npm-exit**, 18. Re-checked and still weak: no high-👍 request, users move to the
  NPMplus fork, and caddyserver/nginx-adapter (358★) partly covers it.
- **oncall-exit** (Opsgenie / Grafana OnCall OSS → GoAlert), 16.
  - Real end-of-life deadlines: OnCall OSS was archived on 2026-03-24, and Opsgenie
    support ends on 2027-04-05.
  - The users are SRE teams rather than homelabbers.
- **coolify-eject**, 15. **Gitea Actions janitor**, 15 (Gitea is closing the gap).

### Rejected in round 7

- Plex→Jellyfin history: crowded.
- Gitea Pages: exists.
- Certificate expiry monitor: covered.
- GitHub→Gitea mirroring: strong demand (gitea#8424, 97 👍) but the mirror space was
  set aside; maybe reconsider.
- Forgejo Actions linter, HCP Terraform exit, Readarr exit, Umami GA import,
  Nextcloud→OpenCloud, Bitwarden attachments: weak or no signal.

## Research round 6 — 2026-10-16 IST

One research pass (GitHub search plus web search). "checked" means read from GitHub
search; "fetched" means the GitHub page was read; everything else is from search
summaries.

### Chosen (provisional): Uptime Kuma v2 config as code (project #6)

Export the monitors, notifications, status pages, maintenance windows and tags of
Uptime Kuma 2.x to YAML, diff them against a live instance, apply changes, and copy
config between instances.
- Export reads SQLite read-only.
- Apply goes through the Socket.IO API.

**Demand:**
- [uptime-kuma#1354](https://github.com/louislam/uptime-kuma/issues/1354)
  "Configure monitors using config file": 91 👍, open since 2022-03 (checked).
  The maintainer said in 2023 it is too hard in the current design (fetched).
- [uptime-kuma#6045](https://github.com/louislam/uptime-kuma/issues/6045) asks for
  import/export of settings: 28 👍, opened 2025-08 (checked). A collaborator wrote in
  2026-07 that "a json dump is a no-go… Nobody will maintain it" (fetched).
- [uptime-kuma-api#75](https://github.com/lucasheld/uptime-kuma-api/issues/75)
  "This project seems to be abandoned": 9 👍; #85 asks for 2.x support and is open
  (checked).
- Uptime Kuma has 92.5k★ (checked).

**Gap:**
- AutoKuma (907★) applies config from labels or files, cannot export, and breaks on
  unknown v2 monitor types (its #167).
- uptime-kuma-api (396★) is stalled.
- terraform-provider-uptimekuma has 70★.
- kumaconf is new and untested on v2.

**Scores:** 5/4/4/3/5 = **21**.

**Risks:**
- The Socket.IO API is unofficial and changes between versions. Pin and test against
  the Kuma versions supported.
- Upstream may add a SQL-dump backup, which would not be readable or diffable.

**Name:** "kumactl" is taken (Kong's Kuma service mesh CLI), so pick another.

**Before the spec:** re-check the counts and the maintainer quotes, and look for
newer tools.

### Runners-up

- **npm-exit** (Nginx Proxy Manager → Caddy/Traefik config), **21**.
  - NPM has 34.4k★; "config as code" (#2695) and CLI creation (#3708) were closed
    not_planned (checked).
  - The only converter found has 0★.
  - The demand evidence is thinner (5 👍 and forum threads).
- **Portainer exit** (stacks → plain compose folders), 19. nopoz/portrieve (11★)
  already does it.
- **GitHub star-lists backup/sync**, 19 (community#8293, 100 👍). The field is
  crowded.
- **n8n Community Edition git sync**, 17. There are many small DIY repos, and n8n may
  ship it.
- **Mastodon archive importer**, 15 (mastodon#12423, 478 👍). It depends on Mastodon
  internals, needs admin access, and federation could cause notification spam.

### Rejected in round 6

- Watchtower successor: dockcheck, Cup, WUD and Komodo exist.
- GitHub→Gitea mirror: gitea-mirror has 1.5k★.
- Editing ports or mounts of a container: runlike and docker-autocompose exist.
- Confluence export: exporters exist.
- ingress-nginx → Gateway API: ingress2gateway covers it.
- Bitnami image exit: overlaps pgcarry.
- Runner-fee estimator: the fee was postponed.
- Image changelog preview, dive on Docker 29, native HTTP healthchecks, registry
  retention, Plex→Jellyfin, *arr config as code, a Compose digest lockfile: weak
  signals, or already covered.

## Research round 5 — 2026-10-15 IST

One research pass. Counts marked (checked) were read from GitHub search that day;
web items come from search summaries (pages not opened).

### Chosen: Gitea/Forgejo issue mover (project #5)

Move (or copy) issues between repositories on a Gitea or Forgejo instance, or between
instances. It carries the title, body, comments, labels, milestones and attachments,
with original authors where admin access allows (Sudo) and original dates kept as
text. It then cross-links and closes the source issue. Dry run first.

- Demand:
  - go-gitea/gitea#453: open since 2016, 79 👍, 37 comments, updated 2026-04-09
    (checked);
  - gitea#39227: a new duplicate, 2026-09, 3 👍 (round 4);
  - gitea#2991: same-org moves, closed, 15 👍 (checked);
  - a Gitea forum thread on transferring issues (round 4);
  - a Fedora Forge investigation calling it impossible without the API (round 4).
- Gap: no tool on GitHub (checked), only ad-hoc scripts. Gitea's "reference in new
  issue" is a partial workaround.
- Scores 3/5/4/4/4 = **20**.
- Risks:
  - upstream may build it (open PRs were not checkable);
  - original timestamps cannot be set through the normal API, so check whether the
    migration code path or Sudo can help;
  - Forgejo's tracker on Codeberg was not checked.

### Runners-up

- **restic pull-mode orchestrator**, now 19 (was 22): restic#299 is open with
  333 👍. But Zerobyte (7,044★) backs up remote SFTP/NFS/SMB sources, and Backrest
  1.13 has a multihost hub. Pulling adds little security over push to an
  append-only rest-server (no credentials stored on the host, but the server holds
  root SSH keys to every host). If built, present it as "agentless central restic".
- **Docker published-port exposure auditor**, 19: moby#22054 is open with 90 👍;
  ufw-docker (6,823★) fixes rather than audits. Risk: a wrong "safe" verdict.
- **Per-container bandwidth limiter** (a docker-tc successor), 18: moby#26767
  (88 👍), moby#4763 (58 👍); docker-tc (757★) has open bugs and is stale.
- **Gitea ≥1.23 → Forgejo migrator**, 16. **Heroku exit bundler**, 15.

### Rejected in round 5

- A multi-registry pull-through cache (rpardini/docker-registry-proxy and zot cover
  it).
- A Valkey RDB downgrade (7 👍).
- Debian trixie SHA1 keys (the fix belongs to each repository's maintainer).
- The Dockhand licence change (free alternatives exist).
- Syncthing shared ignores (`#include` already does it).

## Research round 4 — 2026-10-14 IST

Two parallel searches: re-validation of earlier runners-up, and fresh signals.
Counts marked (checked) were read from GitHub search on 2026-10-14; forum and news
items came through web search summaries (pages not opened).

### Chosen: Compose-aware PostgreSQL major-version upgrader (project #4)

Upgrade the Postgres service of a Docker Compose project (or a plain container) to a
new major version safely: find it, read PG_VERSION / extensions / checksums / image
variant, take and verify a dump, upgrade into a NEW volume (pg_upgrade where both
versions' binaries are available, dump/restore otherwise), verify the result, switch
the service over, and keep the old volume untouched for rollback.

- Demand:
  - docker-library/postgres#37 "Upgrading between major versions?": 278 👍,
    140 comments, open since 2014-11, updated 2026-08-28 (checked).
  - tianon/docker-postgres-upgrade: 1,386★, describes itself as "a PoC … don't
    expect it to work as-is" (checked).
  - pgautoupgrade/docker-pgautoupgrade: 1,191★, 32 open issues (checked). Failure
    reports include #210, cross-device link with tablespaces on separate volumes
    (open, checked), and #157, 17→18 with checksums off (closed, checked).
  - PostgreSQL 14 end of life 2026-11-12. This comes from a web source and matches the
    5-year policy for 14.0 (2021-09); verify on postgresql.org before quoting.
  - 2026 upgrade guides for the Postgres 18 data-directory change: nite07, rezhajul,
    the Pinepods docs and the AFFiNE docs (web search).
- Gap: the existing tools are a proof of concept, an in-place image swap, or 0–2★
  scripts (bastidest/upgrade-postgres-docker-compose, 0★, 2024). None of them finds
  the Compose service, verifies a backup first, writes to a new volume, checks the
  result and offers rollback.
- Scores: demand 5, gap 4, feasibility 4, maintenance 3, fit 5 = **21**.
- Risks: this is data-loss territory. The old volume must never be written, every
  step must be checked, and there must be tests for each version pair and image
  variant (official, alpine, postgis, pgvector). Docker Hub pulls can hit rate
  limits (429).

### Runners-up

- **restic pull-mode orchestrator**: restic#299, 333 👍, open since 2015 (checked);
  backrest#944, 20 👍; restic forum threads. Existing: kaya (17★, last update
  2025-08), gdmn/bkp-restic (20★, shell). Scores 4/5/4/4/5 = 22 on paper, ranked
  second because an append-only rest-server already covers the main security worry
  of push backups. First candidate for round 5.
- **VS Code layered settings composer**: #15909 1,366 👍, #5595 1,150 👍, #40233
  1,040 👍, all open; PR #335281 (per-OS settings only) still open. 21. Needs a
  TypeScript extension shell; Microsoft may merge parts of it.
- **Gitea/Forgejo issue mover**: gitea#453 open, 79 👍; new duplicate #39227. 20.
- **Tailnet DNS server for custom records**: tailscale#1543, 926 👍. 19. High risk:
  Tailscale's alpha extraDNSRecords feature.
- **Docker published-port exposure auditor**: moby#22054, 90 👍. 19. Many firewall
  back ends; a false "safe" verdict would be dangerous.
- **Autoheal successor**: moby#28400, 324 👍. 19. Existing tools work.
- **Terraform exclude targeting**: still not shipped (#2253, 1,539 👍); OpenTofu has
  -exclude. 18.
- **Nextcloud storage auditor**: server#5539, 212 👍. 17.
- **Format-preserving yq editor**: 17. **Dead image-reference auditor**: 17.
- **Google CSE stand-in**: 15, already a drop-in (cse-bridge). **GoAlert
  migrator**: 15, no user-side demand found.

### Rejected in round 4

- Watchtower successor (an active fork has 4.6k★), File Browser successor (the
  Quantum fork), CDKTF→HCL (`cdktf synth --hcl`), OpenTofu linter (being built in),
  terraformer (Terraform `query`), Docker Content Trust (tiny usage), Cirrus CI
  (already shut down), whole-org mirroring for Gitea (scripts exist).

## Research round 3 — 2026-10-14 IST

### Chosen: pushdir (project #3)

A small daemon that watches local folders and pushes every change to any rclone
remote within seconds: debounced batches, renames and deletes handled, a periodic
full check to catch missed events, a retry queue, and a status/metrics endpoint.

Scores: demand 4, gap 5, feasibility 5, maintenance 4, fit 5 (total 23).

Evidence:
- rclone/rclone#249 "watch local folder and sync on change": 213 👍, 51 comments,
  open since 2015-12, labelled IMPORTANT, milestone "Help Wanted"; blocked upstream
  since 2021 on backend change notifications. https://github.com/rclone/rclone/issues/249
- rclone/rclone#4152 (change notifications for local filesystems, 10 👍) and #7474
  (bisync status and notifications, 7 👍).
- People build one-off watchers: dx8ug/rclone-watch (shell, 1 star),
  ninjaxtools/rclonewatch (0 stars, 2026-09), mstarczewski/RcloneFileWatcherCore
  (C#, 10 stars), mhavo/gdrive-sync (shell, 2026-09). None is established.

Why it is worth it: rclone has a very large user base, the request is a decade old
and labelled important, and the workarounds are cron jobs. Evergreen (not tied to
one shutdown). rclone builds here from the Go module proxy (v1.75.2), so it can be
tested end to end.

### Runners-up

- **Dead image-reference auditor** (Bitnami moved versioned tags to bitnamilegacy on
  2025-08-28; bitnami/charts#36309 "My website went down" 40 👍; ~14k compose files
  reference bitnami images, ~42k reference minio/minio, ~62k reference k8s.gcr.io
  per GitHub code search). Tools: kubernetes-sigs/community-images (k8s.gcr.io only),
  disdetta (1 star, created 2026-10-12). Scores 4/4/4/2/4 = 18: the rules list needs
  constant curation and the Bitnami peak was a year ago.
- **Self-hosted stand-in for Google's Custom Search JSON API** (closed to new users,
  existing users lose it 2027-01-01). Scores 4/4/3/3/3 = 17; ToS risks with backends.
- **Gitea/Forgejo issue mover** (go-gitea/gitea#453, 79 👍, open since 2016). 3/5/4/4/4 = 20.
- **Init-script → systemd unit converter after systemd 260** (demand expected, not
  observed). 3/4/4/4/3 = 18.
- **HCP Terraform bulk export** (free plan changes, 2026-03-31). 2/4/3/3/3 = 15.
- **VS Code layered settings**: PR #335281 still open (Copilot reviews only); a
  competitor merges shared/local settings. Dropped to 17.

## Research round 2 — 2026-10-13 IST

### Chosen: bucketmove (project #2)

A CLI for leaving MinIO safely. It lists what a MinIO deployment uses (buckets, object
versions, delete markers, object lock, lifecycle, policies, CORS, tags), probes what
the target (Garage, SeaweedFS, RustFS, Ceph RGW, AWS S3) really supports and reports
exactly what would be lost, then copies every bucket **with all object versions in
order**, and verifies the result.

Scores: demand 5, gap 5, feasibility 4, maintenance 3, fit 5 (total 22).

Evidence:
- minio/minio (61.3k stars) is archived; maintenance mode from 2025-12-03, archived
  2026-04-25 (https://pinggy.io/blog/minio_archived_self_hosted_s3_alternatives/).
  MinIO's images are gone from Docker Hub (checked: `minio/minio` pull denied).
- Migration questions on the targets: rustfs/rustfs#98 (17 comments), #759, #2283,
  #1489, #6295 (IAM migration bug, Aug 2026); seaweedfs#7586 (replacing MinIO).
- Migration guides do it by hand with rclone and lose versions, tags and policies:
  https://jacar.es/en/migrate-minio-to-garage-with-docker/ (also: Garage has no
  versioning, object lock, bucket policies or object tags, so users need to be told
  before they lose them). Tor's TPA-RFC-96 plans MinIO → Garage.
- Existing tools: rclone (no version history), `mc mirror` (current versions only),
  repos named minio-migrate with 0–1 stars. GitHub search for version-preserving S3
  migration tools found nothing established.

Testable here: MinIO builds from source via the Go module proxy (2.5 min); Garage,
SeaweedFS and RustFS images pull from Docker Hub.

### Runners-up

- **VS Code layered settings composer** (CLI + thin extension). Still open upstream
  (#15909 ~390+ 👍, last comment 2026-07; #5595, #40233). Community PR #335281 would
  add per-OS blocks natively. Alternatives tiny/dormant. Scores 5/4/4/3/4 = 20.
- **Dead image-reference auditor** (compose/k8s/Helm images from removed sources like
  docker.io/bitnami): bitnami/charts#35256, #35351, #36268. Scores 4/4/4/3/4 = 19.
- **Ansible-core 2.19 upgrade preflight** (broken conditionals/templating). Evidence
  thinner (ansible#85849 4 👍, netlab, VyOS). Scores 3/4/4/3/3 = 17.
- **Format-preserving yq/jq-style editor** (yq#515 156 👍, jq#105 381 👍). Scores
  4/3/4/3/4 = 18; small competitors exist.
- **Home Assistant add-on → Compose converter**. Scores 4/4/3/2/3 = 16.
- **Forgejo/Woodpecker Actions portability linter**: clear gap, weak demand data (16).

### Rejected in round 2

- Terraform visualizer: now crowded with small 2026 tools; big UI effort.
- termshark successor: too large; Wireshark team may adopt; ownership questions.
- Already solved: Alertmanager ack (kthxbye), Uptime Kuma config (AutoKuma), Komodo
  import, Portainer CE, NPM export, Node20 Actions, npm trust, ingress2gateway.

## Research round 1 — 2026-10-12 IST

### Chosen: docker-mop (project #1)

A Docker CLI plugin that shows exactly what a prune would delete, and how much space
it would free, before anything is removed. It also renames and copies volumes, which
Docker cannot do.

Scores: demand 5, gap 5, feasibility 5, maintenance 4, fit 5 (total 24).

Evidence:
- moby/moby#30623, "prune --dry-run": 391 👍, 110 comments, open since 2017-02-01.
  https://github.com/moby/moby/issues/30623
- moby/moby#31154, "volume rename / copy": 576 👍, open since 2017-02-18.
  https://github.com/moby/moby/issues/31154
- Docker Forums, "Is there a way to rename volumes?":
  https://forums.docker.com/t/is-there-a-way-to-rename-volumes/4570
- Workaround guides that copy data with a throwaway container, e.g.
  https://codemia.io/knowledge-hub/path/docker_volume_rename_or_copy_operation
- Podman ships `podman volume rename`, which shows the feature is wanted and
  workable; Docker still has none.

Existing tools: only tiny repos with 0–1 stars (kirankiran1234/docker-gc-safe,
gh640/docker-volumerename, kevinveenbirkenbach/docker-volume-renamer,
stellarshenson/docker-volume-toolkit). Nothing established.

Why it is worth it: two of the most-upvoted long-open Docker requests, used by a huge
audience, solved by one small Go binary. It can be tested end to end here against a
real Docker daemon.

Main risk: the preview must match Docker's real prune rules exactly (including `-a`,
`until` and `label` filters), otherwise people will trust a wrong list. Tests must
compare the preview with what a real prune actually deletes.

### Runners-up (kept for later rounds)

- **VS Code layered settings composer** (base + per-OS + gitignored local layers).
  Five microsoft/vscode issues with 741–1,366 👍 each (#15909, #5595, #40233, #2809,
  #40239). Best alternative has 35 stars. Scores 5/4/4/3/5 = 21. Risk: Microsoft may
  build it in.
- **Terraform-visualiser successor to rover** (im2nguyen/rover, 3.3k stars, "is this
  project dead?" #143, Dec 2025). Scores 3/4/4/3/4 = 18.
- **Terraform exclude/glob targeting wrapper** (hashicorp/terraform#2253, 1,539 👍;
  #2182, 657 👍). Scores 5/3/4/3/4 = 19. Risk: encourages risky partial applies;
  OpenTofu already has `-exclude`.
- **Gmail POP/Gmailify replacement** (Gmail dropped it in Jan 2026; Ask HN thread,
  The Register article, paid services appearing). Scores 4/3/3/2/3 = 15. Risk: Google
  OAuth restricted scopes and storing mail passwords.
- **Opsgenie / Grafana OnCall → GoAlert migrator** (OnCall OSS archived 2026-03-24;
  Opsgenie support ends 2027-04-05). Scores 3/4/3/3/3 = 16.
- **termshark successor** (10k stars; maintainer has died, issue #167; broken with
  Wireshark 4.6, #169). Scores 4/5/3/2/4 = 18. Wireshark team may adopt it; handle
  with care.
- **GitHub Actions → Forgejo/Woodpecker portability linter**. Scores 3/4/4/3/3 = 17.

### Rejected

- **pyproject.toml version bumper**: crowded field and uv may ship `uv upgrade`.
- **Zed: open VS Code workspaces**: moderate demand; Zed is likely to build it in.
- **Hyprland session restore**: hyprsession (102 stars) already covers it.
- **Discord → Matrix migration**: Discord terms and member-consent problems.
- **Confluence DC → wiki importer**: demand evidence is mostly vendor blogs.
- **Heroku exit CLI**: plausible; revisit with stronger user evidence.
- **Already solved**: Codex rewind tools, cargo-chef, thefuck (pay-respects),
  Todo Tree fork, ctop alternatives, kcat alternatives, garage-webui.
