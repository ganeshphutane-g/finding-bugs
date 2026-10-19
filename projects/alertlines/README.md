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
   - Values of keys that look secret (`password=`, `token=`, `api_key`,
     `secret`, `session_id`, `auth`…) are hidden.
   - So are bearer and basic credentials, passwords in URLs, and well-known
     token formats (GitHub, Slack, Stripe, AWS, JWTs).
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

systemd: copy the binary to `/usr/local/bin/`, `contrib/alertlines.service` to
`/etc/systemd/system/` and your file to `/etc/alertlines/alertlines.yaml`, then
`systemctl enable --now alertlines`. It runs as a throwaway user with no
capabilities.

## Quick start

1. `alertlines.yaml`:

   ```yaml
   alertmanagers: ["http://alertmanager:9093"]
   loki: {url: "http://loki:3100"}
   lines: {labels: [pod]}      # show the pod with each line
   ```

2. Try it on one of your rules:

   ```console
   $ alertlines check --expr 'sum by (pod) (count_over_time({app="shop"} |= "ERROR" [5m])) > 0' --label pod=shop-7f9
   alert {alertname=check, pod=shop-7f9}
     query: {app="shop"} |= "ERROR" | pod="shop-7f9"
     from:  2026-10-19T19:15:25Z to 2026-10-19T19:20:25Z
     log_lines_count: 2 lines
     log_lines:
       2026-10-19T19:20:25Z ERROR payment 19 failed: gateway timeout
       2026-10-19T19:20:25Z ERROR payment 18 failed: card declined (token=[redacted])
   ```

3. Run it, and point Loki's ruler at it instead of Alertmanager:

   ```yaml
   # Loki's config
   ruler:
     alertmanager_url: http://alertlines:9097
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
listen: ":9097"                    # where the ruler sends alerts
alertmanagers:                     # every one gets every alert
  - "http://alertmanager-1:9093"
  - "http://alertmanager-2:9093"
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
timeout: 3s                        # the most an alert waits for Loki
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
- `POST /api/v2/alerts`: alerts from the ruler.
- `/healthz` and `/metrics`, the latter covering:
  - alerts received, enriched, and skipped by reason;
  - forwards by result;
  - time spent in Loki.
- Any other path is passed to the first Alertmanager.

**Exit codes:** 0 fine, 1 error, 2 bad usage, 3 `check` found no lines.

## Limits

- **Grafana-managed alerts are not covered.** Their `generatorURL` points to the
  rule, not the query. Alerts from Loki's own ruler are.
- **Which query is used.** For an expression with several log queries (an error
  ratio, say), the first one is used: usually the numerator. Use
  `alertlines_query` to pick another.
- **Labels the expression groups by** become filters. For an expression with no
  aggregation, the alert's labels are tried as filters first. If that finds
  nothing, the query runs without them, because some of those labels come from
  the rule, not the logs.
- **Redaction is a safety net, not a guarantee.** It knows common shapes of
  secrets. Lines go to wherever your notifications go, so add patterns for your
  own formats, and keep secrets out of logs.
- **Security of the listener.** The listener has no authentication, like
  Alertmanager's own API. Keep it on the same network as the ruler.

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
