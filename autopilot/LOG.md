# Run log

Newest first.

## 2026-10-24 (IST) — run 13

- **Project #14 tsdirect: from milestone 1 to finished in this run.**
  - **Built:**
    - the netlink system layer: routes, a fresh ARP request over
      AF_PACKET, probes bound with SO_BINDTODEVICE, rules marked with
      protocol 0xd1, and events;
    - the reconcile agent;
    - `run`, `check`, `learn` and `cleanup`, with /metrics and SIGHUP
      reload;
    - a hardened systemd unit, checked to work as a dynamic user with only
      CAP_NET_ADMIN and CAP_NET_RAW;
    - the README, CHANGELOG, LICENSE, and CI.
  - **End-to-end harness:** headscale v0.29.5 and real tailscaled 1.104.2 in
    network namespaces, with a home LAN and a café LAN on the same
    addresses. It covers 8 scenarios, run with `--interval 10m` so that
    events drive every reaction. It is green locally and in CI; CI also runs
    the unit tests on big-endian MIPS under qemu.
  - **Lab lessons, recorded in STATE:**
    - Tailscale's relay (DERP) needs TLS;
    - the sandbox proxy variables must be stripped;
    - bridges take their lowest port's MAC.
    The SPEC's earlier claim that "the client could not reach its LAN at
    all" was a lab artefact, and has been corrected.
  - **Two full reviews**, 17 findings, all fixed. The most important:
    - kernels before 4.17 drop the rule's protocol value, so tsdirect would
      never find its rules to remove them; it now refuses to start on them;
    - netlink subscriptions could die silently;
    - a probe to the machine's own address succeeded through loopback;
    - the route choice differed from the kernel's (it now detects
      conflicts);
    - ARP byte order was wrong on big-endian CPUs;
    - a failed start left a crashed run's rule in place;
    - IPv6 was untested, so it is now refused in the config.
  - Mutation checks fail the tests as they should: an unbound probe, and no
    follow-up passes.
- **Research round 16:** about 30 trackers scanned. Provisional pick: a Linux
  exit-node policy for Tailscale (tailscale#3302, 127 👍; #7766, 50 👍; no
  Linux tool found). Runner-up: composeremote (compose#11867, 30 👍; partly
  covered by mutagen-compose).
- **Next:** the checks before the spec (listed in IDEAS round 16), then the
  spec and the build.
- **Owner:** create the tag `tsdirect/v1.0.0` on b56a34c, along with the
  earlier tags listed in STATE. The repo is still public.

## 2026-10-23 (IST) — run 12

