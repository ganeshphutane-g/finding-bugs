# alertlines: spec

## Problem

Loki's ruler can alert on logs, but only on metric queries. The alert that
reaches Alertmanager says "PaymentErrors: count > 0" and nothing about which
lines matched, so whoever is paged opens Grafana to find out what happened. A
notification that showed the matching lines would answer that at once.

Evidence (checked 2026-10-19 in GitHub search):
- [loki#5844](https://github.com/grafana/loki/issues/5844) "Add log message in
  alert": 107 👍, open since 2022-04, updated 2026-04-22. Loki has 29k★.
- Duplicates #3507, #7823 and #10645.
- Grafana community threads asking the same: 46053, 87113, 49785, 118871 and
  127073.
- Loki's 3.6 and 3.7 release notes have nothing on this.

The workaround people use is to copy part of the line into a label with the
pattern parser, then put that label in the template. It gives one field, not the
lines, and makes a separate series (and alert) for every message.

Existing tools: only 0–1★ projects, mostly LLM root-cause bots.

## Verified behaviour (2026-10-19, Loki 3.7.8)

- The ruler sends `POST /api/v2/alerts` with a JSON array of alerts:
  - `labels`, `annotations`, `startsAt` and `endsAt` (in the future while
    firing);
  - `generatorURL`, which is an Explore link carrying the rule's expression,
    e.g. `/explore?left={"queries":[{"expr":"(sum by (app)(count_over_time({app=\"shop\"} |= \"ERROR\"[5m])) > 0)","queryType":"range"}]}`;
  - an `X-Scope-OrgID` header with the tenant.
- The log part of that expression, `{app="shop"} |= "ERROR"`, sent to
  `/loki/api/v1/query_range` with `direction=backward`, returns the matching
  lines with their stream labels.
- Loki's ruler sends alerts again at each evaluation while they fire, and
  Alertmanager keeps the latest annotations.

## What v1 does

`alertlines` is a small HTTP proxy. Point the Loki ruler's `alertmanager_url` at
it; it forwards to the real Alertmanager(s).

For each firing alert it:
1. **Finds the log query.**
   - An `alertlines_query` annotation (LogQL) wins.
   - Otherwise the expression in `generatorURL` is used: the range-vector
     argument of the innermost `count_over_time`, `rate`, `bytes_over_time` (or
     similar), with its range (`[5m]`).
   - Labels the expression groups by (`sum by (pod)`) and the alert carries are
     added as a label filter (`| pod="api-7f9"`), so a per-pod alert shows that
     pod's lines.
2. **Fetches the lines.**
   - It asks Loki over the range before now, newest first, as the alert's
     tenant.
   - Limits: at most N lines (default 10) and B bytes per line (default 300).
     The annotation is capped (default 4 KiB), and each query has a timeout
     (default 3 s).
3. **Redacts them.** Built-in patterns cover tokens, passwords, bearer
   credentials, API keys and URLs with passwords; your own patterns can be
   added.
4. **Adds annotations.**
   - `log_lines` holds the lines, one per line, each with its time and,
     optionally, chosen labels.
   - `log_lines_count` holds, for example, "2 lines" or "10 of 37+ lines".
   - Templates use `{{ .CommonAnnotations.log_lines }}` or
     `{{ .Annotations.log_lines }}`.
5. **Forwards** the whole batch to every Alertmanager.

Never in the way:
- Resolved alerts pass through untouched.
- If Loki is slow, down or erroring, alerts go on without the lines, after at
  most the timeout. An error never stops an alert.
- Results are cached per alert and time window, so a firing alert re-sent
  every evaluation does not query Loki every time.
- Other requests are passed to the first Alertmanager.

Operations:
- one YAML file or flags;
- `/healthz`, plus `/metrics` with alerts seen, enriched, skipped by reason,
  Loki latency and errors;
- a `check` command that takes a sample alert, or an expression, and prints the
  query and the lines it would add;
- a Docker image (distroless, non-root) and a systemd unit.

## Non-goals for v1

- Grafana-managed alerts. Their `generatorURL` points to the rule, not the
  query; reading it needs the Grafana API.
- Prometheus or Mimir rules (no logs).
- Changing Alertmanager templates for you. The README shows examples.

## Tech choices

- Go and the standard library only (YAML via `gopkg.in/yaml.v3`).
- No Loki code: Loki is AGPL-3.0 and this is MIT. The LogQL part needed (finding
  the range-vector argument) is a small, quote-aware scanner with tests drawn
  from real rule expressions.
- Tests:
  - unit tests for the scanner, the limits, redaction and the proxy (against
    fake servers);
  - integration tests against real `grafana/loki` (3.5 and latest) and
    `prom/alertmanager` containers. They push logs, wait for the ruler, and
    read the alert back from Alertmanager and from a webhook receiver.

## Milestones

1. **Query extraction and fetching:** the LogQL scanner, generatorURL decoding,
   grouping-label filters, the Loki client, limits, redaction, and `alertlines
   check`.
2. **The proxy:** the Alertmanager v2 API in and out, several upstreams, the
   timeout pass-through, the cache, `/healthz` and `/metrics`, and integration
   tests with real Loki and Alertmanager.
3. **Packaging and release:**
   - the Docker image and systemd unit;
   - README with real notification output and template examples;
   - two full reviews;
   - CI;
   - CHANGELOG.
