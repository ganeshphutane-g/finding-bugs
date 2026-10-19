# Changelog

## 1.0.0 — 2026-10-20

First release.

- `alertlines run`: a proxy for Loki's ruler.
  - For each firing alert it finds the rule's log query in the alert's
    `generatorURL` (or an `alertlines_query` annotation), with its range and
    offset.
  - It fetches the matching lines for the alert's own series as the alert's
    tenant, and adds them as `log_lines` and `log_lines_count` annotations.
  - It passes every alert on to every Alertmanager, with the tenant header.
- Safe by default:
  - secrets in lines are redacted (keys, headers, cookies, private keys, URL
    and DSN passwords, common token formats, plus your own patterns);
  - limits on lines and bytes, a timeout, and a cap on how far back it looks;
  - alerts are never held up or lost when Loki is slow or down;
  - it listens on 127.0.0.1 unless told otherwise, with optional basic or
    bearer auth for the ruler and a tenant allowlist;
  - it does not pass any other request through to Alertmanager.
- `alertlines check --expr/--alert` shows the query and the lines it would
  add.
- `/healthz`, plus `/metrics` with alerts received, enriched and skipped by
  reason, forwards, and time in Loki.
- A distroless Docker image and a hardened systemd unit (systemd's exposure
  score 1.2).
- Tested against real Loki 3.5 and 3.7 rulers and a real Alertmanager. The
  tests cover:
  - per-series lines;
  - redaction in the notification;
  - Loki down;
  - every cut query accepted by Loki.
