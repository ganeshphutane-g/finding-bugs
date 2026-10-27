# Changelog

## 1.0.0 — 2026-10-28

First release.

- A Docker Compose provider service (`type: composehost`; tested with
  Compose v5.6.0) that runs host-side steps on `docker compose up`, before
  the services that depend on it, and fails `up` with the step's message
  when one fails. The steps happen in the project directory, found from the
  Compose process's own arguments (Compose runs providers in its caller's
  directory; Docker CLI flags before `compose`, the logical `$PWD` and
  Compose's env-file variables are handled, and an unknown flag is an
  error, not a guess):
  - `mkdir`, with `owner` and `mode`: bind-mount directories created if
    missing, and their owner and mode set on every `up` when they differ.
    Paths are walked with openat and O_NOFOLLOW; a symbolic link is
    followed only in a directory that only root (or composehost's user)
    can change. Created directories get exact modes (parents 0755),
    whatever the umask. A `wait-mount` path is only created; paths inside
    one are made after the waits. `/` and system directories are never
    given an owner or mode. Numeric owners are checked to 32 bits.
  - `wait-mount` (a real mount point, by /proc/self/mountinfo; an
    automount point not mounted yet does not count), `wait-path` and
    `wait-tcp`, within `wait-timeout`.
  - `run`: a host command in the project directory.
  - `env`: a host command whose `KEY=VALUE` output becomes environment
    variables of the dependent services. Its output is never shown, not
    even in errors. It fails clearly on a Compose that does not take
    `rawsetenv`.
  - Commands are shown by their first word only (after any `NAME=value`);
    on a timeout, or when Compose dies, their whole process group is
    killed; when Compose stops composehost, the command's shell dies with
    it. Output is kept up to 4 MiB per stream.
- `stop` and `down`: host commands on `docker compose stop` and `down`.
- `composehost check`: shows a project's steps (read through `docker
  compose config`, with Compose's global flags), catches options Compose
  would drop silently, and looks at the waits once. Exit codes 0, 1, 2
  and 3.
- Progress and errors in Compose's own output; whole command output with
  `docker compose --verbose`.
- Works on PATH as `composehost`, or as the Docker CLI plugin
  `docker-composehost`.
- End-to-end tests against real dockerd and Compose in CI.
