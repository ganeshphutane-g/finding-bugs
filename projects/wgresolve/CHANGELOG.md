# Changelog

## 1.0.0 — 2026-10-23

First release.

- `wgresolve run` watches WireGuard peers whose endpoint is a DNS name and
  points a peer that has gone quiet (no handshake for `--stale`, 135 s) at
  what its name resolves to now; a peer with no endpoint gets one as soon as
  its name resolves. Working peers are never touched.
- Peers come from wg-quick confs, systemd-networkd `.netdev` files (with
  drop-ins and directory precedence), NetworkManager keyfiles and `--peer`.
- DNS lookups with TTLs (CNAMEs followed, TCP on truncation, search domains,
  several servers in turn, partial answers used); `--resolver` for a DNS
  server outside the tunnel; `--prefer` and `--family`.
- `wgresolve check`: one dry pass, with exit code 3 when something needs a
  change.
- `/metrics` and `/healthz`; one log line per change, and per state change
  of a peer.
- Kernel WireGuard and wireguard-go; a hardened systemd unit and a Docker
  image.
- Tested end to end with real WireGuard in network namespaces behind a NAT
  router whose public address changes.
