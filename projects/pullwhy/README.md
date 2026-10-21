# pullwhy

Why is my `docker pull` slow, or failing? pullwhy walks the pull the way your
Docker daemon will: its mirrors, its proxy, its certificates, your credentials,
the registry's auth and the manifest. It times each step and names the likely
cause, with the evidence and the fix.

```console
$ pullwhy 172.17.0.1:5443/team/app:1
...
Likely cause

problem: 172.17.0.1:5443's certificate is signed by a CA the daemon does not know
         - Get "https://172.17.0.1:5443/v2/": tls: failed to verify certificate: x509: certificate signed by unknown authority (the certificate presented was issued by CN=Example Lab CA)
         Fix: Copy the registry's CA certificate to /etc/docker/certs.d/172.17.0.1:5443/ca.crt on the daemon's host (no restart needed), or add the CA to the host's trust store and restart Docker.
```

## Why

`docker pull` says little while it waits, and when it fails it names the
symptom, not the cause.
[docker/cli#336](https://github.com/docker/cli/issues/336), "docker pull
should have a --verbose option to see all of the network calls", has 244 👍 and
has been open since 2017. The usual way out is `dockerd --debug`, a `curl` of
`/v2/` by hand, and guessing.

The causes are few and well known, but each looks different from the
outside:

| Cause | What it looks like from outside |
| --- | --- |
| **A dead registry mirror.** | A slow pull, with no error at all. Measured on Docker 29 with the containerd image store, pulling `alpine:3` took **3 min 32 s instead of 1 s**: the daemon waits 30 s for the mirror on each request before falling back. The classic store waits 15 s once. |
| A proxy that lets the registry through but not the CDN its layers come from (`production.cloudfront.docker.com` for Docker Hub). | |
| A registry signed by a private CA, or TLS interception by a company proxy. | |
| A plain-HTTP registry missing from `insecure-registries`. | |
| Docker Hub's pull limit: 100 an hour per address without login, from Hub's own headers. | |
| A login that is missing, wrong or expired. A private repository looks like a missing one: Docker Hub answers both with "unauthorized". | |
| A mirror that asks for a login. On the containerd store the daemon sends it no credentials; after `docker login` the pull stops there instead of falling back. | |
| An image with no build for your platform. | |

pullwhy tells these apart. It changes nothing: no daemon settings, and no
pull unless you ask for one.

## Install

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/pullwhy
make build                    # ./pullwhy
sudo cp pullwhy /usr/local/bin/
```

Or in Docker:

```sh
docker build -t pullwhy .
docker run --rm --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/docker/certs.d:/etc/docker/certs.d:ro \
  -v ~/.docker:/root/.docker:ro \
  pullwhy nginx:alpine
```

In a container, use `--network host` so the probes see the daemon's network.
Credential helpers (`docker-credential-*`) are not in the image, so
credentials from a helper are not available there; `auths` in config.json
work.

## Quick start

```sh
pullwhy nginx:alpine          # check the path the daemon will take: no download
pullwhy --pull nginx:alpine   # also have the daemon really pull it, and time that
pullwhy --deep --platform linux/arm64 nginx:alpine   # also check the platform and download speed
```

It uses the same daemon as the docker CLI: `DOCKER_HOST`, the current
`docker context`, or `/var/run/docker.sock`.

## What it checks

1. **The daemon's settings,** from `docker info`:
   - its image store (containerd or classic, which retry differently);
   - `registry-mirrors`;
   - `insecure-registries`, including the default loopback range;
   - its proxy and `no-proxy`.
2. **The plan.** For a Docker Hub image, each mirror and then
   registry-1.docker.io; for another registry, HTTPS, then plain HTTP if it is
   insecure. Each endpoint comes with the proxy the daemon uses for it, and with
   `/etc/docker/certs.d/<host>`, if that directory exists.
3. **Each endpoint,** timed by phase (DNS, connect, TLS, waiting):
   - `GET /v2/`;
   - the token, anonymous or with your credentials;
   - a `HEAD` of the manifest, giving the digest, the media type and Docker
     Hub's rate-limit headers.

   Docker documents that a HEAD does not count as a pull.
4. **Your credentials,** found as the CLI finds them: `credHelpers`, then
   `credsStore`, then `auths` in `$DOCKER_CONFIG/config.json`. They are never
   printed.
5. **`--deep`** reads the manifest (and the index), checks that your platform is
   in it, and reads up to 8 MB of the largest layer for at most 5 s. It follows
   redirects to the CDN, so a blocked CDN host shows. On Docker Hub this counts
   as one pull.
6. **`--pull`** has the daemon really pull through its API. It reads the
   progress stream for:
   - the time until the layers start;
   - the speed of each layer;
   - stalls;
   - the daemon's own error, classified.

## Examples

All of these are real runs.

**A dead mirror.** `"registry-mirrors": ["http://10.255.255.1:5000"]` on
Docker 29.9.2, containerd store. The address that Docker Hub counted is
changed here.

```console
$ pullwhy --timeout 5s --pull alpine:3
pullwhy docker.io/library/alpine:3

daemon       Docker 29.9.2, containerd image store, unix:///var/run/docker.sock (default)
credentials  none (anonymous)

1. mirror http://10.255.255.1:5000
   ping      GET  /v2/
             no answer in 5s

2. registry https://registry-1.docker.io
   ping      GET  /v2/
             401, 117ms (dns 1ms, connect 0ms, tls 2ms, wait 111ms)
   token     GET  https://auth.docker.io/token
             200, 217ms (dns 17ms, connect 0ms, tls 3ms, wait 195ms)
   manifest  HEAD /v2/library/alpine/manifests/3
             200, 633ms (wait 633ms)
   found     sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 (a multi-platform index)
   limit     100 of 100 pulls left per 3600s, counted for 203.0.113.7

pull by the daemon
   start     first layer after 2m31s
   layer     e2de96513ba9 done at 3m1s
   layer     6d0606d1815c (no progress reported)
   layer     797dd00a0fc7 (no progress reported)
   done      Downloaded newer image for alpine:3 in 3m31s

Likely cause

slow:    The mirror http://10.255.255.1:5000 does not answer, and the daemon waits for it on every pull
         - GET http://10.255.255.1:5000/v2/: no answer in 5s
         - with the containerd image store, the daemon waits about 30 s for it on each request of a pull before it tries the next endpoint (manifest, index, each layer...); in tests a pull of alpine:3 took 2 to 3.5 min instead of 1 s
         Fix: Remove it from "registry-mirrors" in /etc/docker/daemon.json (or fix it), then restart Docker.

slow:    The daemon spent 2m31s resolving the image before any layer
         - likely time spent waiting for the mirror http://10.255.255.1:5000

slow:    The pull made no progress for 30s (before the downloads began)
         - likely time spent waiting for the mirror http://10.255.255.1:5000

slow:    The pull made no progress for 30s (after the layers were extracted)
         - likely time spent waiting for the mirror http://10.255.255.1:5000
```

**Fixed by its own advice.** After `ca.crt` was copied into
`/etc/docker/certs.d/172.17.0.1:5443/`, as the first example said:

```console
$ pullwhy 172.17.0.1:5443/team/app:1
pullwhy 172.17.0.1:5443/team/app:1

daemon       Docker 29.9.2, containerd image store, unix:///var/run/docker.sock (default)
credentials  none (anonymous)

1. registry https://172.17.0.1:5443  (certificates from /etc/docker/certs.d/172.17.0.1:5443)
   ping      GET  /v2/
             200, 6ms (connect 0ms, tls 3ms, wait 2ms)
   manifest  HEAD /v2/team/app/manifests/1
             200, 2ms (wait 2ms)
   found     sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395 (a single-platform manifest)

Nothing wrong found: the daemon can reach the image, and nothing was slow.
```

**TLS interception.** For a public registry such as Docker Hub, an unknown CA
means something swapped the certificate. pullwhy says so, and names the
issuer of the certificate it was given:

```
problem: Something between the daemon and registry-1.docker.io intercepts TLS
         - ... x509: certificate signed by unknown authority (the certificate presented was issued by CN=...)
         - registry-1.docker.io uses a publicly trusted certificate, so an unknown CA means another certificate was put in its place (a company proxy, antivirus or a captive portal)
```

## Flags and output

| Flag | |
| --- | --- |
| `--pull` | also have the daemon really pull, and time it |
| `--deep` | also read the manifest and the start of the largest layer (one pull on Docker Hub) |
| `--platform os/arch[/variant]` | for `--deep` and `--pull` (default: the daemon's) |
| `--timeout 10s` | the most to wait for each request |
| `--pull-timeout 10m` | the most to wait for `--pull` |
| `--json` | the whole report as JSON, for scripts |

**Exit codes:**

| Code | Meaning |
| --- | --- |
| 0 | fine |
| 1 | error (the daemon cannot be reached, for example) |
| 2 | bad usage |
| 3 | a problem was found |
| 4 | it works, but something is slow |
| 5 | a step could not be checked (a proxy password the daemon hides, for example), and nothing worse was found |

When `--pull` succeeds, what the probes saw as failing is reported as a note:
the daemon's own pull is the better witness.

## Limits

- **Where the probes run.** pullwhy probes from where it runs. With a local
  daemon on Linux that is the daemon's own network. With Docker Desktop, a
  VM-based daemon on macOS (colima and the like), a rootless daemon (its own
  network namespace) or a remote `DOCKER_HOST`, it is not, and pullwhy says
  so; `--pull` still shows what the daemon itself does.
- **Proxy passwords.** The daemon hides proxy passwords in `docker info`, so
  through a proxy that needs one pullwhy says it could not check (exit 5),
  rather than guessing; `--pull` shows what the daemon gets.
- **Daemon addresses.** `ssh://` and Windows named pipes are not supported:
  use `unix://` or `tcp://`.
- **Not covered:**
  - pushes;
  - BuildKit's own pulls;
  - Kubernetes (containerd's CRI and `hosts.toml`);
  - Podman.
- **The cost of a dead mirror** depends on the daemon's version
  and image store; the numbers above were measured on Docker 29.

## Roadmap

- Podman, and containerd's `hosts.toml` for Kubernetes nodes.
- A probe from inside the daemon's network, for Docker Desktop.
- Pushes.
- Release binaries and a published image.

## Development

```sh
make test          # vet and unit tests
make test-docker   # also side by side with real pulls (privileged Docker)
```

The side-by-side tests start a throwaway daemon (`docker:dind`) for each case,
with its own `daemon.json`, on both image stores. Each case runs a real
`docker pull` and pullwhy against the same setup, and pullwhy must name the
cause the pull hit. The cases:
- a working registry;
- plain HTTP without `insecure-registries`;
- a private CA, with and without `certs.d`;
- basic auth, with no, wrong and right credentials;
- a missing tag;
- a missing platform;
- a working and a dead proxy;
- a stale mirror;
- a dead mirror.

The mirror cases need Docker Hub from inside the throwaway daemon. Where
outgoing TLS is intercepted, set `PULLWHY_DIND_CA` to a CA bundle the daemon
should trust.

## License

MIT. See [LICENSE](LICENSE).