- **READMEs:** roadmap sections were added to packparity, bucketmove,
  docker-mop, pgcarry and pushdir (the playbook's "done" list asks for one).
- **Project #13 wgresolve, from spec to v1.0.0 in this run.**
  - **Checked first:**
    - Ubuntu ships `reresolve-dns.sh` only as a doc example;
    - NetworkManager resolves names only at activation;
    - there is no kernel WireGuard here, but wireguard-go in network
      namespaces works.
  - **Built:**
    - config sources (wg-quick, networkd, NetworkManager, `--peer`);
    - a DNS resolver with TTLs;
    - the decision rules and watch loop;
    - `run`, `check` and /metrics;
    - a systemd unit, a Docker image and CI.
  - **End-to-end lab:** client, NAT router and server namespaces. Three
    earlier lab designs failed, and the reasons are recorded in STATE: the
    server replying from a deleted address, a second address, and routing
    rules that cannot drop local traffic.
  - **Two full reviews in parallel**, about 25 distinct findings, all fixed.
    The most important:
    - liveness now uses the receive counter, not handshake age, which wrongly
      flagged a healthy tunnel renewed every 165 s;
    - DNS queries carry the firewall mark, tested with wg-quick's full-tunnel
      rules; a mutation check showed the test fails without the mark;
    - the wireguard-go socket path in Docker;
    - IPv4-mapped AAAA answers looping;
    - a bad conf file hiding the others;
    - resolv.conf now re-read when it changes;
    - `check` exit codes.
  - CI found a race in the test harness, now fixed; IPv6 and kernel WireGuard
    pass in CI.
  - With the defaults, the tunnel came back 3 m 10 s after the last packet.
- **Research round 15:**
  - About 40 new trackers were scanned. **tsdirect** chosen (20):
    tailscale#1227 (88 👍), #7947 (38), #6231 (22) and #3646 (22), all
    checked; forum threads; no tool.
  - Upstream PR #21591 is static filtering only.
  - Next: the headscale and tailscaled lab, then the spec.

## 2026-10-22 (IST) — run 11

- **Project #11 pullwhy, finished.**
  - The second review's 18 findings are fixed:
    - unreadable credentials are now a note;
    - mirror failures are problems only when they stop the pull;
    - a working `--pull` turns probe failures into notes;
    - exit 5 means "could not check";
    - `--deep` uses per-endpoint credentials;
    - an unreadable certs.d is reported;
    - the expected HTTPS failure before a plain-HTTP retry is marked.
  - The side-by-side e2e suite passed locally on both image stores (678 s),
    and CI is green at bee622b. Tag commit: 90c49bd.
- **Research round 13:**
  - Six candidates scored. **packparity** chosen (21): restic#256 (44 👍),
    restic#804 (18 👍), borg#225 (43 👍), restic forum par2 threads; no
    tool exists (0★ attempts only), no upstream PR.
  - Runners-up: zfsunlock (20), shelfsort (18).
- **Project #12 packparity, from spec to v1.0.0 in this run.**
  - **Verified first:** restic 0.18.1 and rest-server 0.14.0 write files
    under temporary names and rename them; files are named by their SHA-256.
  - **Built:**
    - the parity format (interleaved block groups, per-block hashes);
    - sync, check, repair and status;
    - systemd units;
    - a README with a real run.
  - **Tests:** end to end with real restic and rest-server. Damage in packs,
    index, snapshots and config is found and repaired, then `restic check
    --read-data` passes.
  - **Two full reviews in parallel**, about 30 distinct findings, all
    addressed. The worst:
    - `repair config` with another repository's parity overwrote its config
      (now refused by every command);
    - damaged parity could not be fixed from the CLI;
    - a routine `forget --prune` tripped the mass-removal guard and stopped
      coverage;
    - crafted parity files crashed every command (a fuzz test now covers
      this);
    - an unmounted parity disk was silently started over (now `--init`).
- **Research round 14:**
  - Seven candidates scored. **wgresolve** chosen (20): systemd#9911
    (51 👍, seen), a WireGuard mailing-list request, forum threads on six
    platforms, six 0–1★ attempts.
  - Upstream systemd PR #21562 is still a draft (updated 2026-04).
  - Next: three checks, then the spec.
- The sandbox disk filled up once; it was cleared by removing scratch copies
  and stopped containers.

## 2026-10-21 (IST) — run 10

- Project #10 **xffdns**, from spec to v1.0.0 in one run.
  - **What:** real client addresses for AdGuard Home behind proxies
    (AdGuardHome#2798, 96 👍). It takes DNS over UDP, TCP and DoT, with or
    without a PROXY header, and passes each query to AdGuard Home's DoH with
    the client in X-Forwarded-For.
  - **Verified on real servers first:**
    - AdGuard Home's per-client settings, blocked services and access list
      follow the forwarded address;
    - a client ID in the path wins over the address;
    - AdGuard Home takes the first X-Forwarded-For entry;
    - its ratelimit does not apply to DoH;
    - the plain-DoH setting was renamed in v0.107.74.
  - **PROXY headers captured** from HAProxy 3.4.7, Traefik 3.7.15 and dnsdist
    2.0.11; they are now the parser's test fixtures.
  - **Built:**
    - DNS and PROXY v1/v2 parsers;
    - the DoH client;
    - UDP/TCP/DoT listeners with limits;
    - certificate reload;
    - metrics and health;
    - `run` and `check`; `check` reads AdGuard Home's query log and exits 3,
      with the fix, when AdGuard Home ignores the address;
    - a Docker image and a systemd unit.
  - **Tested end to end** in CI against AdGuard Home v0.107.45, v0.107.73 and
    latest, with HAProxy, Traefik, nginx and dnsdist in front, from client
    addresses 127.0.0.77 and up. Mutation checks confirm the tests catch an
    ignored PROXY source.
- Two full reviews: 15 and 13 findings, all addressed with tests. The worst:
  - a crafted query cost ~50 ms to parse before any rate limit;
  - slow readers and single clients could hold every in-flight slot;
  - there was no per-client connection cap;
  - UDP answers on wildcard addresses went out from the wrong source address
    (now IP_PKTINFO);
  - a parser regression from the first fix rejected names of four or more
    labels behind a compression pointer.
- One CI run failed on a test that raced the new start-up probe. It was fixed
  the same hour, along with a too-tight timing test. CI is green on e52932e
  (all three AdGuard Home versions, end to end).
- Research round 12: kumarest (Uptime Kuma#118, 706 👍) was set aside, because
  the maintainer's HTTP API PRs are in the 3.1.0 milestone. The restic
  snapshot wrapper fell to an upstream path-rewrite PR, and qbitmove to qui.
  Chosen: **pullwhy**, "why is my docker pull slow or failing?" (docker/cli#336,
  244 👍). Docker Hub's headers today say 100 anonymous pulls an hour per IP.
- Next: the pullwhy spec (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - create the v1.0.0 tags in the web UI:
    - `xffdns/v1.0.0` at e52932e;
    - `alertlines/v1.0.0` at 5e2f87c;
    - `pushprobe/v1.0.0` at 0e881d2;
    - `docker-edit/v1.0.0` at cdc7019;
  - optionally, give projects their own repos.

## 2026-10-20 (IST) — run 9

- Project #9 **alertlines**, from spec to v1.0.0 in one run.
  - **What:** log lines in Loki alerts (loki#5844, 107 👍): a proxy between
    Loki's ruler and Alertmanager.
  - **Found by testing a real Loki 3.7.8:** the ruler puts the rule's whole
    expression in each alert's generatorURL and sends X-Scope-OrgID. So
    alertlines needs no rule changes.
  - **Built:**
    - a quote-aware scanner that cuts the log query out of a metric
      expression (every cut query is checked against a real Loki);
    - the Loki client, limits, redaction and the cache;
    - `check`, and `run` (the proxy);
    - a Docker image and a systemd unit;
    - a README whose Slack message was rendered by Alertmanager's own amtool
      from a real alert.
  - **Tested end to end:** a real Loki ruler, then alertlines, then a real
    Alertmanager, then a webhook, on Loki 3.5.0 and 3.7.8; with Loki down the
    alert still arrives.
- Two full reviews found 25 problems, all addressed. The worst:
  - an open port let anyone read any tenant's logs (now: 127.0.0.1 by
    default, optional auth, a tenant allowlist, no pass-through);
  - un-grouped rules showed other streams' lines (now matched by stream
    labels);
  - offset and without were ignored;
  - redaction missed private keys, headers, cookies and escaped JSON, and
    over-redacted words like tokens_used.
- CI green on every alertlines push so far, including the Docker image build.
- Research round 11 chose the AdGuard Home real-client-IP bridge.
  - Demand: AdGuardHome#2798, 96 👍, open since 2021; no tool exists.
  - Tested on a real AdGuard Home v0.107.79: DoH with X-Forwarded-For from a
    trusted proxy is logged as the real client. So the design works.
- Next: the spec for it (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - create the v1.0.0 tags in the web UI:
    - `alertlines/v1.0.0` at 5e2f87c;
    - `pushprobe/v1.0.0` at 0e881d2;
    - `docker-edit/v1.0.0` at cdc7019;
  - optionally, give projects their own repos.

## 2026-10-19 (IST) — run 8

- Project #8 **pushprobe**, from spec to v1.0.0 in one run.
  - **What:** a probe agent that runs checks where Uptime Kuma or Gatus can't
    reach, and pushes the results to Kuma push monitors and Gatus external
    endpoints. Demand: uptime-kuma#84, 411 👍; gatus#64, 88 👍.
  - **Milestones:**
    - config, plus `check` and `validate`;
    - delivery and `run` (reload, /metrics);
    - `sync`, which sets up the Kuma monitors and saves tokens;
    - ping and exec checks;
    - a Docker image and a systemd unit.
  - **Verified with real Kuma 2.5.6 and Gatus:** up and down in both; Kuma
    marks a silent push monitor down by itself ("No heartbeat in the time
    window"); sync, then run with the saved tokens.
- Two full reviews found 19 distinct problems, all addressed. The worst:
  - two sites sharing a Kuma monitor through sync (sync now needs a group per
    site);
  - a DNS check answering from /etc/hosts instead of the named server;
  - a reload not re-reading saved tokens;
  - sync reporting a token saved when the write failed;
  - http headers sent on to another host after a redirect.
- Fixed in every project: Makefiles stamped all binaries with docker-mop's tag.
  They now use `<project>/vX.Y.Z` tags only.
- Kept pushprobe on Go 1.24: `go get` had silently raised go.mod to 1.26.
- CI is green on every pushprobe push, including building and running the
  Docker image.
- Research round 10 chose **alertlines**, log lines in Loki alerts:
  loki#5844, 107 👍, open since 2022; only 0–1★ attempts exist. Runner-up:
  grafana.com dashboards ready for provisioning (grafana#10786, 213 👍).
- Next: verify Loki's ruler and Alertmanager behaviour, then the alertlines
  spec (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - create the v1.0.0 tags in the web UI, named `<project>/v1.0.0` (for
    example `pushprobe/v1.0.0` at 0e881d2 and `docker-edit/v1.0.0` at
    cdc7019);
  - optionally, give projects their own repos.

## 2026-10-18 (IST) — run 7

- Dropped two ideas:
  - Grafana alerting as code: Grafana is building it (#126737, #128579, #130494,
    #133295).
  - oncall-exit: no user demand.
- Research round 8 (stricter bar) chose **docker-edit**: moby#21282, 50 👍;
  moby#8838, 104 👍.
- Project #7 docker-edit, from spec to v1.0.0 in one run.
  - **Recreation:** exact, from the container's own `docker inspect`. Image
    defaults are not frozen in; anonymous volumes are reattached; every network
    keeps its aliases and fixed IPs.
  - **Swap:** with rollback on a failed start, an unhealthy container or Ctrl-C.
  - **Edits:** every planned edit, with the plan shown first.
  - **Refusals:** files that would be lost, `--rm`, Compose, and shared networks
    or links.
  - **Bugs found on the way:**
    - a moved tag on the containerd image store;
    - a set MAC address lost;
    - a guessable fingerprint for hidden secrets;
    - legacy links breaking.
- Two full reviews found 20 problems; all are fixed, with tests. The worst:
  - an anonymous `--mount` volume that came back empty;
  - containers sharing another's network could never be edited;
  - Ctrl-C during the stop left the service down while saying it was back;
  - secrets in labels and URLs were printed;
  - `--context default` reached the wrong daemon;
  - a kept backup could restart by itself.
- Tests pass on both image stores locally and in CI (cdc7019). CI was red for
  four pushes on the classic store's output, then fixed. README with a real demo;
  CHANGELOG.
- Research round 9 chose a remote probe agent for Uptime Kuma and Gatus:
  uptime-kuma#84, 411 👍; gatus#64, 88 👍; the existing tools have 20★ at most.
- Verified the Kuma 2.5.6 push API and the Gatus external-endpoint API against
  real servers. Wrote projects/pushprobe/SPEC.md (project #8, named pushprobe).
- Next: pushprobe milestone 1 (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - v1.0.0 tags have to be made in the web UI (docker-edit: cdc7019);
  - optionally, give projects their own repos.

## 2026-10-17 (IST) — run 6

- Project #6 kumafile, from spec to v1.0.0 in one run.
  - Research re-checked: uptime-kuma#1354 has 91 👍 and #6045 has 28 👍. 2.x has
    no backup and no REST API for config, so the tool talks to Kuma over its own
    Socket.IO API.
  - Name: "kumactl" is taken (Kong).
  - Milestones:
    - a Socket.IO client;
    - login (password, TOTP, JWT);
    - export with a baseline, so files only list what differs from the web
      interface's defaults;
    - load and diff;
    - apply, with a check of the result.
- Two full reviews against real servers found about 25 problems, all fixed with
  tests. The worst:
  - an apply that hung forever on Kuma 2.0.0;
  - secrets missed for many notification providers, and printed by diff;
  - no timeouts;
  - a file could read any environment variable (now only KUMA_*);
  - prune too broad;
  - export overwriting files;
  - YAML that could not be read back;
  - Kafka SASL, custom links, logos, paused groups and null tags.
- The README shows a real demo run; CHANGELOG written.
- Tests pass locally and in CI (aa90445) on Kuma 2.0.0 and the latest 2.x.
- Research round 7 done: 6 candidates in IDEAS.md.
  - Provisional choice: Grafana alerting as code (grafana#120686, 166 👍).
  - Risk: Grafana may build it itself (that issue is internal); the next run
    checks before the spec.
- Next: research round 7 (STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - v1.0.0 tags have to be made in the web UI;
  - optionally, give projects their own repos.

## 2026-10-16 (IST) — run 5

- issuemove (project #5) built and finished:
  - milestones 1–3: plan, copy, resume, references, dependencies, cross-instance;
  - CI on Gitea 1.24 and the official Forgejo 16 image (the tests were confirmed to
    really run in the Forgejo job).
- Two full reviews: correctness/data loss, and security/usability (run against real
  Gitea containers). Fixed, each with a regression test:
  - fake markers planted in the target or the source could divert a move;
  - labels, milestone and assignees were silently dropped when the author could not
    write to the target;
  - a rerun could close copies, lost reactions and reverted attachment links;
  - authors who could not see a private target made the move fail halfway;
  - a shared token was sent to both instances;
  - a mistyped `--label` selected every issue;
  - cross-instance name matching could be forged (now opt-in with `--match-users`,
    and mentions are quietened);
  - plus redirects, retries, Ctrl-C at the prompt, the plan wording and exit codes.
- I checked by mutation that the new tests fail when each fix is removed.
- README from a real demo run (acme/website -> acme/docs), CHANGELOG, Roadmap.
- Research round 6 done: 6 candidates scored in IDEAS.md.
  - Chosen: config as code for Uptime Kuma v2 (uptime-kuma#1354, 91 👍; the
    maintainers have declined a readable export), provisionally; the evidence is to
    be re-checked before the spec.
  - Runner-up: Nginx Proxy Manager → Caddy/Traefik.
- Next: spec for project #6 (see STATE.md).
- Owner:
  - the finding-bugs repo is still public (your decision);
  - the v1.0.0 tags have to be created in the web UI (the git proxy refuses tags);
  - optionally, create own repos for the projects.

## 2026-10-15 (IST) — run 4

- Found that the finding-bugs repo is PUBLIC (the owner thought it was private).
  Recorded under owner blockers; visibility not changed (playbook rule).
- pgcarry milestones 1–4 done and pushed:
  - plan, the upgrade rehearsal, the switch with a by-position Compose edit,
    rollback, cleanup;
  - a version matrix 13→17, 14/15/16/17→18, Alpine, pgvector, ICU, a password
    file, trust auth.
- Found by testing:
  - 18 catalogs NOT NULL constraints;
  - pending ALTER SYSTEM settings live only in postgresql.auto.conf;
  - pg_isready makes no attempt without a user name under an arbitrary uid;
  - a service that only uses the postgres image for tools was taken for the
    database.
- Two independent reviews found 19 issues. All confirmed ones are fixed with tests.
  The serious ones:
  - writes from undeclared clients were lost (now the dump comes from the old server
    restarted without a network);
  - -f from another folder or different files copied the wrong data (now the project
    comes from the container's labels, and the mount is checked);
  - databases with datallowconn=false were silently skipped;
  - volumes with driver_opts were recreated without them;
  - PGPASSWORD was visible in ps and in errors;
  - a database named "dbname=x" redirected pg_dump;
  - SIGHUP or kill -9 left everything down (now the manifest comes first and
    rollback recovers);
  - list settings were mangled;
  - the wrong backup was picked by name.
- Measured: a 1.1 GB database rehearses in 51 s (dump 14 s, restore 21 s, verify
  2 s).
- A verification review confirmed all 11 fixes. It found 3 new high-severity
  issues in the new code, now fixed with tests:
  - starting the service by hand during the dump put two servers on one data folder
    (now detected and stopped, and the plan puts "run pgcarry rollback" first after
    a crash);
  - a rehearsal copy the user had switched to could be deleted (now it is replaced
    only if its cluster ID, last checkpoint and clean-shutdown state are
    unchanged);
  - recovery could remove a bind copy in use (now it checks every container's
    mounts and only acts after the old service serves).

  Also fixed: a lock for two commands on one upgrade, the Created flag being
  recorded late, env files, temp files and a hint's SQL.
- pgcarry v1.0.0 finished at df3dbb4; the full suite with -race is green locally.
- CI green on df3dbb4.
- Research round 5 done. Chosen project #5: a Gitea/Forgejo issue mover (gitea#453,
  open since 2016, no tool exists). The restic pull orchestrator dropped to 19/25:
  Zerobyte now backs up SFTP sources, and pull adds little over append-only.
- Next run: verify the Gitea/Forgejo API facts on real containers, write SPEC.md,
  start building.

## 2026-10-14 (IST) — run 3

- Owner has not created the docker-mop or bucketmove repos yet.
- Research round 3: chose pushdir (rclone watch mode; rclone#249, 213 👍, open since
  2015, labelled IMPORTANT; existing watchers have 0–10 stars). Runners-up recorded.
- pushdir milestones 1–2 and most of 3–4 built and tested with real rclone. Found
  and fixed while testing: rclone's --max-delete deletes up to the limit before
  stopping (so pushdir pre-counts), and a per-batch delete cap let slow mass
  deletions through (now a quiet period plus an hourly budget).
- pushdir finished (v1.0.0). Two independent reviews found real bugs, all fixed
  with tests: any rclone filter after `--` broke every batch (rclone refuses filters
  with --files-from-raw; batches are now chosen by an rclone listing with the user's
  rules in rclone's order, checked against `rclone copy` for 14 combinations);
  replacing the watched folder wiped the remote (now pushdir stops); a look-alike
  file could be "renamed" over (now hash-checked); renames skipped --backup-dir;
  atomic saves used up the delete budget; a blocked event queue could hang shutdown
  and lose new folders after an overflow; files with future timestamps spun the CPU;
  passwords in connection strings were logged and served on /status; overlapping or
  relative destinations were accepted; unknown remotes retried forever; a second
  Ctrl-C orphaned rclone. Docker image checked end to end; systemd unit untested.
- Research round 4 (two parallel searches; key counts re-checked on GitHub). Chosen
  project #4: pgcarry, a Compose-aware PostgreSQL major-version upgrader
  (docker-library/postgres#37, 278 👍, open since 2014; the 18 image's own error
  points there). SPEC.md written; Postgres image layouts verified on real images.
  Runner-up for round 5: restic pull-mode orchestrator (restic#299, 333 👍).

## 2026-10-13 (IST) — run 2

- Owner has not created the docker-mop repo yet; it stays in projects/docker-mop.
- Research round 2 (re-validated round-1 runners-up + new signals). Chosen project #2:
  bucketmove — leave MinIO (archived April 2026, images gone) without losing object
  versions, tags, locks or policies. Verified no version-preserving migration tool
  exists and that Garage silently lacks several features.
- Test servers working locally: MinIO built from source (4 drives), Garage,
  SeaweedFS, RustFS. Script: projects/bucketmove/scripts/test-servers.sh.
- Milestones 1–3 done: endpoints (URLs, env, mc aliases, MC_HOST_*), `inventory`,
  `check` (real feature probe + plain-words loss report, exit 3 on loss). Found on
  real servers: Garage has no versioning/tags/policy/object lock/ListObjectVersions
  and silently drops object tags on upload; MinIO has no per-bucket CORS.
- Milestones 4–5 done: `copy` (all versions oldest first, delete markers, headers,
  metadata, tags, retention, legal holds, lifecycle/policy/CORS/tags/default
  retention, journal with crash-safe resume, refuses on loss or existing buckets)
  and `verify` (version-by-version with SHA-256). Integration tests copy a seeded
  MinIO to Garage, SeaweedFS and RustFS and verify every version; resume and
  existing-bucket tests pass. Bugs found and fixed: objects written before
  versioning ("null" version) would have been copied as the newest version;
  SeaweedFS quirks (see STATE notes).
- Milestone 6 done: scale test (2,500 keys, 50-version key, 80 MiB multipart) on all
  targets; a Ctrl-C test through the CLI found a resume bug (an earlier version was
  adopted as a lost upload, silently dropping a version) — fixed, with a test that
  crashes at every point of a key's history. Two independent reviews: fixed secret
  leaks in URL errors, version order on RustFS, duplicate versions on retry, lock and
  default-retention handling, storage classes, journal repair, stricter verify,
  missing losses in check (existing bucket without lock, encryption/notifications/
  replication, unreadable settings), mc alias details, CLI interrupt handling.
  README with real output, LICENSE, CHANGELOG, `make dist`. Project #2 done.

## 2026-10-12 (IST) — run 1

- Research round 1 done (3 parallel searches); 13 candidates scored in IDEAS.md.
- Chosen: docker-mop, a Docker CLI plugin to preview prunes and rename/copy volumes
  (moby#30623 391 👍, moby#31154 576 👍, no established tool).
- Wrote SPEC.md. Milestone 1 done: dependency-free Engine API client (unix/TCP/TLS,
  DOCKER_HOST, contexts), plugin metadata, `docker mop version`, tests, CI, Makefile.
  Verified it installs and runs as a real `docker mop` plugin.
- Blocker: cannot create new repos (403); building in projects/docker-mop/ for now.
- Milestone 2 done: `docker mop plan` (containers, images, volumes, networks,
  system; until/label/label! filters; table and JSON). Integration tests compare the
  plan with Docker's real prune on the same state: 11 scoped cases + 8 unscoped +
  2 classic-builder cases, all identical on both the containerd and classic image
  stores. Bugs found and fixed on the way: hidden intermediate images, order-dependent
  parent handling, and containerd ignoring parent links.
- Found that Docker removes networks still used by stopped containers; the plan now
  warns about it.
- Milestone 3 done: `docker mop prune` shows the plan, asks (or --yes), removes
  exactly the planned objects without force, and skips anything that changed:
  a container started, a tag moved to a new image, a volume recreated or taken into
  use. docker-mop prune leaves the same state as Docker's prune in every case, on
  both image stores.
- Milestone 4 done: `docker mop volume copy|rename` with file-by-file verification
  (path, type, mode incl. setuid/sticky, owner, size, sha256, link targets, mtimes),
  label copy, --compose-project relabelling, and refusals for unsafe cases (in use,
  driver options, target exists). No image pulls: an empty local helper image and
  never-started containers. Tested with awkward data on both image stores.
- Milestone 5 done: two independent full reviews; fixed 6 issues in volume copy
  (race with a concurrently created target, hidden leftover helpers, stale Compose
  labels, xattr/dir-time checks, Ctrl-C leftovers, one fewer full read) and 9 in
  plan/prune (deleting an image that was tagged after the plan, config-only networks,
  until=0 and date parsing vs Docker, daemon clock/time zone, https without cert
  checks, docker global flags, API<1.42 volume rules, image edge cases). Added tests
  for each. README with real captured output, LICENSE, CHANGELOG, `make dist`.
  Full suite green on both image stores and in GitHub Actions. The v1.0.0 tag could
  not be pushed (git proxy refuses tags); noted as an owner step.
- Project #1 is done. Next run: research round 2.

## 2026-10-12 — setup

Playbook and state files created. Daily runs start after 12:30 AM IST.
Next: research for project #1.
