# tsroam: spec

Use a Tailscale exit node automatically on Linux whenever the machine is not
on one of its own networks. Optionally, a kill switch makes sure nothing
leaves outside the tailnet while the exit node is meant to be on.

## Why

Tailscale has no automatic exit-node policy on Linux:
- a laptop on café or hotel Wi-Fi uses whatever exit-node setting was last
  left;
- if tailscaled crashes, restarts, updates or is brought down, traffic goes
  straight out on the untrusted network.

**Demand** (checked 2026-10-24):
- tailscale#3302 "FR: Automatically use an Exit Node on Untrusted/New Wifi
  Networks": 127 👍, open since 2021-11;
- #10773, a kill switch while tailscaled updates: 23 👍 (seen by research,
  not re-checked);
- #13992, an exit node on demand: 20 👍 (same);
- #7766 "Bypass exit nodes for certain domains": 50 👍. This is a later
  feature, not v1.

**Existing tools:**
- woopstar/AutoExitNode (1★) is Windows only.
- leolovenet/tailscale-routes (8★) is macOS only.
- kafu-densha/tsexit is a manual exit-node picker.
- No Linux tool, and no upstream PR, was found.

## Verified in the lab (2026-10-24)

- **Setup:** headscale v0.29.5 and tailscale 1.104.2 in network namespaces,
  with an exit node and a client on a LAN behind NAT, and a web host on the
  "internet".
- **`tailscale set --exit-node=IP`** returns in about 20 ms. Table 52 gets
  `default dev tailscale0` and the LAN's prefix; the LAN is reachable again
  only with `--exit-node-allow-lan-access`. `--exit-node=` turns it off.
- **Exit nodes forward only to public addresses.** A 10.x destination is
  rejected ("acl").
- **Leaks:** `kill -9 tailscaled` leaves Tailscale's ip rules but removes
  the tun's routes, so traffic goes out of the LAN interface directly.
  `tailscale down` does the same. A restarted tailscaled restores the exit
  route within about 1 s.
- **The kill switch:** an nftables table with an output-hook chain that
  accepts:
  - `lo` and `tailscale0`;
  - packets whose mark matches `0x80000/0xff0000` (tailscaled marks its own
    sockets);
  - DHCP (`udp dport 67`).

  It drops everything else. As built, after two reviews, the chain is on
  postrouting with policy drop: forwarded traffic may use only the tailnet,
  and tailscaled's DNS to resolvers off the tailnet is dropped. It also admits:
  - DHCP only from the client ports, on the network's interface (and
    DHCPv6);
  - IPv6 router and neighbour discovery to link-local and link multicast
    addresses;
  - tsroam's own probes (mark 0x01000000).

  See the README. With it, nothing left the machine with
  tailscaled killed or down, and a restarted tailscaled reconnected through
  it.

## What it does

`tsroam run` is a small daemon. `tsroam check` is one dry pass. `tsroam
learn PREFIX` prints a `trusted` line for the network the machine is on.
`tsroam off` removes the kill switch and stops forcing the exit node, until
the next network change. That is the way out of a captive portal.

```
exit-node 100.64.0.1          # or a node name, as tailscale set --exit-node takes
kill-switch on                # off by default
trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
trusted 10.10.0.0/16 probe 10.10.0.5:22
```

The network is the one carrying the main table's default route. Its
interface must not be Tailscale's. tsroam:

1. **On a trusted network** (its connected prefix and fingerprint match a
   `trusted` line, with tsdirect's fail-closed rules): no exit node, and no
   kill switch.
2. **On any other network:**
   - `tailscale set --exit-node=NODE --exit-node-allow-lan-access=false`;
   - with `kill-switch on`, the nftables table above. The café's LAN is
     not allowed: nothing but DHCP leaves outside the tailnet.
3. **No default route** (offline): nothing changes.

It acts:
- on netlink events (addresses, routes, links), settled, then two follow-up
  looks;
- every `--interval`;
- when tailscaled comes back (it checks the exit-node setting each pass and
  sets it again if something changed it).

Failing safe:
- An unknown network is untrusted.
- If tsroam crashes, its nftables table stays, so the kill switch holds.
  `tsroam off`, or `tsroam cleanup`, removes it.
- On a clean stop, tsroam removes the table only on a trusted network (as
  built: a restart at a café must not open a gap). It leaves the exit-node
  setting as it is.

tsroam controls Tailscale through the `tailscale` CLI (`--socket` for a
non-default tailscaled): `tailscale set` and `tailscale status --json` are
the public interface. Its nftables table is its own (`inet tsroam`), and it
never touches other tables.

Also:
- a log line for each change, with the reason;
- `/metrics` (`--listen`);
- `check` exit codes: 0 trusted or offline, 4 untrusted (the exit node is
  or would be on), 1 error.

## Non-goals (v1)

- Bypassing the exit node by domain or app (#7766): with MagicDNS the
  answers are not visible to tsroam. It is a later feature.
- Wi-Fi SSIDs as fingerprints (NetworkManager): later.
- IPv6 fingerprints. tsdirect has the same limit. The kill switch still
  drops IPv6 (the table is `inet`).
- Choosing among several exit nodes, or failover.
- macOS and Windows.

## Tech

- Go, one static binary. tsdirect's netlink, ARP and fingerprint code is
  copied in and adapted (the same author's code, in this repo).
- nftables through netlink (github.com/google/nftables, Apache-2.0), so the
  `nft` binary is not needed.
- Root, or CAP_NET_ADMIN and CAP_NET_RAW, plus access to tailscaled's
  socket. A systemd unit ordered after tailscaled.

## Tests

- **Unit:**
  - config;
  - the decision (trusted, untrusted, offline, IPv6 only, multipath, a
    trusted network on another interface);
  - the nftables ruleset, installed in network namespaces and tested with
    real traffic: local, forwarded, marks and DHCP ports;
  - `tailscale` CLI calls through a fake binary.
- **End to end** in the lab, with headscale, an exit node and a client on a
  home LAN; a café LAN behind another NAT; and a public-address web host
  that logs who connects:
  - **at home (trusted):** no exit node, and the web host sees the home
    NAT;
  - **at the café:** the exit node is set, and the web host sees the exit
    node's NAT (home);
  - **with the kill switch at the café:**
    - `kill -9 tailscaled` blocks the web host and the café LAN;
    - a restarted tailscaled restores traffic through the exit node;
    - `tailscale down` stays blocked;
  - **back home:** exit node off, kill switch gone;
  - **crash of tsroam:** the kill switch stays; `cleanup` removes it;
  - `tsroam off`.

## Milestones

1. Config, network identity (copied from tsdirect) and the decision, with
   unit tests.
2. The tailscale CLI driver and the nftables kill switch; `run`, `check`,
   `learn`, `off`, `cleanup`.
3. End-to-end tests in the lab; CI.
4. README with real runs, CHANGELOG, LICENSE, systemd unit; two full reviews.
