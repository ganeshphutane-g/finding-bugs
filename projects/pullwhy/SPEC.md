# pullwhy: spec

## Problem

`docker pull` says almost nothing while it waits, and its errors name a symptom
rather than a cause. When a pull hangs, crawls or fails, people end up timing
`curl https://registry/v2/` by hand, reading `dockerd --debug` logs, or
guessing. The usual causes:
- a dead mirror;
- a proxy that blocks the CDN;
- a missing CA;
- Docker Hub's rate limit;
- an image without the right platform.

docker/cli#336, "docker pull should have a --verbose option to see all of the
network calls", has 244 👍 and has been open since 2017; #4161 asks for the
same.

pullwhy runs the pull the way the daemon will, step by step. It times each
step and names the likely cause, with the evidence and the fix.

## Verified behaviour (2026-10-21, Docker 29.8.2)

**Docker Hub:**
- Without login, a HEAD of a manifest returns `ratelimit-limit: 100;w=3600`
  and `ratelimit-remaining: N;w=3600`. `x-ratelimit-*` carries the same, and
  `docker-ratelimit-source: <ip>` names the address counted.
- So the limit is 100 an hour per IP without login.
- The remaining count varies between requests when an address goes out
  through several egress IPs (seen here: 50, 86, 50, 36).
- A logged-in account was not tested; there is none here.

**`docker info`** (Engine API `/info`) reports what pullwhy needs:
- `RegistryConfig.Mirrors` and `IndexConfigs`;
- `InsecureRegistryCIDRs` (127.0.0.0/8 and ::1/128 by default, so loopback
  registries may use plain HTTP);
- `HTTPProxy`, `HTTPSProxy` and `NoProxy` (from daemon.json `proxies`);
- the image store (`DriverStatus` holds `io.containerd.snapshotter.v1` for
  the containerd store).

**Mirrors** (`registry-mirrors`, docker.io images only):
- *containerd store:*
  - The mirror is asked first, as
    `HEAD /v2/library/alpine/manifests/3?ns=docker.io`. When it answers, Hub is
    not contacted at all.
  - When it refuses connections, dockerd logs "trying next host" and goes to
    registry-1.docker.io. It tries the mirror again first for every later
    fetch (index, referrers, blobs).
  - **With an unreachable mirror (packets dropped), each of those fetches
    waits 30 s.** Pulling alpine:3 took **2 min 1 s**, against about 1 s
    without the mirror.
