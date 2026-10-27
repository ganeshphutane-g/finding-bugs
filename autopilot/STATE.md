# State

Current project: #16, composehost (host-side steps for Compose projects
through Compose's provider services). Phase: build. Spec:
projects/composehost/SPEC.md (with what the lab verified).

## Next step

composehost: milestones 1–3 are done, and two full reviews were made with
their findings fixed (c27fabb, 29796a2). The big finding: Compose runs
providers in its caller's directory, so composehost finds the project
directory from the parent Compose process (/proc). A third, narrow review
of 29796a2 is under way. Left: fix what it finds, CI green, then mark
composehost finished (owner tag `composehost/v1.0.0`) and start project
#17: stseed, provisional, with the checks listed in IDEAS.md round 18.

Lab facts for composehost (2026-10-28): `-p NAME` without `-f` skips
providers on stop/down (with -f it works); with `env`, every up recreates
dependents; `docker-composehost` in ~/.docker/cli-plugins is found by
Compose; `docker compose --verbose` shows provider debug messages; Docker
Hub may rate-limit pulls here (busybox:latest is already pulled).

The lab here: start `dockerd` in the background (Docker 29.8.2, Compose
v5.6.0 at /usr/libexec/docker/cli-plugins). Compose's provider docs are
docs/extension.md in docker/compose (fetched with curl from
raw.githubusercontent.com).

Lab facts (2026-10-24):
- `GOBIN=/tmp/ts-bin GOTOOLCHAIN=auto go install
  tailscale.com/cmd/tailscaled@v1.104.2 tailscale.com/cmd/tailscale@v1.104.2`
  (fetches Go 1.27.3) and `github.com/juanfont/headscale/cmd/headscale@v0.29.5`.
  The disk is tight: run `go clean -cache` after building them.
- The harness is projects/tsdirect/internal/e2e/e2e_test.go: run with
  `PATH=/tmp/ts-bin:$PATH TSDIRECT_INTEGRATION=1 TSDIRECT_BIN=/tmp/tsdirect`
  (about 40 s). It writes its own headscale config.
- Tailscale's relay (DERP) needs TLS: the harness makes a self-signed cert
  for 10.50.0.2 and gives tailscaled SSL_CERT_FILE. Without DERP the nodes
  may never talk (that was the earlier "rx 0").
- Strip HTTP(S)_PROXY from lab processes: tailscaled uses the sandbox's
  proxy for DERP otherwise.
- A Linux bridge takes its lowest port's MAC: give lab "routers" fixed MACs,
  or two LANs end up with the same gateway MAC.
- Always `tailscale up ... --accept-dns=false`: otherwise tailscaled
  rewrites /etc/resolv.conf (namespaces share it). The harness also puts it
  back on cleanup.

wgresolve (#13) is finished (see Finished projects); CI is green at 27e1655.

Facts checked for wgresolve (2026-10-22):
- No kernel WireGuard in this sandbox (CONFIG_WIREGUARD unset), but TUN,
  netns and veth work. Use wireguard-go:
  `GOBIN=/tmp/wg-bin go install golang.zx2c4.com/wireguard@latest` gives
  /tmp/wg-bin/wireguard. Each device needs a unique name (UAPI sockets in
  /var/run/wireguard are shared across namespaces).
- `sudo apt-get update; sudo apt-get install -y iproute2 wireguard-tools
  dnsmasq-base iputils-ping` works here.
- Never `pkill -f` a pattern that appears in your own command line: it
  kills the shell.
- miekg/dns v1.1.68 is the last release for Go 1.24; wgctrl
  v0.0.0-20241231184526-a9ab2273dd10.
- Ubuntu ships reresolve-dns.sh only as a doc example, not as a service.
  NetworkManager resolves names only at activation (its issue #739 covers
  the DNS-through-tunnel deadlock).

Facts checked for packparity (2026-10-22), useful for any restic tool:
- restic 0.18.1 and rest-server 0.14.0 write every file under a temporary
  name (`<id>-tmp-…`) and rename; files are named by the SHA-256 of their
  contents (config is not); restic makes them read-only (0400).
- Install for tests: `GOBIN=/tmp/restic-bin go install
  github.com/restic/restic/cmd/restic@v0.18.1` and
  `.../rest-server/cmd/rest-server@v0.14.0` (the Go proxy works here).
- A routine `restic forget --prune` rewrites every index file and repacks
  small packs: guards must not count removals.

Disk: this sandbox's disk filled up once (scratch copies from reviews plus
Docker leftovers). `docker container prune -f; docker volume prune -f` and
clearing the scratchpad freed enough.

