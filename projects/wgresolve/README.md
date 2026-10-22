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
- it never touches a peer that is working.

```
$ wgresolve check
INTERFACE  PEER       NAME                    ENDPOINT          HANDSHAKE  STATE
wg0        Gdi16G4B…  home.example.net:51820  192.0.2.20:51820  8s ago     ok

wg0 Gdi16G4B… (/etc/wireguard/wg0.conf:5): working (handshake 8s ago)

# ... the server's connection gets a new address, and its DNS name follows ...

$ wgresolve run
2026-10-22T20:03:58Z wgresolve v1.0.0: watching 1 peer whose endpoint is a DNS name, every 30s
2026-10-22T20:06:28Z wg0 peer Gdi16G4B… (home.example.net): endpoint 192.0.2.20:51820 -> 198.51.100.7:51820: no handshake for 2m39s, and 192.0.2.20:51820 is not what the name resolves to now

$ wgresolve check
INTERFACE  PEER       NAME                    ENDPOINT            HANDSHAKE  STATE
wg0        Gdi16G4B…  home.example.net:51820  198.51.100.7:51820  23s ago    ok
```

(A real run, in network namespaces behind a NAT router. The path is
shortened, and the first line of `run` is shown as the release prints it.)

## How it decides

Every 30 seconds wgresolve reads each peer's endpoint and last handshake.

- **A working peer is left alone.** "Working" means a handshake in the last
  135 seconds, the window wireguard-tools' script uses too: WireGuard
  re-handshakes every 2 minutes while traffic flows. It is not touched even if
  DNS says something else, because WireGuard may have roamed to the address
  the peer really uses.
- **A peer with no endpoint** (its name did not resolve when the interface
  came up) gets one as soon as the name resolves.
- **A quiet peer whose endpoint is not what its name resolves to now** is
  pointed at the name's first address. If it is still quiet after another
  135 seconds, wgresolve tries the name's next address, if it has one.
- **A quiet peer at the name's current address** is down for some other
  reason; wgresolve says so and changes nothing.

So a dead tunnel comes back about 2½ minutes after its last handshake. That
is measured: in the end-to-end test the handshake came back 2 m 20 s after
the last good one, and in the run above 2 m 39 s after.

Lookups go straight to DNS servers, with TTLs: an answer is reused until its
TTL runs out (at least `--min-ttl`, default 30 s; at most an hour). A working
peer needs no lookup at all.

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

Or as a container, next to WireGuard on the host:

```sh
docker build -t wgresolve .
docker run -d --name wgresolve --network host --cap-add NET_ADMIN \
  -v /etc/wireguard:/etc/wireguard:ro -v /run/wireguard:/run/wireguard \
  wgresolve run --resolver 1.1.1.1
```

It needs root, or CAP_NET_ADMIN plus read access to the config files. It
works with kernel WireGuard and with wireguard-go (and other userspace
implementations that use the standard UAPI socket).

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
| `--interval` | `30s` | how often to look at the peers |
| `--stale` | `135s` | how old a handshake may be before the peer counts as not working |
| `--resolver` | servers in `/etc/resolv.conf` | DNS servers to ask (repeat) |
| `--prefer` | `ipv4` | try `ipv4` or `ipv6` addresses first |
| `--family` | both | only `ipv4` or only `ipv6` addresses |
| `--min-ttl` | `30s` | keep an answer at least this long |
| `--dns-timeout` | `3s` | per DNS server |
| `--listen` | off | serve `/metrics` and `/healthz` (run only) |
| `--no-files` | | read no config files; only `--peer` |

**Full tunnels and DNS.** If your system's DNS is only reachable through the
tunnel (wg-quick's `DNS =` with `AllowedIPs = 0.0.0.0/0`), a dead tunnel
also means no DNS. That is the deadlock NetworkManager users report. Give
`--resolver` a server outside the tunnel (your router, or a public one) so
wgresolve can still find the new address.

## check

`wgresolve check` does one pass and changes nothing. It lists every peer it
found, where it was configured, its endpoint, its handshake, what the name
resolves to and what `run` would do. It exits 3 when a peer needs a change
or its name does not resolve, so it can be used in a monitoring script.

## Metrics

With `--listen 127.0.0.1:9586`:
- `/metrics` reports, per peer, `wgresolve_peer_up`,
  `wgresolve_peer_handshake_age_seconds`,
  `wgresolve_endpoint_changes_total` and `wgresolve_resolve_errors_total`,
  plus `wgresolve_peers` and `wgresolve_last_round_timestamp_seconds`;
- `/healthz` answers 200 while rounds keep running.

## Limits

- **Linux only.** Not appliances (pfSense, MikroTik) or Windows.
- **No routes are changed.** wg-quick's full-tunnel mode uses policy routing,
  so a new endpoint needs no host route. A hand-made full tunnel with a
  static route to the old endpoint needs that route updated too.
- **`/etc/hosts` is not read.** Endpoint names are looked up in DNS, which is
  the dynamic-DNS case this is for.
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
- one full run with the default 135 s.

## License

MIT
