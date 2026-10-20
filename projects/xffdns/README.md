# xffdns

Real client addresses for AdGuard Home when something stands in front of it.

AdGuard Home does everything per client: per-client settings, blocked
services, access lists, the query log, and statistics. A reverse proxy, a load
balancer or a container network in front of it hides the clients, so every
device shows up as the proxy's one address.

xffdns sits between the two:
- It takes DNS over UDP, TCP and TLS (DoT, which is Android's "Private DNS"),
  with or without a PROXY protocol header from HAProxy, Traefik, nginx or
  dnsdist.
- It passes each query on to AdGuard Home's DoH endpoint, with the real client
  in `X-Forwarded-For`.
- AdGuard Home then sees each device again.

```
phone (DoT, 203.0.113.9) ──▶ Traefik :853 ──PROXY v2──▶ xffdns ──DoH + X-Forwarded-For: 203.0.113.9──▶ AdGuard Home
laptop (DNS, 192.168.1.20) ─────────────────────────▶ xffdns ──DoH + X-Forwarded-For: 192.168.1.20──▶ AdGuard Home
```

## Why

AdGuard Home has no PROXY protocol support.
[AdGuardHome#2798](https://github.com/AdguardTeam/AdGuardHome/issues/2798),
"Support PROXY Protocol", has 96 👍 and has been open since 2021; #1806 asks
for the same. Until then, the only thing a proxy can pass it is
`X-Forwarded-For` on DoH, from a trusted proxy. xffdns turns every other kind
of query into that.

Tested against AdGuard Home v0.107.45, v0.107.73 and the latest release. What
follows the forwarded address:
- the query log and statistics;
- per-client settings and blocked services, for a client set up by address;
- the access list: a disallowed address gets REFUSED.

Client IDs work too. A DoT name such as `kid.dns.example.com` gives client ID
`kid`, the way AdGuard Home's own DoT does.

## Install

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/xffdns
make build            # ./xffdns
```

Docker:

```sh
docker build -t xffdns .
docker run -d --name xffdns -p 853:853 -p 53:53/udp -p 53:53 \
  -v /srv/xffdns:/etc/xffdns:ro xffdns
```

**The image runs as uid 65532.** On Docker's own networks it can still bind
ports 53 and 853, because Docker allows it there. On the host network it
cannot. Use high ports there, or run it as root with every capability dropped
but one:

```sh
docker run --network host --user 0:0 --cap-drop ALL --cap-add NET_BIND_SERVICE ...
```

**systemd:**
1. Copy the binary to `/usr/local/bin/`, `contrib/xffdns.service` to
   `/etc/systemd/system/`, and your file to `/etc/xffdns/xffdns.yaml`.
2. Run `systemctl enable --now xffdns`.

It runs as a throwaway user, with only the capability to bind low ports. The
unit's comments show how to hand it Let's Encrypt keys.

## Quick start

### 1. Let AdGuard Home take X-Forwarded-For from xffdns

**Plain HTTP for DoH.** In `AdGuardHome.yaml`, allow DoH over plain HTTP, so
that xffdns can talk to it without TLS:

| AdGuard Home | Setting |
| --- | --- |
| v0.107.74 and newer | `http.doh.insecure_enabled: true` |
| v0.107.73 and older | `tls.allow_unencrypted_doh: true` |

You can skip this and use `https://`, but then use the name AdGuard Home's
certificate is for.

**Trusted proxies.** Make sure the address xffdns connects from is in
`dns.trusted_proxies`:
- Its default (`127.0.0.0/8`, `::1/128`) covers xffdns on the same host.
- If xffdns runs in a container, add the container's network, for example
  `172.18.0.0/16`.

Restart AdGuard Home.

### 2. Write `xffdns.yaml`

```yaml
upstream:
  url: "http://127.0.0.1:80/dns-query"   # AdGuard Home's web address + /dns-query
listeners:
  - name: dot                            # Android Private DNS, behind Traefik
    protocol: dot
    address: ":8853"
    cert_file: /etc/xffdns/fullchain.pem
    key_file: /etc/xffdns/privkey.pem
    server_name: dns.example.com         # kid.dns.example.com → client ID "kid"
    proxy_protocol: true
    proxy_from: ["172.18.0.2"]           # Traefik's address
  - name: lan                            # plain DNS for the LAN
    protocol: udp
    address: "192.168.1.2:53"
  - name: lan-tcp
    protocol: tcp
    address: "192.168.1.2:53"
```

### 3. Check that AdGuard Home trusts it

This is real output from AdGuard Home v0.107.79:

```console
$ XFFDNS_ADGUARD_PASSWORD=... xffdns check -c xffdns.yaml --ip 192.168.5.77 --adguard-user admin
query   xffdns-check.example.com A, as 192.168.5.77, to http://127.0.0.1:3080/dns-query
answer  NOERROR, 1 records, in 2ms
log     client 192.168.5.77

AdGuard Home sees the address xffdns sends.
```

`check` sends one query as `--ip` (192.0.2.10 by default), then reads AdGuard
Home's query log. If AdGuard Home logged xffdns's own address, `check` says
so and exits 3:

```console
log     client 127.0.0.1

AdGuard Home logged the query as from 127.0.0.1, not 192.168.5.77: it does not trust xffdns's X-Forwarded-For.
Add the address xffdns connects from (127.0.0.1, if that is it) to dns.trusted_proxies in AdGuardHome.yaml, and restart it.
```

When plain-HTTP DoH is off, AdGuard Home answers 404, and `check` names the
setting.

### 4. Run it

```sh
xffdns run -c xffdns.yaml
```

Then point your proxy, or your clients, at it.

## Proxies in front of it

All four are tested with xffdns and AdGuard Home in CI.

**HAProxy**, DoT passed through (xffdns has the certificate):

```
frontend dot
  mode tcp
  bind :853
  default_backend xffdns
backend xffdns
  mode tcp
  server x 127.0.0.1:8853 send-proxy-v2   # or send-proxy (v1)
```

**Traefik v3:**

```yaml
tcp:
  routers:
    dot: {entryPoints: [dot], rule: "HostSNI(`*`)", service: xffdns}
  services:
    xffdns:
      loadBalancer:
        serversTransport: pp
        servers: [{address: "xffdns:8853"}]
  serversTransports:
    pp: {proxyProtocol: {version: 2}}
```

**nginx**, which sends v1:

```
stream {
  server {
    listen 853;
    proxy_pass 127.0.0.1:8853;
    proxy_protocol on;
  }
}
```

**dnsdist**, UDP and TCP:

```lua
newServer({address="127.0.0.1:5300", useProxyProtocol=true})
```

Give xffdns a `udp` and a `tcp` listener on `:5300`, both with
`proxy_protocol: true`.

## Configuration

```yaml
upstream:
  url: "http://127.0.0.1:80/dns-query"
  timeout: 4s                  # then the client gets SERVFAIL
  ca_file: ""                  # for https:// with a private CA
  server_name: ""              # for https://, if not the URL's host
listeners:
  - name: dot                  # for logs and metrics (default: protocol/address)
    protocol: dot              # udp, tcp or dot
    address: ":853"
    cert_file: ...             # dot: read again when the files change
    key_file: ...
    server_name: dns.example.com  # dot: <id>.dns.example.com gives client ID <id>
    client_id: ""              # or one client ID for every query here
    proxy_protocol: false      # true: a PROXY header (v1/v2; v2 on UDP) is required...
    proxy_from: []             # ...and only these addresses or CIDRs may send one
limits:
  udp_qps: 50                  # UDP queries a second per client subnet (0: no limit)
  subnet_v4: 24                # the subnets it counts by
  subnet_v6: 56
  clients: 10000               # subnets it keeps track of
  max_conns: 1000              # TCP and DoT connections
  max_inflight_per_conn: 32
  max_inflight: 2000           # queries waiting on AdGuard Home
  idle_timeout: 30s            # TCP and DoT
  handshake_timeout: 5s        # the PROXY header and the TLS handshake
metrics_listen: ""             # such as 127.0.0.1:9354: /metrics and /healthz
```

- `${XFFDNS_...}` in the file is read from the environment. No other variables
  are.
- A PROXY header from an address outside `proxy_from` is refused: the
  connection is closed, or the datagram dropped. Without that rule, anyone
  could claim any address.
- A v2 `LOCAL` header, which proxies send for health checks, counts as the
  proxy itself.

**UDP.**
- Answers larger than the client can take are truncated, with TC set, so the
  client retries over TCP. A client can take 512 bytes, or its EDNS size up to
  4096.
- AdGuard Home's own `ratelimit` does not apply to DoH. That is why xffdns has
  `udp_qps`. Queries over the limit are dropped, as AdGuard Home does.

**Metrics:**
- `xffdns_queries_total` by listener and result: answered, servfail,
  truncated, rate_limited, busy, malformed;
- `xffdns_upstream_seconds`;
- `xffdns_upstream_errors_total` by kind;
- `xffdns_proxy_errors_total`;
- `xffdns_conns_refused_total`;
- `xffdns_open_conns`;
- `xffdns_cert_reloads_total`.

**Exit codes:**

| Code | Meaning |
| --- | --- |
| 0 | fine |
| 1 | error |
| 2 | bad usage |
| 3 | `check` saw AdGuard Home ignore the forwarded address |

## Limits

- **The protocol in AdGuard Home's log is `doh`** for every query, whatever the
  client used.
- **One upstream.** xffdns has no cache, and passes every query on.
- **DoQ and DoH listeners are not included.** A proxy in front of AdGuard Home's
  own DoH can already set `X-Forwarded-For` itself.
- **Trust.** AdGuard Home trusts `X-Forwarded-For` from every address in
  `trusted_proxies`. Anything else that can reach its web port from there can
  claim any client address too. That is how AdGuard Home works, with or
  without xffdns.

## Roadmap

- DoQ (DNS over QUIC) listeners.
- Client IDs from PROXY v2 TLVs, for proxies that end TLS themselves.
- Release binaries and a published image.

## Development

```sh
make test          # vet and unit tests
make test-server   # also against real servers in Docker
```

The server tests run on the host network. They start these images:
- AdGuard Home (`XFFDNS_ADGUARD_IMAGE` picks the version);
- HAProxy;
- Traefik;
- nginx;
- dnsdist.

Clients connect from addresses such as 127.0.0.77, through each proxy. The
tests then read AdGuard Home's query log for the address, the client ID,
blocked services and the access list. A spoofed header must not get through.

CI runs these tests against AdGuard Home v0.107.45, v0.107.73 and the latest
release. AdGuard Home's setup wizard needs port 3000 free.

## License

MIT. See [LICENSE](LICENSE).
