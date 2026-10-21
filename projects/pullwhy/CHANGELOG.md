# Changelog

## 1.0.0 — 2026-10-22

First release.

- `pullwhy IMAGE` walks a pull the way the daemon will, and names the likely
  cause with its evidence and the fix.
  - **The daemon's settings:** its image store, mirrors, insecure registries,
    proxy and no-proxy.
  - **Each endpoint:** a timed ping, token and manifest HEAD, with Docker Hub's
    rate-limit headers.
  - **Credentials** found as the CLI finds them.
- Causes it tells apart:
  - dead, refusing or stale mirrors, with the cost on each image store;
  - private CAs, and TLS interception on public registries (the issuer is
    named);
  - plain HTTP without insecure-registries;
  - proxy failures, including a blocked CDN host;
  - DNS, refused connections and timeouts;
  - missing or rejected logins;
  - missing tags;
  - Docker Hub's rate limit;
  - missing platforms;
  - slow downloads and stalls.
- `--deep` checks the platform in the index, and the download speed from the
  layer's real host (CDN redirects followed).
- `--pull` has the daemon really pull, and reports the time to the first
  layer, the speed of each layer, stalls and the daemon's error.
- `--json`, and exit codes for scripts (0 fine, 3 problem, 4 slow).
- Tested side by side with real `docker pull`s in throwaway daemons, on both
  the containerd and the classic image stores. A dead mirror measured 3 min
  32 s against 1 s on containerd, and 16 s on classic.
