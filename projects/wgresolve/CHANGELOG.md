# Changelog

## 1.0.0 — 2026-10-23

First release.

- `wgresolve run` watches WireGuard peers whose endpoint is a DNS name and
  points a peer that has gone quiet (nothing received from it for
  `--stale`, 190 s, judged by its receive counter and handshakes) at what
  its name resolves to now; a peer with no endpoint gets one as soon as its
  name resolves. Working peers are never touched; the peer is read again
  just before a change. Several addresses are tried once through.
- Peers come from wg-quick confs, systemd-networkd `.netdev` files (with
  drop-ins and directory precedence), NetworkManager keyfiles and `--peer`.
- DNS lookups with TTLs (CNAMEs followed, TCP on truncation, search domains
  as glibc applies them, A and AAAA at once, several servers in turn,
  partial answers used briefly, back-off after failures, unusable answers
  ignored). Queries carry the interface's firewall mark, so a full tunnel
  does not swallow them; `--resolver` for a DNS server outside the tunnel;
  resolv.conf is read again when it changes; `--prefer` and `--family`.
- `wgresolve check`: one dry pass, with exit code 3 when something needs a
  change.
- `/metrics` and `/healthz`; one log line per change, and per state change
  of a peer (DNS errors once per kind).
- Kernel WireGuard and wireguard-go; a hardened systemd unit and a Docker
  image.
- Tested end to end with real WireGuard in network namespaces behind a NAT
  router whose public address changes.