Local Hub access from dind: this sandbox intercepts TLS, so mount the CA
bundle: PULLWHY_DIND_CA=/tmp/claude-0/ca-bundle.crt (a 0644 copy of
/root/.ccr/ca-bundle.crt). Never publish its issuer name.

Test bed facts (2026-10-21):
- `docker run -d --privileged -e DOCKER_TLS_CERTDIR= docker:dind
  --registry-mirror=...` works here (29.9.2, containerd store). Run pullwhy
  inside it (`docker cp` the static binary) so it shares the daemon's network
  and socket. The host's own address is 192.0.2.2 here; registry:2 is cached.
- An unreachable mirror (10.255.255.1) costs 30 s per fetch on the containerd
  store (alpine:3 took 2 min 1 s) and one 15 s /v2/ timeout on the classic
  store (16 s). A refused mirror falls back at once.
- The containerd store reads /etc/docker/certs.d/<host:port>/ca.crt.
- With daemon.json proxies, dockerd CONNECTs auth.docker.io,
  registry-1.docker.io and production.cloudfront.docker.com (blobs).
- This sandbox has several egress IPs, so Hub's remaining count jumps; avoid
  spending Hub pulls (each docker:dind / image pull costs one).
- Restore /etc/docker/daemon.json (none by default) after experiments.

Tags: every Makefile takes its version from `<project>/vX.Y.Z` tags only, so the
owner's v1.0.0 tags should be named that way (docker-mop/v1.0.0 already is).

The Docker daemon here is 29.8.2 (API 1.56). Start it with `sudo nohup dockerd >
/tmp/dockerd.log 2>&1 &`.

Uptime Kuma test server, should a later project need one: `docker run -d -p
127.0.0.1:3901:3001 louislam/uptime-kuma:2`, then POST /setup-database
{"dbConfig":{"type":"sqlite"}}, wait about 20 s, and call setup(user, pass)
until needSetup is false (an early call is lost while the server restarts). A
probe script with the bundled socket.io-client works inside the container
(`docker exec X node script.js`).

Gitea test server, should a later project need one: `docker run -d --name gt1 -p
127.0.0.1:3001:3000 -e GITEA__security__INSTALL_LOCK=true -e
GITEA__database__DB_TYPE=sqlite3 gitea/gitea:1.24`, then `docker exec -u git gt1
gitea admin user create --admin ...`. Use `--noproxy '*'` with curl for 127.0.0.1.
Forgejo cannot be pulled here (codeberg.org is blocked); it runs fine in GitHub
Actions as codeberg.org/forgejo/forgejo:16.

## Notes for runs

- The Docker daemon is not running at session start. Start it detached with
  `sudo nohup dockerd > /tmp/dockerd.log 2>&1 &` and wait a few seconds
  (a plain `&` inside a compound command dies with the shell).
- Integration tests: `MOP_INTEGRATION=1 go test ./...` (label-scoped, safe) or
  `MOP_INTEGRATION=destructive` (unscoped; only on a throwaway daemon like this one).
- The classic image store prunes differently (parent images). To test it, stop
  dockerd and start one with `--feature containerd-snapshotter=false` and a separate
  `--data-root`; the classic-builder tests (DOCKER_BUILDKIT=0) only bite there.
- SeaweedFS quirks found: it lists versions out of key order (verify falls back to
  per-key lookups), guesses Content-Type for application/octet-stream objects in
  unversioned buckets (probe "exact content type"; reported as a change), stores
  "aws-chunked" as Content-Encoding for streaming-signed uploads (copier sends
  plain MD5-checked uploads instead), and needs -volume.max raised for many buckets.
- bucketmove servers: `scripts/test-servers.sh up /tmp/test-servers.env`, then
  `set -a; . /tmp/test-servers.env; set +a; BUCKETMOVE_INTEGRATION=1 go test ./...`.
  MinIO runs with 4 drives (versioning/object lock need erasure coding) and
  MINIO_CI_CD=1. Garage has no versioning, tags, policy, object lock and no
  ListObjectVersions; MinIO has no per-bucket CORS. SeaweedFS and RustFS support all.
