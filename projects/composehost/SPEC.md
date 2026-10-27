# composehost: spec

Host-side steps for Docker Compose projects, declared in the compose file:
- create bind-mount directories with the right owner;
- wait for a NAS mount or a host port before the containers that need it
  start;
- run a host command on `up` or `down`;
- hand values computed on the host to the containers.

It is built on Compose's own provider services, so plain `docker compose
up` runs the steps, with no wrapper script, Makefile or alias.

```yaml
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: [./data, ./config]
        owner: "1000:1000"
        wait-mount: /mnt/nas
        run: ./scripts/prepare.sh
  app:
    image: example/app
    depends_on: [host]
    volumes: [./data:/data, /mnt/nas/media:/media]
```

## Why

Compose has no host-side hooks.
- **Compose's own hooks run inside containers.** `post_start` and
  `pre_stop` are not on the host.
- **Missing directories become root's.** A bind-mount directory that is
  missing is created by the Docker daemon, owned by root, so the service
  then fails on permissions.
- **No wait for mounts.** A container that starts before the NAS is
  mounted writes into the empty mount point.
- **Wrappers are bypassed.** People wrap `up` in scripts that Dockge,
  Portainer, CI and their own muscle memory then skip.

**Demand** (checked 2026-10-24/27):
- docker/compose#6736 "Hooks to run scripts on host before starting any
  containers": 225 👍, 31 ❤️, 2019–2023. It was closed as "completed", but the
  team declined host hooks; that closing comment has 163 👎 (seen by the
  researcher).
- #1341 "Hooks/Plugins support to run arbitrary scripts": 90 👍, closed
  2017.
- #468 "Add pre/post command option": 35 👍 (seen by the researcher).
- #7616 "Wait or retry starting a container waiting for NFS volume to be
  mounted": 13 👍.
- #2781, bind-mount directories created as root: 11 👍 (seen by the
  researcher).
- Docker forum threads (web).

**Existing tools:** the existing provider plugins do other jobs:
docker-vals (38★) handles secrets, and the rest have 0★. Wrappers such as
compose-companion replace `docker compose up`. No host-hook provider was
found.

## Verified in the lab (2026-10-27, Docker 29.8.2, Compose v5.6.0)

**Calls:**
- Compose runs `composehost compose metadata`, then `composehost compose
  --project-name=P up --opt=v ... SERVICE`.
- **Correction (2026-10-28, found in review):** it runs them in its
  caller's working directory, not the project's, and passes no project
  directory (only `COMPOSE_PROJECT_NAME`). The first lab runs were all made
  from the project directory, which hid this. composehost reads the parent
  Compose process's arguments and environment from /proc and applies
  Compose's rules (`--project-directory`, the first `-f` or `COMPOSE_FILE`,
  else the nearest compose file up from the working directory); if it
  cannot tell, it fails.
- Compose sets `COMPOSE_PROVIDER_MESSAGES` (5.6.0:
  `error,info,setenv,rawsetenv,debug,...`). Compose v2.36.1's source knows
  only error, info, setenv and debug, and has no `stop` (read by the
  reviewer, not tested).
- Dependents start after `up` exits 0. A non-zero exit, with an `error` JSON
  line, fails `up` and shows the message.
- `up` is called on every `docker compose up`.
- `stop` is called only if metadata declares a `stop` block. `restart`
  calls nothing.
- `down` and `stop` reach the provider only when Compose reads the compose
  file: with `-p NAME` and no `-f`, they work from the containers alone and
  skip providers, even in the project directory. `-p NAME -f FILE`,
  `COMPOSE_PROJECT_NAME` and `name:` in the file all work (checked
  2026-10-28).
- With `env`, every `up` recreates the dependent containers (the injected
  values count as a change); without it, running dependents are left alone.

**Messages and options:**
- A provider that prints nothing is accepted.
- `{"type":"rawsetenv","message":"K=V"}` reaches the dependent container's
  environment.
- Options become flags, and lists become repeated flags. Once metadata
  lists parameters, only those are passed.
- The context (`--context`) is not passed to the provider; DOCKER_HOST is
  passed only if set.

## What it does

The options of one provider service, run in this order on `up`:

| Option | |
|---|---|
| `mkdir: PATH` (repeat) | Create the directory (and parents) if missing. Relative paths are from the project directory. |
| `owner: UID:GID` | Owner of the `mkdir` directories, set on every `up` when different (needs root). |
| `mode: "0750"` | Their mode, likewise (quoted: YAML reads 0750 as 488). |
| `wait-mount: PATH` (repeat) | Until PATH is a mount point (not the empty directory beneath). |
| `wait-path: PATH` (repeat) | Until PATH exists. |
| `wait-tcp: HOST:PORT` (repeat) | Until a TCP connection succeeds. |
| `wait-timeout: 60s` | How long the waits may take in all; then `up` fails. |
| `run: COMMAND` | A host command (`sh -c`, in the project directory); `up` fails if it fails. |
| `env: COMMAND` | A host command whose `KEY=VALUE` output lines become environment variables of the dependent services (`rawsetenv`). |
| `down: COMMAND` | A host command on `docker compose down`. |
| `stop: COMMAND` | A host command on `docker compose stop`. |
| `timeout: 5m` | The longest `run`, `env`, `down` or `stop` may take. |

**Behaviour:**
- Progress is shown in Compose's own progress UI (`info` messages), and
  failures as the service's error.
- Every step is idempotent (Compose calls `up` again on every `up`), except
  what a user's own `run` command does.
- Output of commands: the last lines of stderr go into the error; with
  `--verbose`, all of it goes into `debug` messages.

Also:
- `composehost check [-f compose.yaml]`: shows the steps the project
  declares and runs the waits once, without changing anything.
- `composehost compose metadata`: declares every option, and `stop`.

## Non-goals (v1)

- **`upload`** (copying local files into a volume on a remote engine,
  compose#11867): the provider cannot see the CLI context. Phase 2.
- **Hooks when dockerd restarts containers at boot:** Compose is not
  involved then.
- **Running inside UIs that bundle their own Compose** (Dockge's or
  Portainer's container): the provider must be on the PATH where Compose
  runs, and acts there. Documented, not solved.

## Tech

- Go, one static binary, no dependencies beyond the standard library and
  x/sys (openat-based mkdir).
- Installed on PATH as `composehost`, or as a Docker CLI plugin
  (`docker-composehost`, which Compose also finds).

## Tests

- **Unit:** flag parsing; every action; the waits (a mount point
  against a plain directory; TCP; a timeout); env parsing; the JSON
  messages.
- **End to end with real dockerd and Compose:**
  - `up` creates directories with the owner and mode before the container
    starts (the container lists them);
  - a wait holds the dependent until a mount appears (a tmpfs mounted
    during the wait), and a timeout fails `up` with a clear message;
  - `env` reaches the container;
  - `run` fails `up` and the message is shown;
  - `down` and `stop` run their commands;
  - a second `up` is idempotent;
  - `-p NAME stop` and `down` without `-f` are the documented limit.

## Milestones

1. The options, actions and protocol (metadata, up/down/stop, JSON lines),
   with unit tests.
2. `composehost check`, the binary, end-to-end tests with dockerd and
   Compose; CI.
3. README with real runs, CHANGELOG, LICENSE; two full reviews.
