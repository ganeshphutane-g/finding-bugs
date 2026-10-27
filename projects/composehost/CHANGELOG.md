# Changelog

## 1.0.0 — unreleased

First release.

- A Docker Compose provider service (`type: composehost`, Compose 2.36.1 or
  later) that runs host-side steps on `docker compose up`, before the
  services that depend on it, and fails `up` with the step's message when
  one fails:
  - `mkdir`, with `owner` and `mode`: bind-mount directories created if
    missing, and their owner and mode set on every `up` when they differ.
    Symbolic links are refused; parents get 0755.
  - `wait-mount` (a real mount point, by /proc/self/mountinfo),
    `wait-path` and `wait-tcp`, within `wait-timeout`.
  - `run`: a host command in the project directory.
  - `env`: a host command whose `KEY=VALUE` output becomes environment
    variables of the dependent services. Its output is never logged.
- `stop` and `down`: host commands on `docker compose stop` and `down`.
- `composehost check`: shows a project's steps (read through `docker
  compose config`), catches options Compose would drop silently, and looks
  at the waits once. Exit codes 0, 1 and 3.
- Progress and errors in Compose's own output; whole command output with
  `docker compose --verbose`.
- Works on PATH as `composehost`, or as the Docker CLI plugin
  `docker-composehost`.
- End-to-end tests against real dockerd and Compose in CI.
