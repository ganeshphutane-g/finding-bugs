# Changelog

## 1.0.0 — 2026-10-21

First release.

- `xffdns run` takes DNS over UDP, TCP (several queries per connection, answered
  in any order) and TLS (DoT), and passes each query on to AdGuard Home's DoH
  endpoint with the client's address in `X-Forwarded-For`.
- The PROXY protocol, versions 1 and 2 (version 2 on UDP too), from the
  addresses in `proxy_from` only; v2 LOCAL health checks are accepted.
- Client IDs: from the DoT name (`<id>.<server_name>`), or one per listener.
- DoT certificates are read again when their files change.
- Limits: UDP queries per second per client subnet (AdGuard Home does not
  limit DoH), connections, queries in flight, idle and handshake timeouts.
  UDP answers that are too big are truncated.
- `xffdns check` sends a query as an address of your choice and reads AdGuard
  Home's query log to tell whether it took the address, and what to change if
  not.
- `/metrics` and `/healthz`.
- A distroless Docker image and a hardened systemd unit (systemd's exposure
  score 1.4).
- Tested against AdGuard Home v0.107.45, v0.107.73 and the latest release, with
  HAProxy (PROXY v1 and v2), Traefik (v2), nginx (v1) and dnsdist (v2 over UDP
  and TCP) in front. The tests read AdGuard Home's query log for the client
  address and client ID, and check per-client blocked services, the access
  list, and that a spoofed header is refused.