- bucketmove test servers: MinIO is not on Docker Hub or reachable registries any
  more; build it with `GOBIN=/tmp/minio-bin go install
  github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c` (about 2.5 minutes).
  Garage (dxflrs/garage), SeaweedFS (chrislusf/seaweedfs) and RustFS (rustfs/rustfs)
  pull from Docker Hub.
- pushdir tests need rclone: `GOBIN=/tmp/rclone-bin go install
  github.com/rclone/rclone@v1.75.2` then `RCLONE_BIN=/tmp/rclone-bin/rclone
  make test`. Docker builds here cannot reach the Go proxy: to check the image, copy
  the project to a temp folder, run `go mod vendor`, and build with `-mod=vendor`.
- Docker Hub pulls can fail with 429 (rate limit). Tests must not depend on pulling
  images: build tiny test images locally (e.g. `FROM scratch` with a static file).

## Finished projects

1. **docker-mop** (projects/docker-mop, v1.0.0 at commit 50c1a1e). Docker CLI plugin:
   `plan`/`prune` (exact prune preview, verified against Docker's real prune on both
   image stores) and verified `volume copy`/`rename`. Before making it public the
   owner should: read the README, try `make install` and `docker mop plan` on their
   own machine, check that GitHub Actions ran green on the push, and decide whether
   to give it its own repo (recommended, so `go install` and releases work).
2. **bucketmove** (projects/bucketmove, v1.0.0). Leave MinIO safely: `inventory`,
   `check` (real feature probe + loss report), `copy` (all versions in order, delete
   markers, metadata, tags, locks, settings; crash-safe resume), `verify` (version by
   version with SHA-256). Tested against MinIO, Garage, SeaweedFS and RustFS; two full
   reviews fixed. Owner checks before going public: README, try it against their own
   MinIO with a test bucket, GitHub Actions green.

