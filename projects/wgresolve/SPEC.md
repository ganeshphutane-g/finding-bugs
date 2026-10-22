# wgresolve: spec

Keep WireGuard peers whose endpoint is a DNS name connected when the address
behind the name changes (dynamic DNS, a moved server, a name that could not be
resolved at boot).

## Why

WireGuard resolves an endpoint's name once, when the peer is configured, and
then keeps that address. When a home server's dynamic IP changes, the tunnel
stays dead until someone restarts it.

**Demand:**
- systemd#9911 "networkd WireGuard netdev: re-resolve DNS peer names every now
  and then (based on DNS TTL)": 51 👍, open since 2018.
- A NetworkManager issue reports the same thing: a name is resolved only when
  the connection comes up. NetworkManager#739 adds the deadlock where the
  name can only be resolved through the broken tunnel.
- A 2022 WireGuard mailing-list request, "Configurable Re-Resolving of DNS
  Endpoints".
- Forum threads on OpenWrt, Netgate, MikroTik, Keenetic, GL.iNet and NixOS.
- At least six small home-made scripts and repos, 0–1★, the newest from
  2026-10.

**What exists:**
- wireguard-tools ships `reresolve-dns.sh` as an example (Ubuntu:
  /usr/share/doc/wireguard-tools/examples/, not installed as a service,
  checked 2026-10-22). Run from cron, it re-sets each wg-quick peer's
  endpoint when its handshake is older than 135 s.
  - It reads wg-quick confs only, one interface per call;
  - it has no networkd or NetworkManager support;
  - it ignores DNS TTLs and cannot resolve outside a tunnel whose DNS is
    down;
  - it has no logs or metrics, and needs bash and cron.
- systemd PR #21562 (re-resolve in networkd) has been an open draft since
  2021. NetworkManager merge request 721 only waits for resolution at
  activation. OpenWrt's watchdog restarts whole interfaces.

## Who it is for

Linux hosts (servers, home routers running a normal distro, laptops) whose
WireGuard peers are reached by name. It is for wg-quick, systemd-networkd,
NetworkManager, or tunnels set up by hand. It does not run on appliances
(pfSense, MikroTik).

## What it does

`wgresolve run` is a small daemon; `wgresolve check` is one dry pass.

1. **It finds peers with a name for their endpoint** by reading:
   - wg-quick confs (`/etc/wireguard/*.conf`; the interface is the file
     name);
   - systemd-networkd `.netdev` files (`/etc/systemd/network`,
     `/run/systemd/network`; `Kind=wireguard`, `[WireGuardPeer]`);
   - NetworkManager keyfiles (`/etc/NetworkManager/system-connections`,
     `[wireguard-peer.<key>] endpoint=`);
   - `--peer IFACE=PUBKEY=host:port` flags, for anything else.

   Peers are matched to live devices by interface name and public key,
   through wgctrl (kernel netlink, or wireguard-go's UAPI socket). Endpoints
   given as an IP address are left alone.
2. **It watches each peer.** Every `--interval` (default 30 s) it reads the
   peer's current endpoint and last handshake.
3. **It resolves the name**, with the TTL respected: an answer is reused until
   its TTL runs out (at least 30 s, at most 1 h). It queries the system's
   resolvers from `/etc/resolv.conf`, or `--resolver` servers. When the
   system's DNS is only reachable through the tunnel, `--resolver` can name
   servers outside it, so the deadlock cannot happen. `--prefer ipv4|ipv6`
   orders the answers (default: as the resolver gives them).
4. **It changes an endpoint only when the peer is not working:**
   - the peer has no endpoint (the name could not be resolved at boot): it is
     set as soon as the name resolves;
   - the handshake is older than `--stale` (default 135 s, the same as
     WireGuard's own retry window) and the endpoint is not one of the name's
     addresses: it is set to the first address;
   - the handshake is still stale after a change: on the next round it tries
     the name's next address.

   A peer with a fresh handshake is never touched, even if DNS says something
   else, because WireGuard may have roamed to the address the peer actually
   uses.
5. **It reports what it does:**
   - a log line for each change, saying why;
   - `/metrics` (`--listen`, off by default): per peer, the handshake age, the
     endpoint, changes, resolution errors and the TTL left;
   - `/healthz`.
6. **Config changes are picked up** on SIGHUP and every 5 minutes.

`wgresolve check` lists every peer found, where it came from, its name, the
answers, the current endpoint, the handshake age and what `run` would do. It
exits 0 when all is well and 3 when a peer needs a change (it would fix it)
or its name does not resolve.

## Non-goals (v1)

- Changing routes. wg-quick's full-tunnel mode uses policy routing (fwmark),
  so new endpoints need no host route. `check` warns when an endpoint's
  address would itself be routed into the tunnel.
- Appliances, Windows, macOS.
- Re-resolving for wg-quick's `DNS=` or anything other than peer endpoints.
- Being a DNS-update client: it follows the name; something else keeps the
  name up to date.

## Tech

- **Go, one static binary.**
  - golang.zx2c4.com/wireguard/wgctrl (MIT) talks to kernel WireGuard and
    wireguard-go;
  - github.com/miekg/dns (BSD) does DNS queries with TTLs (v1.1.68, the last
    release for Go 1.24).
- **Needs:** root or CAP_NET_ADMIN.
- **Packaging:** a systemd unit, and a Docker image (`network_mode: host`,
  `cap_add: NET_ADMIN`).

## Tests

- **Unit:**
  - the three config parsers, with real-world files (comments, odd
    spacing, IPv6 endpoints in brackets, several peers);
  - the decision rules with a fake clock and a fake device;
  - TTL caching against an in-process DNS server.
- **End to end:** network namespaces joined by veth, with real WireGuard.
  This sandbox has no kernel module, so the tests use wireguard-go (checked
  2026-10-22: a tunnel between two namespaces passes traffic; each device
  needs its own name, because the UAPI sockets share /var/run/wireguard).
  dnsmasq serves the server's name with a short TTL. The scenarios:
  - the server moves to a new address and its record changes: the client's
    handshake comes back within one TTL plus one interval;
  - a name that does not resolve at boot, then does;
  - a peer with a fixed IP endpoint is never touched;
  - a fresh handshake is not overridden by a different DNS answer;
  - the system resolver reachable only through the tunnel, with
    `--resolver` outside it;
  - IPv6 and `--prefer`;
  - wg-quick, networkd and NetworkManager files as sources.
- CI runs the same tests as root on GitHub's runners.

## Milestones

1. Config sources and the peer model, with parser tests.
2. Resolver (TTL cache) and decision engine, with unit tests.
3. `run` and `check`, wgctrl apply, metrics, reload; a systemd unit and a
   Docker image.
4. End-to-end tests in network namespaces; CI.
5. README with real runs, CHANGELOG, LICENSE; two full reviews.
