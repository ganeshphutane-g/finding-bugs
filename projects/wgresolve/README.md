# wgresolve

Keeps WireGuard peers whose endpoint is a DNS name connected when the address
behind the name changes.

WireGuard resolves an endpoint's name once, when the peer is set up, and keeps
that address for good. When a home server's dynamic IP changes, the tunnel
stays dead until someone restarts it. Users have asked for a fix since 2018
([systemd#9911](https://github.com/systemd/systemd/issues/9911); NetworkManager
and the WireGuard mailing list have the same request, and forum threads on
OpenWrt, pfSense, MikroTik and NixOS).

wireguard-tools ships an example cron script, `reresolve-dns.sh`, for wg-quick
confs. wgresolve is the maintained version of that idea:
- it works with wg-quick, systemd-networkd and NetworkManager alike;
- it respects DNS TTLs;
- it can resolve outside a tunnel whose own DNS is down;
- it logs every change and why, and has metrics;
- it never touches a peer that is working, judged by what arrives from it.

```
$ wgresolve check
INTERFACE  PEER       NAME                    ENDPOINT          HANDSHAKE  STATE
wg0        vCBZUzZi…  home.example.net:51820  192.0.2.20:51820  0s ago     ok

wg0 vCBZUzZi… (/etc/wireguard/wg0.conf:5): working (last heard from 0s ago)

# ... the server's connection gets a new address, and its DNS name follows ...

$ wgresolve run
2026-10-22T20:33:41Z wgresolve v1.0.0: watching 1 peer whose endpoint is a DNS name, every 30s
2026-10-22T20:37:12Z wg0 peer vCBZUzZi… (home.example.net): endpoint 192.0.2.20:51820 -> 198.51.100.7:51820: nothing received from it for 3m31s, and 192.0.2.20:51820 is not what the name resolves to now

$ wgresolve check
INTERFACE  PEER       NAME                    ENDPOINT            HANDSHAKE  STATE
wg0        vCBZUzZi…  home.example.net:51820  198.51.100.7:51820  28s ago    ok
```

(A real run, in network namespaces behind a NAT router; the path is
shortened.)

## How it decides

Every 30 seconds wgresolve reads each peer's endpoint, last handshake and
byte counters.

- **A working peer is left alone.** "Working" means something arrived from
  it within the last 190 seconds: its receive counter grew, or it
  handshook. 190 s is past WireGuard's 3-minute session lifetime, after
  which no traffic can flow without a new handshake. A working peer is not
  touched even if DNS says something else, because WireGuard may have
  roamed to the address the peer really uses.

  Handshake age alone would not do. A session the other side started is
  renewed only every 165 s when this side just sends keepalives, and an idle
  tunnel has no handshakes at all.
- **A peer with no endpoint** (its name did not resolve when the interface
  came up) gets one as soon as the name resolves.
- **A quiet peer whose endpoint is not what its name resolves to now** is
  pointed at the name's first address.
- **A quiet peer at one of the name's addresses** that keeps sending without
  an answer is tried at the name's other addresses, once through the list.
  Otherwise it is down for some other reason; wgresolve says so and changes
  nothing.
- Just before a change it looks at the peer once more, and leaves it if
  anything arrived meanwhile.

So a dead tunnel comes back a little over 3 minutes after the last packet
from it: 3 m 10 s in the end-to-end test with the defaults.

Lookups go straight to DNS servers, with TTLs: an answer is reused until its
TTL runs out (at least `--min-ttl`, default 30 s; at most an hour). A working
peer needs no lookup at all. Failed lookups are retried after 10 s, then
20 s, and so on, up to 5 minutes.

## Install

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/wgresolve
make build                      # ./wgresolve
sudo install wgresolve /usr/local/bin/
sudo cp contrib/wgresolve.service /etc/systemd/system/
sudo systemctl enable --now wgresolve
```

Flags for the service go in `/etc/default/wgresolve`, as
`WGRESOLVE_OPTS="--resolver 192.168.1.1"`.

Or as a container, next to WireGuard on the host:

```sh
docker build -t wgresolve .
docker run -d --name wgresolve --network host --cap-add NET_ADMIN \
  -v /etc/wireguard:/etc/wireguard:ro \
  -v /run/wireguard:/var/run/wireguard \
  wgresolve run --resolver 192.168.1.1
```

The second mount is for wireguard-go (its control sockets); kernel
WireGuard needs only `--network host` and NET_ADMIN. For systemd-networkd
or NetworkManager, mount `/etc/systemd/network` or
`/etc/NetworkManager/system-connections` read-only as well.

wgresolve needs root. Kernel WireGuard needs CAP_NET_ADMIN, the config files
are root's (0600), and wireguard-go's control sockets are root-only. It works
with kernel WireGuard and with wireguard-go (and other userspace
implementations that use the standard UAPI socket in `/var/run/wireguard`).

## Where peers come from

| Source | Files | Interface |
| --- | --- | --- |
| wg-quick | `/etc/wireguard/*.conf` (`--wg-quick-dir`) | the file's name |
| systemd-networkd | `.netdev` files with `Kind=wireguard`, in networkd's directories, with drop-ins (`--networkd-dir`) | `Name=` |
| NetworkManager | keyfiles in `/etc/NetworkManager/system-connections` (`--nm-dir`) | `interface-name=` |
| flags | `--peer IFACE=PUBLICKEY=host:port` (repeat) | as given |

Peers whose endpoint is an IP address are ignored. Peers are matched to the
live interface by public key. The configuration is read again every 5
minutes and on SIGHUP (`systemctl reload wgresolve`), so new peers are picked
up.

## Flags

| Flag | Default | |
| --- | --- | --- |
| `--peer` | | `IFACE=PUBLICKEY=host:port`, a peer to watch (repeat) |
| `--wg-quick-dir` | `/etc/wireguard` | wg-quick confs (`""` to skip) |
| `--networkd-dir` | networkd's own list | `.netdev` directories (repeat) |
| `--nm-dir` | NetworkManager's own | keyfile directories (repeat) |
| `--resolv-conf` | `/etc/resolv.conf` | where the system's DNS servers are listed; read again when it changes |
| `--interval` | `30s` | how often to look at the peers |
| `--stale` | `190s` | how long nothing may arrive from a peer before it counts as not working (below 3 minutes, a working peer that sends nothing may count as quiet) |
| `--resolver` | servers in `/etc/resolv.conf` | DNS servers to ask (repeat) |
| `--prefer` | `ipv4` | try `ipv4` or `ipv6` addresses first |
| `--family` | both | only `ipv4` or only `ipv6` addresses |
| `--min-ttl` | `30s` | keep an answer at least this long |
| `--dns-timeout` | `3s` | per query (A and AAAA are asked at once, then the next server) |
| `--listen` | off | serve `/metrics` and `/healthz` (run only) |
| `--no-files` | | read no config files; only `--peer` |

**Full tunnels and DNS.** When everything goes through the tunnel
(`AllowedIPs = 0.0.0.0/0`), a dead tunnel can take DNS with it. That is
the deadlock NetworkManager users report. wgresolve handles both halves:
- its DNS queries carry the interface's firewall mark, which wg-quick and
  NetworkManager set for full tunnels. So they leave by the normal route,
  as WireGuard's own packets do, not into the dead tunnel.
- if the system's DNS server itself is only reachable through the tunnel
  (wg-quick's `DNS =`), give `--resolver` a server outside it: your router,
  or a public one. A local stub such as systemd-resolved's 127.0.0.53
  forwards queries itself, without the mark, so name a real server.

## check

`wgresolve check` does one pass and changes nothing. It lists every peer it
found, where it was configured, its endpoint, its handshake, what the name
resolves to and what `run` would do.

**Exit codes**, for monitoring scripts:
- **0:** all well. An interface that is not up, such as a wg-quick profile
  not in use, is only a note.
- **3:** a peer would be changed, is down, is missing from its interface, or
  its name does not resolve.
- **1:** wgresolve could not read an interface or the config (run it as
  root).

## Metrics

With `--listen 127.0.0.1:9586`:
- `/metrics` reports, per peer:
  - `wgresolve_peer_up`;
  - `wgresolve_peer_handshake_age_seconds`;
  - `wgresolve_peer_endpoint_info` (the endpoint as a label);
  - `wgresolve_answer_ttl_seconds`;
  - `wgresolve_endpoint_changes_total`;
  - `wgresolve_resolve_errors_total`;

  plus `wgresolve_peers` and `wgresolve_last_round_timestamp_seconds`.
- `/healthz` answers 200 while the loop keeps making progress.

The metrics name each peer's public key and host. A server's public key lets
anyone probe whether it answers, so keep `--listen` on loopback or a trusted
network.

## Security

WireGuard authenticates both ends, so a forged DNS answer cannot read or
inject tunnel traffic. But someone who can answer wgresolve's DNS queries
while a peer is quiet could point the peer at themselves. They would then
see its handshake attempts, or relay the traffic and see its metadata. Use
a DNS server you trust (your own resolver, over a network you trust).
wgresolve logs every change it makes.

Answers that cannot be an endpoint (0.0.0.0, loopback, multicast, link-local:
a DNS sinkhole or a captive portal) are ignored, and said so.

## Limits

- **Linux only.** Not appliances (pfSense, MikroTik) or Windows.
- **IPv6-only hosts:** pass `--family ipv6` (or `--prefer ipv6`), or the
  first change goes to an IPv4 address and the IPv6 one is tried a
  `--stale` later.
- **No routes are changed.** wg-quick's full-tunnel mode uses policy routing,
  so a new endpoint needs no host route. A hand-made full tunnel with a
  static route to the old endpoint needs that route updated too.
- **`/etc/hosts` is not read**, while wg-quick, networkd and NetworkManager
  do read it when they set a peer up. Names are looked up in DNS, which is
  the dynamic-DNS case this is for. A peer whose name you pin in
  `/etc/hosts` should get that address in its config instead.
- **Something else keeps the name up to date.** wgresolve follows a dynamic
  DNS name; it does not update one.

## Roadmap

- Host routes for hand-made full tunnels.
- A watch on the config files instead of a 5-minute reload.
- `/etc/hosts` and mDNS names.
- Release binaries and a published image.

## Development

```sh
make test              # unit tests
make test-wireguard    # also end to end with real WireGuard (root)
```

The end-to-end tests build three network namespaces: a client, a NAT router
with a public address, and a server behind it. They use real WireGuard
(kernel, or wireguard-go when the kernel has no module), dnsmasq for the
name, and nftables for the NAT. The tests cover:
- the server's public address changing;
- a name that resolves only after the interface is up;
- a working peer not being overridden by a changed DNS answer;
- a peer with a fixed address never being touched;
- IPv6 with `--prefer ipv6`, where the kernel has IPv6;
- `check`;
- one full run with the default `--stale`;
- DNS through a full tunnel (wg-quick's policy routing), which fails without the firewall mark.

## License

MIT