3. **pushdir** (projects/pushdir, v1.0.0 at commit 9d521f9). Watch a folder and push every change to
   any rclone remote within seconds (rclone#249). Batches with settle checks, filters
   applied by rclone's own rules, opt-in deletions with an hourly budget and
   unmount/empty/replaced-folder guards, hash-checked remote-side renames, retries,
   /status and /metrics, systemd unit, Docker image (checked end to end). Two full
   reviews (data loss; ops/security): 9 + 12 confirmed findings fixed, each with a
   test. Owner checks before going public: README, run it on a real folder for a day,
   try the systemd unit (not tested on a real systemd host), GitHub Actions green.

4. **pgcarry** (projects/pgcarry, v1.0.0 at commit df3dbb4). Safe major-version
   upgrades of Postgres in Docker Compose (docker-library/postgres#37).
   - **Commands:** plan; upgrade (dump from the old server restarted without a
     network, a matching new cluster in a new volume or folder, verified
     inventories, Compose edit by position, a cluster-identity check);
     `--no-switch` rehearsal; rollback (including recovery of a killed run);
     cleanup.
   - **Reviews:** two full reviews and one verification review; 19 + 7 findings
     fixed with tests.
   - **Tests:** Postgres 13–18, Debian/Alpine, pgvector, ICU, binds, user:,
     override files, and interrupts at every step.
   - **Owner checks before going public:** read the README; rehearse on a copy of a
     real app (`--no-switch`); CI green.

5. **issuemove** (projects/issuemove, v1.0.0 at commit e7c8051). Move issues between Gitea/Forgejo repositories, on one
   instance or across two (go-gitea/gitea#453, open since 2016, 79 👍).
   - **What it copies:** comments, labels, milestone, assignees, due date, state,
     attachments with relinked URLs, reactions, dependencies (both ways), and
     rewritten references.
   - **Authorship:** with an admin token, posts as the original authors (Sudo).
   - **Safety:** plan first; resumable without duplicates; markers trusted only
     from the token's user or the author.
   - **Reviews:** two full reviews (correctness; security/usability), with about 25
     findings fixed, each with a test.
   - **CI:** green on Gitea 1.24 and Forgejo 16.
   - **Owner checks before going public:** read the README; try `plan` and then a
     `move` of one test issue on their own instance.

6. **kumafile** (projects/kumafile, v1.0.0 at commit aa90445). Config as code for
   Uptime Kuma 2.x (uptime-kuma#1354, 91 👍; the JSON backup is gone in 2.0).
   - **Commands:** `export` (readable YAML, defaults left out, secrets in a 0600
     env file), `diff`, `apply` (dependency order, prune per section, moved
     monitors changed in place, the result checked) and `login` (token).
   - **How:** Kuma's own Socket.IO API, with a client written here, so it works
     remotely and with MariaDB.
   - **Reviews:** two full reviews (correctness; security/usability), with about 25
     findings fixed and regression tests.
   - **Tests:** a copy to an empty instance is exact; a second apply changes
     nothing; 2FA; 16 monitor types; tested on Kuma 2.0.0 and 2.5.6, locally and
     in CI.
   - **Owner checks before going public:** read the README; export your own
     instance and run `diff` (it should say in sync); try `apply` on a test
     instance first.

7. **docker-edit** (projects/docker-edit, v1.0.0 at commit cdc7019). Change an
   existing container's ports, env, mounts, restart policy, labels, image or
   networks (moby#21282, 50 👍; moby#8838, 104 👍).
   - **How:** the container is made again from its full `docker inspect`, with only
     the changes; a plan first; a swap with rollback (failure, unhealthy, Ctrl-C);
     image defaults are not frozen in; restart-only changes in place.
   - **Refuses** when something would be lost: changed files outside volumes,
     `--rm`, paused, Compose, `--network container:` and `--link` dependents, an
     image VOLUME left empty.
   - **Reviews:** two full reviews (correctness and data loss; security and
     usability), 20 findings, all fixed. Two only in part, the rest documented
     as limits: secrets in command lines are shown; --tlscacert/--tlscert/--tlskey
     are refused. Each fix has a regression test except the message for a
     failed rollback.
   - **Tests:** exact recreation of 9 container shapes, interrupts at every step,
     every edit; green on both image stores locally and in CI.
   - **Owner checks before going public:** read the README; try `--dry-run` and
     then an edit on a throwaway container on your own machine.

8. **pushprobe** (projects/pushprobe, v1.0.0 at commit 0e881d2). Runs checks
   where Uptime Kuma or Gatus can't reach (a LAN, another site, behind NAT) and
   pushes the results to Kuma push monitors and Gatus external endpoints
   (uptime-kuma#84, 411 👍; gatus#64, 88 👍).
   - **Checks:** http, tcp, ping, dns, tls, disk, memory, and opt-in exec.
   - **Commands:** `check` (dry run), `validate`, `run` (reload on SIGHUP,
     /healthz, /metrics), and `sync`, which creates the Kuma push monitors and
     saves their tokens.
   - **Packaging:** a Docker image and a systemd unit.
   - **Reviews:** two full reviews (correctness and delivery; security and
     usability) found 19 distinct problems; they overlapped on two. All are
     addressed: the promised TCP ping fallback was dropped from the spec, and
     the rest are fixed.
     - 14 fixes have regression tests.
     - These have none, since this Linux root sandbox can't reproduce them: a
       token that can't be saved, the note for an unreadable env file,
       refusing disk and memory checks on other systems, the Windows ping
       message, and the timeout for a hung network mount.
   - **Tests:** against real Uptime Kuma 2.5.6 and Gatus, locally and in CI.
     CI also builds and runs the Docker image.
   - **Owner checks before going public:**
     - read the README;
     - run `check`, then `sync --dry-run`, then `sync` against your own Kuma
       with a test group;
     - leave `run` going for a day.

9. **alertlines** (projects/alertlines, v1.0.0 at commit 5e2f87c). Puts the log
   lines that fired a Loki alert into the alert (loki#5844, 107 👍).
   - **How:** a proxy between Loki's ruler and Alertmanager. It reads the
     rule's expression from the alert's generatorURL, so rules need no
     change. It fetches the alert's own series' lines as the alert's tenant,
     redacts secrets, and adds `log_lines`/`log_lines_count` annotations. It
     never delays an alert beyond the timeout or drops one.
   - **Also:** `check` to try a rule; auth, a tenant allowlist and no
     pass-through; /metrics; a Docker image and a hardened systemd unit.
   - **Reviews:** two full reviews (correctness; security and usability) found
     25 problems; the two reviews overlapped on the missing tenant header. All
     are addressed. The worst was that anyone who could reach the port could
     read any tenant's logs through it; that is now closed by
     localhost-by-default, auth and no pass-through. Slack formatting of log
     text is documented, not changed. Regression tests cover most fixes; the
     permission-denied message and the `run --listen/--alertmanager` flags
     have none.
   - **Tests:** against real Loki 3.5.0 and 3.7.8 rulers and a real
     Alertmanager, locally and in CI. CI also builds the Docker image.
   - **Owner checks before going public:** read the README; run `alertlines
     check` on a few of your own rules; then point a test ruler at it.

10. **xffdns** (projects/xffdns, v1.0.0 at commit e52932e). Real client
    addresses for AdGuard Home behind proxies
    (AdGuardHome#2798, 96 👍).
    - **How:** takes DNS over UDP, TCP and DoT, with or without a PROXY v1/v2
      header (only from `proxy_from`), and passes each query to AdGuard Home's
      DoH endpoint with the client in X-Forwarded-For; client IDs from the DoT
      name. AdGuard Home's per-client settings, blocked services, access list
      and query log follow the forwarded address (verified).
    - **Also:** `check` (reads the query log to prove AdGuard Home trusts it,
      exit 3 with the fix if not), a start-up probe with hints, limits per
      client and in all, a UDP rate limit (AdGuard Home does not limit DoH),
      certificate reload, /metrics and /healthz, a Docker image and a hardened
      systemd unit (exposure 1.4).
    - **Tests:** end to end against AdGuard Home v0.107.45, v0.107.73 and
      latest, with HAProxy v1/v2, Traefik v2, nginx v1 and dnsdist v2 UDP/TCP
      in front, plus a spoofed header; CI green.
    - **Reviews:** two full reviews, 15 and 13 findings, all addressed with
      tests (parser cost, slot starvation, per-client limits, deadlines,
      redirects, UDP source address on wildcard listeners, docs).
    - **Owner checks before going public:** README; try it behind your own
      proxy with an Android phone's Private DNS; GitHub Actions green.

11. **pullwhy** (projects/pullwhy, v1.0.0 at commit 90c49bd). Shows why a docker
    pull is slow or fails (docker/cli#336, 244 👍).
    - **How:** reads the daemon's own settings (image store, mirrors,
      insecure registries, proxy, certs.d) and walks each endpoint the
      daemon will try: a timed ping, token and manifest HEAD, with Docker
      Hub's rate-limit headers and credentials found as the CLI finds them.
      `--deep` checks the platform and the download speed from the layer's
      real host; `--pull` has the daemon pull and times each layer.
    - **Verified on real daemons first** (both image stores): dead-mirror
      costs (30 s per request on containerd, one 15 s wait on classic),
      which certs.d each store reads for Hub, and how each store sends
      mirrors credentials (a login mirror stops only logged-in pulls on
      containerd).
    - **Reviews:** two full reviews, 16 and 18 findings, all addressed with
      tests (including exit 5 for "could not check", a working `--pull`
      overriding probe failures, and unreadable credentials as a note).
    - **Tests:** side by side with real `docker pull`s in throwaway daemons
      on both stores, locally and in CI.
    - **Owner checks before going public:** README; run it against your own
      daemon and a registry you know is slow or broken.

12. **packparity** (projects/packparity, v1.0.0 at commit 89cf92f). Reed-Solomon
    repair data for restic repositories (restic#256, 44 👍, open since 2015;
    restic#804; borg#225).
    - **How:** parity for every repository file in a separate directory,
      in interleaved 4 KiB block groups (10% by default: any 10 bad blocks
      per group, or a 410-block run in a 16 MiB pack). `sync` covers new
      files after checking them against their SHA-256 names and retires
      parity for pruned ones; `check` reads each file once a month, spread;
      `repair` rebuilds, proves the result by its name, keeps the damaged
      copy and puts the file back through the folder's handle; `status`.
    - **Guards:** not a repository, a parity directory not set up (`--init`),
      another repository's parity, an emptied data/, index/ or keys/, a
      changed config, crafted parity files (fuzzed), a waiting lock.
    - **Reviews:** two full reviews (correctness and data safety; security
      and operations), about 30 distinct findings, all fixed or documented
      as limits (memory, NFS locks, repair during a prune), with tests.
    - **Tests:** end to end with real restic 0.18.1 and rest-server 0.14.0,
      locally and in CI.
    - **Owner checks before going public:** README; run `sync --init` on a
      copy of a real repository, damage a pack copy, and repair it.

13. **wgresolve** (projects/wgresolve, v1.0.0 at commit 27e1655). Keeps
    WireGuard peers whose endpoint is a DNS name connected when the address
    behind the name changes (systemd#9911, 51 👍; NetworkManager and
    mailing-list requests).
    - **How:** reads peers from wg-quick confs, systemd-networkd `.netdev`
      files (drop-ins, precedence) and NetworkManager keyfiles. A peer
      counts as working while anything arrives from it (receive counter or
      handshake) within 190 s. A quiet peer is pointed at what its name
      resolves to now; a peer with no endpoint gets one when its name
      resolves; several addresses are tried once through. DNS queries
      respect TTLs and carry the interface's firewall mark (so a full
      tunnel cannot swallow them). Also `check` (exit codes for
      monitoring), /metrics, a hardened systemd unit and a Docker image.
    - **Reviews:** two full reviews (correctness; security and operations),
      about 25 distinct findings, all fixed. The worst: handshake age
      alone made a healthy tunnel renewed by the other side look quiet;
      DNS through a dead full tunnel; the wireguard-go socket path in
      Docker; IPv4-mapped answers looping.
    - **Tests:** end to end with real WireGuard (kernel in CI, wireguard-go
      here) in three namespaces with a NAT router whose public address
      changes. With the defaults, the tunnel came back 3 m 10 s after the
      last packet.
    - **Owner checks before going public:** README; run `wgresolve check`
      on a machine with a dynamic-DNS peer; then the service for a day.

14. **tsdirect** (projects/tsdirect, v1.0.0 at commit b56a34c). Keeps a
    Linux machine's LAN traffic on the LAN while Tailscale's accepted subnet
    routes cover that LAN, and only while it really is on it
    (tailscale#1227, 88 👍; #7947, #6231, #3646).
    - **How:** adds `ip rule to PREFIX lookup main priority 5200 proto 0xd1`
      while the kernel's main-table route for the prefix is a connected one
      on an up, non-Tailscale interface, no other route takes part of the
      prefix, and the fingerprint matches (gateway MAC by a fresh ARP
      request; and/or a TCP probe bound to the LAN interface, never to the
      machine's own address). Fails closed. Its rules are found by their
      protocol value: removed on exit, on failed starts, on stale
      priorities/LANs and by `cleanup`. Netlink events (plus two follow-up
      looks), `check` (exit 3 on a lookalike network), `learn`, /metrics,
      SIGHUP reload, a hardened systemd unit (dynamic user, CAP_NET_ADMIN +
      CAP_NET_RAW, verified working with only those). IPv4 only; Linux 4.17+.
    - **Reviews:** two full reviews, 17 findings, all fixed. The worst:
      kernels before 4.17 drop the rule's protocol value (rules never found,
      so never removed: now refused at start); netlink subscriptions dying
      silently; a probe to the machine's own address succeeding through
      loopback; route choice that differed from the kernel's; ARP byte order
      on big-endian CPUs.
    - **Tests:** unit tests (also on big-endian MIPS under qemu in CI); end
      to end with headscale v0.29.5 and real tailscaled 1.104.2 in network
      namespaces (home LAN, café with the same addresses, leaving, tailscaled
      restart, a second interface with part of the prefix, kill -9 and
      cleanup, a probe the tailnet could have answered), with
      `--interval 10m` so events drive every reaction. A mutation check
      showed the probe test fails without SO_BINDTODEVICE.
    - **Owner checks before going public:** README; on a laptop with
      `--accept-routes`, `sudo tsdirect learn <home prefix>`, `check` at home
      and on another network, then the service for a few days.

15. **tsroam** (projects/tsroam, v1.0.0 at commit 3905284). Uses a
    Tailscale exit node on Linux whenever the machine is not on a trusted
    network, with an optional nftables kill switch (tailscale#3302, 127 👍;
    #10773, 23 👍).
    - **How:** trusted networks are recognised with tsdirect's fail-closed
      fingerprints (fresh ARP for the gateway MAC; a bound probe that
      carries its own mark), on the default route's interface. On an
      untrusted network:
      - `tailscale set --exit-node`, enforced every pass;
      - the kill switch: `inet tsroam`, postrouting, policy drop. It
        admits only the tailnet, tailscaled's mark (but not its DNS off
        the tailnet), tsroam's probes, the machine's DHCP to
        broadcast/LAN, and IPv6 ND on the link. Forwarded traffic may use
        only the tailnet. Its rules carry integrity tags.
    - **When the switch goes in or stays:**
      - it goes in at start, on the default route's link changes or
        route moves, and while offline, before the network is looked at;
      - it is checked every 2 s;
      - it stays on a crash, and on a stop off trusted networks.
    - **Other features:** `off` (for a captive portal: a pause until the
      network, keyed by gateway MAC and prefix, changes, or it runs out),
      `check`, `learn`, `cleanup`, /metrics, SIGHUP, and a systemd unit
      before network-pre with only CAP_NET_ADMIN and CAP_NET_RAW
      (verified).
    - **Reviews:** two full reviews, 24 findings, all fixed. The worst were
      forwarded traffic, a leak window on network moves (including a café
      with home's gateway address), DHCP-port exfiltration, tailscaled's
      DNS through its mark, a probe blocked by the switch, and pause
      expiry.
    - **Tests:**
      - unit tests, with the kill switch tested with real traffic in
        network namespaces (local, forwarded, marks, DHCP ports, a
        hand-replaced rule);
      - big-endian MIPS under qemu;
      - end to end with headscale and tailscaled: 10 scenarios with
        `--interval 10m` (home, café, tailscaled killed and down, re-set,
        a flushed ruleset, off, home again with a probe under the switch,
        a crash, stops);
      - mutation checks for the mark rule, the probe mark and the
        postrouting hook.
    - **Owner checks before going public:**
      - the README;
      - on a laptop: `learn` at home, `check` at home and away, then the
        service with `kill-switch on` for a few days;
      - `tsroam off` behind a real captive portal.

## Blockers for the owner

- The finding-bugs repo itself is PUBLIC (seen 2026-10-15). Everything in it
  (projects, logs, research notes) is visible to anyone. The playbook rules forbid
  changing visibility, so the owner decides: make it private (GitHub > Settings >
  Danger Zone), or keep it public knowingly.

- Creating new repos fails: the GitHub app this automation uses gets "403 Resource not accessible
  by integration". To give docker-mop its own repo, create an empty private repo
  `ganeshphutane-g/docker-mop` on GitHub and give that GitHub app access to it
  (GitHub settings > Applications > that app > Repository access). The next run will
  move the project there with its history.
- Same for bucketmove, pushdir, pgcarry and issuemove (one empty private repo each,
  with that app given access), if they should live on their own.
- Same for pushdir: an empty private repo `ganeshphutane-g/pushdir` with that
  app given access lets the next run move it there (its README already uses that
  module path for `go install`).
- Pushing git tags is refused by this environment's git proxy (branch pushes work).
  To mark the release, create tag `docker-mop/v1.0.0` on commit 50c1a1e from the
  GitHub web UI (Releases > Draft a new release), or let the run tag it in the new
  repo once it exists.
- Release tags still to create (GitHub web UI), at the commits in Finished
  projects: `bucketmove/v1.0.0`, `pushdir/v1.0.0` (9d521f9), `pgcarry/v1.0.0`
  (df3dbb4), `issuemove/v1.0.0` (e7c8051), `kumafile/v1.0.0` (aa90445),
  `docker-edit/v1.0.0` (cdc7019), `pushprobe/v1.0.0` (0e881d2),
  `alertlines/v1.0.0` (5e2f87c), `xffdns/v1.0.0` (e52932e), `pullwhy/v1.0.0`
  (90c49bd), `packparity/v1.0.0` (89cf92f), `wgresolve/v1.0.0` (27e1655)
  `tsdirect/v1.0.0` (b56a34c) and `tsroam/v1.0.0` (3905284),
  plus `docker-mop/v1.0.0`
  (50c1a1e) if not yet done.