- *classic store:*
  - The same unreachable mirror costs one 15 s `GET /v2/` timeout ("Attempting
    next endpoint for pull after error"); then Hub. That pull took 16 s.
- A mirror can serve a different manifest from Hub's. Here, a mirror holding a
  single-platform manifest made the pull resolve digest `d56c…` instead of
  Hub's index `294b…`.

**TLS:**
- Both stores read `/etc/docker/certs.d/<host:port>/ca.crt` (checked on the
  containerd store; the classic store logs `hostDir:
  /etc/docker/certs.d/<host:port>`).
- Without it, the pull fails with `tls: failed to verify certificate: x509:
  certificate signed by unknown authority`.

**Proxy:** with `proxies.https-proxy` set, dockerd sends `CONNECT` through it
for:
- `auth.docker.io:443`;
- `registry-1.docker.io:443`;
- `production.cloudfront.docker.com:443`, Hub's blob CDN, reached by a
  redirect.

A proxy or firewall that allows the registry but not the CDN host breaks
layer downloads only.

**Pull progress:** Engine API `POST /images/create` streams JSON lines:
- `Pulling fs layer`, `Downloading` (with `progressDetail.current` and
  `total`), `Download complete`, `Extracting` and `Pull complete`, each per
  layer;
- `errorDetail.message` on failure.

Timings must be taken as the lines arrive; the stream carries none.

## Users

People whose pulls are slow or fail: homelab and CI hosts behind proxies or
mirrors, private registries with their own CAs, and anyone hitting Hub's limit.

## v1 scope

`pullwhy [flags] IMAGE` (for example `nginx:alpine`, `ghcr.io/o/r:tag`, or
`registry:5000/x@sha256:…`) talks to the same daemon the docker CLI does:
`DOCKER_HOST`, the current context, or `/var/run/docker.sock`. It never
changes daemon configuration.

**1. Daemon context.**
- From `/info`: version, image store, mirrors, insecure registries and
  proxies.
- It notes whether the daemon is on this host:
  - a unix socket and a Linux host: probes see the daemon's network;
  - otherwise (Docker Desktop, a remote `DOCKER_HOST`): probes run from
    pullwhy's own network, and it says so.

**2. The plan.** What the daemon will try, in order:
- for docker.io, each mirror and then registry-1.docker.io;
- for another registry, that host.

For each endpoint, the scheme (https, or http if it is insecure) and the
certs.d directory if there is one (readable only on the daemon's host).

**3. Probes,** per endpoint, through the daemon's proxy unless `NoProxy`
matches. Each is timed:
- DNS (the addresses);
- TCP connect (or the proxy's `CONNECT`);
- the TLS handshake and certificate check, using the system roots plus
  certs.d's `ca.crt` (expired, unknown authority or wrong name, each named);
- `GET /v2/` (200 or 401, and the `WWW-Authenticate` realm);
- a token from the realm: anonymous, or with the CLI's credentials;
- `HEAD` of the manifest: digest, media type and Hub's rate-limit headers.

None of this downloads an image. On Hub, a HEAD is not counted as a pull,
according to Docker's rate-limit page (docs.docker.com/docker-hub/download-rate-limit/:
"up to two GET requests on registry manifest URLs"; HEADs not counted). It
could not be measured here, and the newer pull-usage page words it
differently, so the report says it relies on that page.

**4. Credentials,** as the CLI finds them:
- `$DOCKER_CONFIG/config.json` (default `~/.docker`): `auths`, `credsStore`
  and `credHelpers`;
- helpers are run as `docker-credential-<name> get`.

Credentials are used for the token request and never printed. The report says
which source supplied them.

**5. `--deep`** (counts as one pull on Hub, so it is opt-in):
- `GET` of the manifest, and of the index if there is one, to check that the
  requested platform (`--platform`, default the daemon's) is there;
- then a ranged GET of the largest layer, following redirects (the CDN
  host), for up to 5 s, to measure throughput.

**6. `--pull`** asks the daemon to really pull through the API, and records:
- the time to the first layer event (resolution, mirrors, auth);
- per-layer throughput;
- stalls (no progress for 10 s);
- the total time;
- on failure, the daemon's error, classified.

**7. Diagnosis.** Rules turn findings into causes, most likely first, each
with its evidence and the fix:

| Cause | Evidence |
| --- | --- |
| Unreachable or slow mirror | Timeouts, with the cost per request on the daemon's store (30 s each, containerd; one 15 s wait, classic). Fix: remove or fix it in daemon.json |
| A mirror serving another digest than the registry | A stale mirror |
| Rate limit | Remaining 0, or under 10%, or 429 |
| TLS | The certs.d path and file name to add |
| An HTTP registry not in `insecure-registries` | |
| Proxy | Unreachable or refusing; CDN host blocked; `NoProxy` missing an internal registry |
| DNS | Failure or slowness |
| Auth | 401 with or without credentials; a missing repository, or one that is private ("pull access denied") |
| The platform not in the index | |
| Slow CDN or registry throughput | |

**Output:**
- a readable report: the plan, a timeline of the probes, then "Likely cause";
- `--json` for scripts.

**Exit codes:**

| Code | Meaning |
| --- | --- |
| 0 | the pull path works and nothing slow was found |
| 1 | error |
| 2 | bad usage |
| 3 | a problem was found |
| 4 | the pull path works, but something slow was found |

## Out of scope for v1

- Pushes.
- Kubernetes (containerd's CRI, `hosts.toml`).
- Podman.
- Changing configuration.
- Pulling through BuildKit.

## Design

Go 1.24. The Engine API is called over HTTP on its socket directly, with no
moby client library. The registry protocol (distribution spec, token auth)
is implemented here.

| Package | What it does |
| --- | --- |
| `internal/engine` | /info, /version, the pull stream |
| `internal/plan` | endpoints from the daemon's config |
| `internal/probe` | DNS, TCP, proxy CONNECT, TLS, /v2/, token, manifest |
| `internal/creds` | config.json and helpers |
| `internal/diagnose` | the rules |
| `internal/report` | text and JSON output |
| `internal/cli` | the command |
| `internal/testkit` | registries, a proxy and a scratch dockerd for tests |

## Tests

**Unit tests** cover each piece against `httptest` servers: a registry with
token auth, 429s and rate-limit headers, redirects, self-signed TLS, and a
CONNECT proxy.

**Side by side** (`PULLWHY_INTEGRATION=1`): a scratch dockerd (`docker:dind`,
privileged) for each scenario, with its own daemon.json. Each scenario runs a
real `docker pull` and pullwhy against the same setup, and pullwhy must name
the cause the pull hit:

| Scenario | Setup |
| --- | --- |
| A working pull | through a registry:2 mirror |
| A dead mirror | a blackholed address, so slow |
| A refusing mirror | |
| A stale mirror | |
| A private CA | with and without certs.d |
| An HTTP registry | without `insecure-registries` |
| Basic auth | registry:2 with htpasswd; good, wrong and missing credentials |
| A proxy | working; down; blocking a host |
| Hub's rate limit | a fake registry |
| A missing platform | |

Both image stores are covered. CI runs all of it; the scenarios that reach
Hub are skipped when its remaining limit is low.

## Milestones

1. Engine API client, daemon context and the plan.
2. Probes and credentials.
3. `--pull` timeline and error classification; `--deep`.
4. The diagnosis rules and the report (text, JSON, exit codes).
5. Side-by-side tests on both stores, CI, then README, CHANGELOG, LICENSE
   and two full reviews.
