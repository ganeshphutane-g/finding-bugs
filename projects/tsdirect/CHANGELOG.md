# Changelog

## 1.0.0 — 2026-10-24

First release.

- `tsdirect run` adds `ip rule to PREFIX lookup main priority 5200` for each
  configured LAN while the machine is really on it, so the LAN's traffic
  stays on the LAN instead of going through Tailscale's accepted subnet
  routes (table 52), and removes it as soon as it is not.
- "Really on it": the prefix is a directly connected network on an interface
  that is up and not Tailscale's, and its fingerprint matches: the gateway's
  MAC (asked fresh with ARP, not taken from the neighbour cache), a TCP probe
  made from the LAN interface (bound to it, so the tailnet cannot answer it),
  or both. Anything unknown counts as "not this LAN".
- Its rules carry their own protocol value (`--rule-proto`, 0xd1), so it
  only ever removes its own: on exit, at start after a crash, with stale
  priorities or LANs no longer configured, and with `tsdirect cleanup`.
- `tsdirect check` (exit code 3 when on a LAN's addresses with a fingerprint
  that does not match) and `tsdirect learn PREFIX` (prints the config line
  for the network the machine is on).
- Reacts to netlink events within a second, and looks again every
  `--interval`; config reload on SIGHUP; `/metrics` and `/healthz`; one log
  line per rule change and per changed verdict.
- A hardened systemd unit that runs it unprivileged with CAP_NET_ADMIN and
  CAP_NET_RAW.
- Tested end to end with headscale and real tailscaled in network
  namespaces: home LAN, a café with the same addresses, leaving, restarting
  tailscaled, a crash, and a probe that the tailnet could have answered.
