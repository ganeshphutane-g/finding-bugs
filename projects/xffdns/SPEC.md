# xffdns: spec

## Problem

AdGuard Home decides everything per client: which per-client settings apply,
which services are blocked, the access lists, the query log, and the
statistics. It identifies a client by the source address of the query. When
something sits in front of it, every device looks like that one address.
Examples:
- a reverse proxy passing DoT (port 853, Android's "Private DNS") through;
- a load balancer;
- Docker or Podman networking that rewrites the source.

The usual fix for this is the PROXY protocol, which AdGuard Home does not
support:
- AdGuardHome#2798, "Support PROXY Protocol", has 96 👍 and has been open
  since 2021;
- #1806 asks for the same thing.

AdGuard Home does trust `X-Forwarded-For` on DoH requests from
`trusted_proxies`. xffdns uses that: it takes DNS queries as they come in, and
sends each one to AdGuard's DoH endpoint with the real client's address in
`X-Forwarded-For`.

## Verified behaviour (2026-10-21)

**AdGuard Home v0.107.79.** Its web port serves DoH over plain HTTP when
`http.doh.insecure_enabled: true` is set. The default `trusted_proxies` are
`127.0.0.0/8` and `::1/128`.

These requests come from 127.0.0.1:
- **Client by IP.** `X-Forwarded-For: 192.168.5.77` is logged as client
  192.168.5.77, protocol `doh`. A persistent client with that IP gets its own
  settings: its blocked service (youtube) is blocked for it, and for no one
  else.
- **Access list.** `disallowed_clients` applies to the forwarded IP: the query
  is answered REFUSED.
- **Client ID.** `/dns-query/kid` uses the client with ID `kid` (its blocked
  service, tiktok). When both are given, the client ID wins over the IP. The
  log shows both: client 192.168.5.77, client_id kid.
- **IPv6.** A bare IPv6 address works (`2001:db8::5`).
- **Ignored forms.** `[2001:db8::5]` and `192.168.5.77:5555` are ignored, and
  the query is logged as from 127.0.0.1.
- **Several addresses.** For `10.9.9.9, 192.168.5.77`, the first address is
  used. So xffdns must replace the header, never append to it.
- **No rate limit.** AdGuard's `ratelimit` does not apply to DoH: 20 queries
  from one forwarded IP at `ratelimit: 2` were all answered. Plain UDP to the
  same server was cut to 10 of 20. xffdns therefore needs its own per-client
  limit for UDP.
- **Failed queries.** Queries that fail upstream are not logged.

**PROXY headers, captured from:**

| Sender | What it sends | Header |
| --- | --- | --- |
| HAProxy 3.4.7, `send-proxy` | v1 | `PROXY TCP4 127.0.0.1 127.0.0.1 53988 9851\r\n` |
| HAProxy 3.4.7, `send-proxy-v2` | v2, PROXY, TCP over IPv4 | 28 bytes, no TLVs |
| Traefik 3.7.15, `serversTransport.proxyProtocol.version: 2` on a `HostSNI(*)` TCP router | v2, the same shape | 28 bytes, no TLVs |
| dnsdist 2.0.11, `useProxyProtocol=true` | v2, UDP over IPv4 (`0x21 0x12`), at the front of every datagram | no TLVs |

- dnsdist's answers come back as plain DNS.
- The captures are in `internal/proxyproto/testdata/`.

## Users

People who run AdGuard Home behind one of:
- Traefik, HAProxy, nginx stream or Caddy L4;
- a cloud or k8s load balancer with PROXY protocol;
- a container network that hides the client.

They want per-device rules, and logs that name devices.

## v1 scope

### Listeners

Any number of listeners, each one of these protocols:

| Protocol | What it is |
| --- | --- |
| `udp` | DNS over UDP |
| `tcp` | DNS over TCP, RFC 7766 framing, several queries per connection, answers in any order |
| `dot` | DNS over TLS, RFC 7858 |

**DoT.**
- xffdns terminates TLS itself, with a certificate and key from files.
- The files are re-read when they change, so a Let's Encrypt renewal needs no
  restart.
- TLS 1.2 or newer, ALPN `dot`.
- In front of it there can be nothing, or a TCP passthrough proxy that sends a
  PROXY header.

**PROXY protocol, on each listener.**
- `proxy_protocol: true` makes xffdns expect a header: v1 or v2 on TCP and
  DoT, v2 on UDP.
- `proxy_from` lists the sources (CIDRs) allowed to send headers, and is
  required whenever `proxy_protocol` is on.
- A connection from anywhere else is closed (a datagram is dropped). Otherwise
  anyone could claim any address.
- A connection with no header, or a bad one, is closed.
- v2 `LOCAL` (health checks) uses the connection's own address.
- The header must arrive within `handshake_timeout`.

**The client's address.** It is the PROXY source if there is one, and the
peer address otherwise. IPv4-mapped IPv6 is unmapped. It goes into
`X-Forwarded-For` as a bare address.

**Client IDs.**
- DoT: with `server_name: dns.example.com`, an SNI of `kid.dns.example.com`
  gives client ID `kid`. This works the way AdGuard's own DoT does.
- Any listener can have a fixed `client_id`.
- IDs are checked against AdGuard's rules: lowercase letters, digits and `-`,
  at most 64 characters.
- The query is sent to `<upstream path>/<id>`.

### Upstream

One AdGuard DoH URL:
- `http://host:port/dns-query`, which needs `insecure_enabled`;
- or `https://...`, with an optional `ca_file` and `server_name`.

**How each query is sent.**
- A POST of `application/dns-message`, with the message ID set to 0 (RFC 8484).
- The client's ID is put back in the answer.
- Connections are reused (HTTP/1.1 keep-alive, or HTTP/2 over TLS).
- `X-Forwarded-For` is set, and replaces any value already there.

**When it fails.** On a timeout (`timeout`, 4 s by default) or an HTTP error,
the client gets SERVFAIL, with its own ID and question.

### Limits

| Limit | Default |
| --- | --- |
| UDP queries per second per client subnet (/24 for IPv4, /56 for IPv6; AdGuard's defaults, as is the rate) | 20 |
| Clients tracked by the limiter (in two generations of up to this many; the older is dropped whole) | 10,000 |
| Open TCP/DoT connections, all listeners | 1,000 |
| Open TCP/DoT connections per client (IPv4 address, IPv6 `subnet_v6`; counted after the PROXY header) | 32 |
| Queries not yet answered, per client (held until the answer is written) | 64 |
| Bytes of TCP answers waiting on slow readers, in all (past it, the connection that would add more is closed) | 64 MB |
| Queries in flight per connection | 32 |
| Queries in flight in all | 2,000 |
| Idle timeout for TCP/DoT | 30 s |
| Time for the PROXY header and the TLS handshake | 5 s each |
| Query size | 4,096 bytes |
| Connections to AdGuard Home | as many as queries in flight |

- Queries must be plain queries: no answer or authority records, at most 3
  additional ones. Anything else is dropped (UDP) or closes the connection.
- Queries over the UDP limit are dropped, before they are parsed.
- Queries over the in-flight limit get SERVFAIL on UDP. On TCP, xffdns stops
  reading from that connection until queries finish. A query stops counting
  once AdGuard Home has answered it, even if its client is slow to read.
- Redirects from the upstream are not followed; they count as errors.
- UDP on a wildcard address binds an IPv4 and an IPv6-only socket and answers
  from each query's destination address (IP_PKTINFO).
- At start, one test query to AdGuard Home; its failure is logged with a hint.
  `/healthz` answers 503 while the last exchange in the past minute failed.

**UDP answers.**
- An answer larger than the client's buffer is truncated to the header and
  question, with TC set.
- The buffer is 512 bytes without EDNS. With EDNS it is the advertised size,
  at most 4,096.
- The truncated answer keeps the upstream's flags and extended rcode; error
  answers xffdns makes echo the query's RD and CD.

### Commands

- `xffdns run [-c xffdns.yaml]`: serve.
- `xffdns check [-c ... | --url ... [--ca-file ...] [--server-name ...]]
  [--ip 192.0.2.10] [--name ...] [--client-id ...] [--adguard-url ...
  --adguard-user ...]`, with the password in `XFFDNS_ADGUARD_PASSWORD`:
  - Sends one query to the upstream as the given IP and prints the answer.
    The name defaults to a new `xffdns-check-<random>.example.com`. Loopback
    and unspecified IPs are refused: AdGuard would log them either way.
  - With AdGuard API credentials, it also reads the query log. It reports
    whether AdGuard logged the forwarded IP or xffdns's own address. Its own
    address means it is not in `trusted_proxies`, and the check fails. The
    entry is the one that was not in the log before the query, so clocks do
    not matter.
- `xffdns version`.

**Exit codes:** 0 fine, 1 error, 2 bad usage, 3 `check` saw AdGuard ignore the
forwarded address.

### Observability

- `/metrics` and `/healthz` on `metrics_listen`, which is off by default:
  - queries by listener and result (answered, servfail, truncated,
    rate_limited, busy, malformed);
  - upstream latency as a histogram;
  - upstream errors;
  - open connections;
  - PROXY header errors (untrusted source, bad header);
  - connections closed at once (max_conns, max_conns_per_client, TLS);
  - certificate reloads.
- Logs:
  - one line at start for each listener;
  - one line per error at most once a minute for each kind of error;
  - no per-query logs, because AdGuard keeps those.

### Configuration

```yaml
upstream:
  url: "http://127.0.0.1:80/dns-query"
  timeout: 4s
listeners:
  - name: dot
    protocol: dot
    address: ":853"
    cert_file: /etc/xffdns/fullchain.pem
    key_file: /etc/xffdns/privkey.pem
    server_name: dns.example.com
    proxy_protocol: true
    proxy_from: ["172.18.0.0/16"]
  - name: lan
    protocol: udp
    address: "192.168.1.2:53"
limits: {udp_qps: 20}
metrics_listen: "127.0.0.1:9354"
```

`${XFFDNS_*}` environment variables are expanded, and no others, in these
fields: the upstream's url, ca_file and server_name, metrics_listen, and each
listener's name, address, cert_file, key_file, server_name, client_id and
proxy_from entries.

### Packaging

- A Docker image (distroless, non-root, with `NET_BIND_SERVICE` needed for
  port 53/853).
- A hardened systemd unit.
- MIT.

## Out of scope for v1

- DoQ and DoH listeners. A reverse proxy in front of DoH already sets
  `X-Forwarded-For` for AdGuard.
- Caching, and more than one upstream.
- EDNS Client Subnet.
- AdGuard's own DoT client ID through PROXY TLVs.

## Design

Go 1.24. The only dependency is `gopkg.in/yaml.v3`. The DNS and PROXY
protocol code is written here from the RFCs and the PROXY protocol spec
(haproxy.org).

| Package | What it does |
| --- | --- |
| `internal/dnsmsg` | header, question and OPT parsing; SERVFAIL/REFUSED; truncation |
| `internal/proxyproto` | v1/v2 parsing from a stream and from a datagram |
| `internal/ratelimit` | token buckets by subnet, bounded |
| `internal/upstream` | the DoH client |
| `internal/server` | the UDP, TCP and DoT listeners |
| `internal/certs` | reloading certificate files |
| `internal/config` | the configuration file |
| `internal/metrics` | the metrics endpoint |
| `internal/cli` | the commands |
| `internal/testkit` | AdGuard Home setup for tests |

## Milestones

1. **Wire formats.**
   - `dnsmsg` and `proxyproto`, with tests.
   - The proxyproto tests use the captured headers, plus malformed,
     truncated, IPv6 and v2 TLV cases.
2. **Upstream and config.**
   - The DoH client, against an `httptest` server.
   - Config loading and validation.
   - The rate limiter.
3. **Listeners.**
   - UDP, TCP and DoT with PROXY handling, the limits, metrics and certificate
     reloads.
   - The `run` and `check` commands.
   - Unit tests over loopback.
4. **Against real servers** (`XFFDNS_INTEGRATION=1`).
   - AdGuard Home in Docker, with persistent clients by IP and by ID.
   - Checks, read from the query log and from blocked answers: queries over
     UDP, TCP and DoT give the right client IP, client ID and blocked
     services.
   - In front of xffdns: HAProxy (v1 and v2), Traefik (v2), nginx (v1) and
     dnsdist (v2 over UDP and TCP).
   - A spoofed header from a source outside `proxy_from` is refused.
   - CI matrix: AdGuard latest; v0.107.73, the last with
     `tls.allow_unencrypted_doh` (v0.107.74 renamed it
     `http.doh.insecure_enabled`); and v0.107.45.
5. **Release.**
   - Dockerfile, systemd unit, README, CHANGELOG, LICENSE.
   - A full review, with every finding fixed.
