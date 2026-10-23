# tsdirect: spec

Keep a Linux machine's LAN traffic on the LAN when Tailscale's subnet routes
cover that LAN, and only while it really is on that LAN.

## Why

With `tailscale up --accept-routes`, Linux puts accepted subnet routes in
table 52, behind ip rules (5210–5270) that are looked up before the main
table. A laptop at home, on the 192.168.1.0/24 that the home subnet router
advertises, then sends its LAN traffic into the tailnet. It is slow at best.
In the lab it was broken: the client could not reach its own LAN at all, and
it fails whenever Tailscale is down.

**Demand** (checked 2026-10-23):
- tailscale#1227 "When local route is available to a subnet, bypass tailscale
  subnet relay": 88 👍, open since 2021;
- #7947 "accept routes at lower priority on Linux": 38 👍;
- #6231 "accepted subnet routes should not have a higher priority than main
  table": 22 👍;
- #3646 "exit-node-allow-lan-access for routed local subnets": 22 👍;
- forum threads on Tailscale, OpenWrt, NixOS and GL.iNet, and Tailscale's own
  docs page on overlapping subnet routes.

**Upstream:** PR #21591 (open, 2026-09-30) adds static allow and deny lists
for accepted routes, without detecting the LAN; draft #5770 has been stalled
since 2023. The workarounds are a static `ip rule` (wrong on roaming Wi-Fi)
or advertising a less specific prefix. No tool exists.

## Verified in the lab (2026-10-23)

- **Setup:** headscale v0.29.5 and tailscale 1.104.2, with kernel TUN, in
  network namespaces. There is a home LAN behind a NAT gateway, with a
  subnet router advertising 192.168.77.0/24, and a client on that LAN with
  `--accept-routes`.
- **The client's rules and routes:** 5210, 5230 and 5250 (fwmark
  0x80000/0xff0000), then 5270 `lookup 52`. Table 52 holds
  `192.168.77.0/24 dev tailscale0`. `ip route get 192.168.77.20` says
  `dev tailscale0 table 52`, and a ping to the LAN host failed.
- **The fix:** `ip rule add to 192.168.77.0/24 lookup main priority 5200`.
  The route becomes `dev lcl` (the LAN interface), pings work, and they
  keep working with the subnet router's Tailscale down. Without the rule,
  the LAN is unreachable again.

## What it does

`tsdirect run` is a small daemon; `tsdirect check` is one dry pass;
`tsdirect learn PREFIX` prints the current network's fingerprint for the
config.

For each configured LAN:

```
lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
lan 10.10.0.0/16 probe 10.10.0.5:22
```

it adds `ip rule to <prefix> lookup main priority 5200` while all of these
hold, and removes it as soon as one does not:

1. **The prefix is directly connected** in the main table, through an
   interface that is not Tailscale's (the machine is on a network of that
   address range).
2. **The network is the one configured:**
   - `gateway-mac`: the neighbour entry of the main table's gateway on that
     interface (or, for a LAN without a default route, of a given address)
     has that MAC. It is refreshed with an ARP or NDP probe.
   - `probe host:port`: a TCP connection made from that interface (bound to
     it, outside the tailnet) succeeds.
   - Both can be required. With neither, the LAN is refused. Fail closed: a
     café on the same 192.168.1.0/24 must not get the home LAN's traffic.

It acts:
- on netlink events (addresses, routes, links, neighbours), and every
  `--interval` (default 15 s);
- right after tailscaled starts or restarts (its rules come back, and
  tsdirect's stay).

Its rules carry their own `protocol` value (`--rule-proto`, default 0xd1), so
it removes exactly its own rules: on exit, at start (after a crash), and
with `tsdirect cleanup`.

Also:
- a log line for each rule added or removed, with the reason;
- `/metrics` (`--listen`, off by default);
- `check` exit codes: 0 fine, 3 a LAN is on but its fingerprint does not
  match, 1 error.

## Non-goals (v1)

- macOS and Windows (most of the demand, but the routing works differently
  there).
- Exit nodes ("allow LAN access" is already a Tailscale flag), and exit nodes
  on untrusted networks (a possible later feature, reusing the fingerprint).
- Changing Tailscale's settings. If PR #21591 lands, a later version could
  drive its deny list instead of ip rules.

## Tech

- Go, one static binary, `github.com/vishvananda/netlink` (Apache-2.0) for
  routes, rules, neighbours and events.
- Root or CAP_NET_ADMIN (ip rules; SO_BINDTODEVICE for probes).
- A systemd unit ordered after tailscaled.

## Tests

- **Unit:** config parsing; the decision for each combination of connected,
  fingerprint match and mismatch, and missing gateway; rule bookkeeping by
  protocol.
- **End to end** in the lab above, with headscale and tailscaled in CI too
  (tailscale needs Go 1.27; `GOTOOLCHAIN=auto` fetches it):
  - on the home LAN: the rule appears, LAN traffic goes direct, and still
    works with the subnet router down;
  - a "café" network with the same prefix and another gateway: no rule;
  - leaving the LAN (the link goes down, or the address changes): the rule
    goes;
  - restarting tailscaled: the rule stays;
  - killing tsdirect and starting it again: one rule, not two; `cleanup`
    removes it;
  - a probe-fingerprinted LAN.

## Milestones

1. Config, fingerprints and the decision rules, with unit tests.
2. netlink: rules with the protocol mark, connected-route checks, neighbour
   lookup, probes, events; `run`, `check`, `learn`, `cleanup`.
3. End-to-end tests in the headscale lab; CI.
4. README with real runs, CHANGELOG, LICENSE, systemd unit; two full reviews.
