# Ideas

Researched candidates, with evidence. Newest research first.
Scores are 1–5 (higher is better): demand, gap, feasibility, maintenance (5 = light), audience fit.

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
