# Changelog

## 1.0.0 — 2026-10-25

First release.

- `tsroam run`: on any network that is not one of the configured trusted
  ones, the configured Tailscale exit node is on (`tailscale set
  --exit-node`), enforced every pass. On a trusted network it is turned off
  on arrival. Offline, nothing changes.
- Trusted networks are recognised by their addresses and a fingerprint (the
  gateway's MAC by a fresh ARP request, a TCP probe bound to the interface,
  or both) on the interface the default route leaves on. They fail closed.
- `kill-switch on`: an nftables table (`inet tsroam`, the only table tsroam
  touches; postrouting, policy drop) lets only tailnet traffic, tailscaled's
  own packets (but not its DNS to resolvers off the tailnet), tsroam's
  probes, loopback, the machine's DHCP client and IPv6 router/neighbour
  discovery on the link leave (and a private LAN with `allow-lan on`).
  Routed containers and VMs may use only the tailnet. It holds while
  tailscaled is killed, restarting or down. It goes in at start, on link
  changes and while offline, before the network is looked at. It stays if
  tsroam crashes or stops on an untrusted network, and it is checked every
  2 s and repaired if flushed or edited.
- `tsroam off` stands down for a captive portal until the network changes or
  `--for` runs out. `tsroam check` (exit 4 on an untrusted network),
  `tsroam learn`, `tsroam cleanup`.
- Reacts to netlink events (and to Tailscale's exit route changing) within
  a second; SIGHUP reload; `/metrics` and `/healthz`; a hardened systemd unit
  (root with only CAP_NET_ADMIN and CAP_NET_RAW, checked in the lab).
- Tested end to end with headscale and real tailscaled in network
  namespaces, with a home LAN, a café LAN with the same addresses and a public
  web host, all with a 10-minute interval so that events drive every
  reaction.
