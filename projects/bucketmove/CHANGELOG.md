# Changelog

## 1.0.0 — 2026-10-13

First release.

- `inventory`: buckets, versions, delete markers, sizes and every feature in use.
- `check`: probes the target with a scratch bucket and reports, per bucket, what a move
  would lose or change. Exit code 3 on loss.
- `copy`: every version oldest first, delete markers, headers, metadata, tags, storage
  classes, retention, legal holds, lifecycle, policy, CORS, bucket tags, default
  retention; crash-safe journal and resume; refuses on loss without `--accept-loss`.
- `verify`: version-by-version comparison with SHA-256 of the content.
- Endpoints from `mc` aliases, `MC_HOST_*`, URLs and environment variables.
- Tested against MinIO, Garage v2.1.0, SeaweedFS 4.48 and RustFS 1.0.1.
