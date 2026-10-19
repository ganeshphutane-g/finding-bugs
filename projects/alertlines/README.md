# alertlines

Put the log lines that fired an alert into the alert. alertlines sits between
Loki's ruler and Alertmanager. For each firing alert it fetches the matching
lines from Loki, hides secrets in them, and adds them as an annotation. Your
Slack message, email or page then says *what* happened, not just that a
counter went above zero.

```
*Payment errors on shop-7f9*
2026-10-19T19:29:05Z pod=shop-7f9 ERROR payment 19 failed: gateway timeout after 30s
2026-10-19T19:29:05Z pod=shop-7f9 ERROR payment 18 failed: card declined (customer=4411, token=[redacted])
_2 lines_
```

That message was rendered by Alertmanager from an alert that went through a real
Loki ruler and alertlines.

## Why

Loki's ruler only evaluates metric queries. The alert it sends says
`PaymentErrors: sum(count_over_time(...)) > 0` and carries no log line, so
whoever is paged has to open Grafana to see what broke.
[loki#5844](https://github.com/grafana/loki/issues/5844), "Add log message in
alert", has 107 👍 and has been open since 2022. The Grafana forums are full of
the same question.

The usual workaround is to copy part of the line into a label with the
`pattern` parser and put that label in the template. It gives you one field
instead of the lines, and makes a separate series (and alert) for every distinct
message.

alertlines needs no change to your rules: the ruler already sends each rule's
expression in the alert's `generatorURL`, and alertlines takes the log query
from it.

## How it works

For each firing alert in a batch from the ruler:

1. **The query.**
   - alertlines takes the log part of the rule's expression:
     `sum by (pod) (count_over_time({app="shop"} |= "ERROR" [5m])) > 0` gives
     `{app="shop"} |= "ERROR"` over the last 5 minutes.
   - The alert's grouping labels become filters
     (`| pod="shop-7f9"`), so a per-pod alert shows that pod's lines.
   - An `alertlines_query` annotation on the rule overrides all this.
2. **The lines.**
   - It fetches the newest matching lines from Loki, as the alert's tenant
     (the ruler's `X-Scope-OrgID`).
   - At most 10 lines, each cut to 300 bytes, and 4 KB in all.
3. **Secrets.**
   - Values of keys named like secrets are hidden: `password`, `token`,
     `api_key`, `secret`, `session_id` and the like, in logfmt, JSON
     (escaped too) and query strings.
   - So are `Authorization` and `Cookie` values, private key blocks, passwords
     in URLs and DSNs, and well-known token formats: GitHub, GitLab, Slack,
     Stripe, AWS, Google, npm, OpenAI and JWTs.
   - You can add your own patterns.
4. **Annotations.**
   - `log_lines` holds the lines, newest first, each with its time and any
     labels you choose.
   - `log_lines_count` says, for example, "2 lines" or "the latest 10 lines
     (there are more)".
5. **On to Alertmanager.** Every Alertmanager you list gets the batch.

It never stands in the way of an alert:
- Resolved alerts, and alerts it finds no query for, pass through unchanged.
- If Loki is slow or down, the batch goes on without lines after the timeout
  (3 s).
- The ruler re-sends firing alerts at every evaluation, so lines are cached for
  a minute.

## Install

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/alertlines
make build            # ./alertlines
```

Docker:

```sh
docker build -t alertlines .
docker run -d --name alertlines -p 9097:9097 \
  -v /srv/alertlines:/etc/alertlines:ro alertlines
```

In the container, set `listen: ":9097"` (the default is `127.0.0.1:9097`), and
make the file readable to uid 65532: `chgrp 65532 alertlines.yaml && chmod 640
alertlines.yaml`.

```
```

systemd: copy the binary to `/usr/local/bin/`, `contrib/alertlines.service` to
`/etc/systemd/system/` and your file to `/etc/alertlines/alertlines.yaml`, then
`systemctl enable --now alertlines`. It runs as a throwaway user with no
capabilities.

## Quick start

1. `alertlines.yaml`:

   ```yaml
   listen: ":9097"                       # the ruler is on another host
   auth: {username: ruler, password: "${ALERTLINES_RULER_PASSWORD}"}
   alertmanagers: ["http://alertmanager:9093"]
   loki: {url: "http://loki:3100"}
   lines: {labels: [pod]}                # show the pod with each line
   ```

2. Try it on one of your rules:

   ```console
   $ alertlines check --expr 'sum by (pod) (count_over_time({app="shop"} |= "ERROR" [5m])) > 0' --label pod=shop-7f9
   alert {alertname=check, pod=shop-7f9}
     query: {app="shop"} |= "ERROR" | pod="shop-7f9"
     from:  2026-10-19T19:39:23Z to 2026-10-19T19:44:23Z
     (if that finds nothing: {app="shop"} |= "ERROR")
     keeping lines whose streams agree with {pod=shop-7f9}
     log_lines_count: 2 lines
     log_lines:
       2026-10-19T19:44:23Z ERROR payment 19 failed: gateway timeout
       2026-10-19T19:44:23Z ERROR payment 18 failed: card declined (token=[redacted])
   ```

3. Run it, and point Loki's ruler at it instead of Alertmanager:

   ```yaml
   # Loki's config
   ruler:
     alertmanager_url: http://alertlines:9097
     alertmanager_client:
       basic_auth_username: ruler
       basic_auth_password: ...           # the same as alertlines' auth
   ```

   ```sh
   alertlines run
   ```

4. Use the lines in your Alertmanager templates (below).

## Templates

Each alert gets `.Annotations.log_lines` and `.Annotations.log_lines_count`.

Slack (the output at the top of this page):

```yaml
receivers:
  - name: slack
    slack_configs:
      - channel: "#alerts"
        title: "{{ .CommonLabels.alertname }}"
        text: |
          {{ range .Alerts }}*{{ .Annotations.summary }}*
          {{ if .Annotations.log_lines }}```
          {{ .Annotations.log_lines }}
          ```
          _{{ .Annotations.log_lines_count }}_{{ end }}
          {{ end }}
```

Email:

```yaml
    email_configs:
      - to: oncall@example.com
        html: |
          {{ range .Alerts }}<h3>{{ .Annotations.summary }}</h3>
          <pre>{{ .Annotations.log_lines }}</pre>
          <p>{{ .Annotations.log_lines_count }}</p>{{ end }}
```

Webhooks get the annotations as they are, in each alert's `annotations`.

## Configuration

```yaml
listen: "127.0.0.1:9097"           # where the ruler sends alerts (the default)
auth:                              # what the ruler must send (or bearer_token)
  username: ruler
  password: "${ALERTLINES_RULER_PASSWORD}"
tenants: [team-a, team-b]          # only these tenants' alerts get lines
alertmanagers:                     # every one gets every alert; credentials
  - "http://alertmanager-1:9093"   #   for one go in its URL
  - "https://am:${ALERTLINES_AM_PASSWORD}@alertmanager-2:9093"
loki:
  url: "http://loki:3100"
  username: alertlines             # or bearer_token
  password: "${ALERTLINES_LOKI_PASSWORD}"
  tenant: ""                       # default: the tenant the ruler sends
lines:
  max: 10
  max_line_bytes: 300
  max_bytes: 4000                  # for the whole annotation
  labels: [pod, namespace]         # stream labels shown with each line
range: 5m                          # when the expression has none
max_range: 1h                      # the most it ever looks back
timeout: 3s                        # the most an alert waits for Loki (at most 8s)
cache: 1m
redact:
  builtin: true
  patterns:                        # the match, or its first group, is hidden
    - 'card \d{4}-\d{4}-\d{4}-(\d{4})'
annotation: log_lines              # and log_lines_count
```

- `${NAME}` reads environment variables whose names start with `ALERTLINES_`;
  no others.
- To choose the query yourself, add an `alertlines_query` annotation to a rule:

  ```yaml
  - alert: SlowCheckouts
    expr: 'avg_over_time({app="shop"} | json | unwrap duration_ms [5m]) > 2000'
    annotations:
      alertlines_query: '{app="shop"} | json | duration_ms > 2000'
  ```

**Endpoints.**
- `POST /api/v2/alerts` (also under a path prefix): alerts from the ruler.
- `/healthz`.
- `/metrics` (asks for `auth` too):
  - alerts received, enriched, and skipped by reason;
  - forwards by result;
  - time spent in Loki.
- Nothing else. alertlines is not a way into Alertmanager's API.

**Flags.**
- `run`: `--listen` and `--alertmanager` (repeatable) instead of the file,
  with `--loki`.
- `check`: `--tenant` for a multi-tenant Loki.

**Exit codes:** 0 fine, 1 error, 2 bad usage, 3 `check` found no lines, 4
`check` found no log query for the alert.

## Limits

- **Grafana-managed alerts are not covered.** Their `generatorURL` points to the
  rule, not the query. Alerts from Loki's own ruler are.
- **Which query is used.** For an expression with several log queries (an error
  ratio, say), the first one is used: usually the numerator. Use
  `alertlines_query` to pick another.
- **Which lines are shown.**
  - Labels the expression groups by become filters.
  - For an expression with no aggregation, or with `without (…)`, the query
    runs unfiltered. Then only lines whose streams agree with the alert's
    labels are kept. Labels such as `severity` come from the rule and no
    stream has them, so they don't count.
  - `offset` moves the window back, and no query looks back further than
    `max_range`.
- **Redaction is a safety net, not a guarantee.** It knows common shapes of
  secrets. Lines go to wherever your notifications go, so add patterns for your
  own formats, and keep secrets out of logs.
- **Who may send alerts.** Anyone who can reach alertlines can make it read
  Loki, with its own credentials, for any tenant: whatever query they put in an
  "alert". So it listens on 127.0.0.1 unless told otherwise, and when it
  listens anywhere else it says so until you set `auth`. Use `tenants` on a
  multi-tenant Loki.
- **Chat formatting.** Log lines go into the annotation as they are. In a Slack
  template, text such as `<!channel>` or `<https://x|y>` from a log line is
  formatted by Slack; escape it there if your logs carry user input.

## Roadmap

- Grafana-managed alerts, through Grafana's rule API.
- Lines from before the alert started (context), and lines around each match.
- Prometheus Alertmanager's v1 API, for very old rulers.
- Release binaries and a published image.

## Development

```sh
make test          # vet and unit tests
make test-server   # also against real Loki and Alertmanager in Docker
```

The server tests start `grafana/loki` (set `ALERTLINES_LOKI_IMAGE` for another
version) and `prom/alertmanager`. They push log lines, let the ruler fire, and
read the alert from Alertmanager's API and from a webhook. One test does the
same with Loki down: the alert must still arrive. CI runs them against Loki 3.5
and the latest release.

## License

MIT. See [LICENSE](LICENSE).
