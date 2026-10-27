# composehost

Host-side steps for Docker Compose projects, declared in the compose file:
- create bind-mount directories with the right owner and mode;
- wait for a NAS mount, a socket or a port before the containers that need
  it start;
- run a host command on `up`, `stop` or `down`;
- hand values computed on the host to the containers.

There is no wrapper to remember. composehost is a Compose *provider
service*, so a plain `docker compose up` (from a terminal, a script, CI or a
UI that runs the Docker CLI) runs the steps first, and fails with their
message if one fails.

```yaml
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: [./data, ./config]
        owner: "1000:1000"
        mode: "0750"
        wait-mount: ./nas
        run: ./prepare.sh
        env: "echo BUILD_ID=$$(git rev-parse --short HEAD 2>/dev/null || echo none)"
        down: "rm -f ./data/ready"
  app:
    image: busybox:latest
    command: ["sh", "-c", "ls -ln / | grep -E ' (data|config)$$'; ls /media; echo BUILD_ID=$$BUILD_ID"]
    user: "1000:1000"
    volumes: [./data:/data, ./config:/config, ./nas:/media]
    depends_on: [host]
```

## Why

Compose has no host-side hooks. Its `post_start` and `pre_stop` hooks run
inside containers. So:
- **Missing bind-mount directories become root's.** The Docker daemon
  creates a missing directory, owned by root, and a service running as an
  ordinary user then fails on permissions.
- **Nothing waits for mounts.** A container that starts before the NAS is
  mounted writes into the empty directory beneath the mount point, and its
  data then hides under the mount.
- **Wrappers get bypassed.** People wrap `docker compose up` in scripts and
  Makefiles that UIs, CI and muscle memory then skip.

People have asked for host hooks for years:
- [docker/compose#6736](https://github.com/docker/compose/issues/6736),
  "Hooks to run scripts on host before starting any containers": 225 👍.
  The Compose team declined host hooks in it;
- [#1341](https://github.com/docker/compose/issues/1341), "Hooks/Plugins
  support to run arbitrary scripts": 90 👍;
- [#468](https://github.com/docker/compose/issues/468), pre/post commands;
- [#7616](https://github.com/docker/compose/issues/7616), waiting for an NFS
  volume to be mounted;
- [#2781](https://github.com/docker/compose/issues/2781), bind-mount
  directories created as root.

Compose 2.36 added provider services: a service whose lifecycle is handed to
a program on the host. Compose calls it on `up` before the services that
depend on it, and on `stop` and `down`. composehost is such a program, for
the host steps above.

## Install

Linux, with Docker Compose v5.6.0 (the version tested). Compose has had
provider services since 2.36, but its early versions take neither the
`rawsetenv` messages `env` needs nor `stop`. composehost says so when Compose
announces the message types it takes. With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/composehost
make build                     # ./composehost, a static binary
sudo make install              # copies it to /usr/local/bin
```

Compose looks for the provider on the PATH of the `docker compose` command.
It also finds the binary as a Docker CLI plugin:

```sh
mkdir -p ~/.docker/cli-plugins
install -m 755 composehost ~/.docker/cli-plugins/docker-composehost
docker composehost check
```

The steps run as the user running `docker compose`, on the machine where it
runs. `owner` needs root, unless that user owns the directories already.

## Quickstart

Add a provider service with `type: composehost`, and make the services that
need its steps depend on it. Then look at what it will do, without changing
anything:

```
$ composehost check
host:
  before:    app
  on up:     mkdir ./data ./config (owner 1000:1000, mode 0750)
  on up:     wait until ./nas mounted (at most 1m0s)
  on up:     run ./prepare.sh
  on up:     env from echo BUILD_ID=$(git rev-parse --short HEAD 2>/dev/null || echo none)
  now:       up would wait for ./nas to be mounted
  on down:   rm -f ./data/ready
```

Then `docker compose up`. Below, the NAS was mounted 4 seconds after the
start. The `host` lines are composehost's (shown with `--progress plain`;
the network lines are left out):

```
$ docker compose --progress plain up
 host Creating
 host created ./data
 host ./data owned by 1000:1000
 host created ./config
 host ./config owned by 1000:1000
 host waiting for ./nas to be mounted
 host run: ./prepare.sh
 host env: echo …
 host env: 1 value for the dependent services
 host Created
 Container demo-app-1 Creating
 Container demo-app-1 Created
Attaching to app-1
 Container demo-app-1 Starting
 Container demo-app-1 Started
app-1  | drwxr-x---    2 1000     1000          4096 Oct 27 19:51 config
app-1  | drwxr-x---    2 1000     1000          4096 Oct 27 19:51 data
app-1  | movie.mkv
app-1  | BUILD_ID=none
```

The container's directories had the right owner and mode, `/media` showed
the mounted NAS, and the value computed on the host reached its
environment. (The demo directory was not a git repository, hence `none`.)
On `docker compose down`:

```
 host Removing
 host down: rm …
 host Removed
```

When a step fails, `up` fails with the step's message, and the dependent
services do not start. Here the waits were `wait-path: ./never` and
`wait-tcp: 127.0.0.1:1`, with `wait-timeout: 2s` (output from the
end-to-end tests, network lines left out):

```
$ docker compose --progress plain up -d
 host Creating
 host waiting for ./never to exist
 host waiting for 127.0.0.1:1 to accept connections
 host gave up after 2s waiting for ./never to exist, 127.0.0.1:1 to accept connections
 provider:host gave up after 2s waiting for ./never to exist, 127.0.0.1:1 to accept connections
 Container demo-app-1 Creating
gave up after 2s waiting for ./never to exist, 127.0.0.1:1 to accept connections
$ echo $?
1
```

## Options

On `up` the steps run in this order: `mkdir`, the waits, `run`, `env`.

| Option | |
|---|---|
| `mkdir: PATH` (or a list) | Create the directory, with its parents, if missing. Relative paths are from the project directory. Parents get mode 0755. A path that is also a `wait-mount` is only created: its owner and mode would be the mounted filesystem's. A path inside a `wait-mount` is made after the waits, on the mounted filesystem. |
| `owner: UID:GID` | The owner of the `mkdir` directories, set on every `up` when it differs. Also `USER:GROUP`, `USER` (its primary group) or `UID`. Names are looked up in /etc/passwd and /etc/group only (the binary is static): give numbers for LDAP or SSSD users. Never applied to `/` or system directories such as `/etc` or `/var`. |
| `mode: "0750"` | Their mode, set on every `up` when it differs (up to 0777). Quote it: YAML reads an unquoted 0750 as the number 488. |
| `wait-mount: PATH` (or a list) | Wait until PATH is a mount point, not just the empty directory beneath (nor an automount point not mounted yet). |
| `wait-path: PATH` (or a list) | Wait until PATH exists. |
| `wait-tcp: HOST:PORT` (or a list) | Wait until a TCP connection to it succeeds. |
| `wait-timeout: 60s` | How long the waits may take in all; then `up` fails. |
| `run: COMMAND` | A host command (`sh -c`, in the project directory). `up` fails if it fails, with the last lines of its error output. |
| `env: COMMAND` | A host command whose `KEY=VALUE` output lines become environment variables of the dependent services. `export`, blank lines and `#` comment lines are allowed, and matching quotes around a whole value are removed (no escapes, no comments after a value). |
| `stop: COMMAND` | A host command on `docker compose stop`. |
| `down: COMMAND` | A host command on `docker compose down`. |
| `timeout: 5m` | The longest `run`, `env`, `stop` or `down` may take. |

**How the commands run:**
- In the project directory, with `sh -c`. They get `COMPOSE_PROJECT_NAME`,
  `COMPOSEHOST_PROJECT_DIR`, and `COMPOSEHOST_SERVICE` (the provider
  service's name).
- Only a command's first word is shown (`run: curl …`), after any
  `NAME=value` words: Compose has already put variables' values into it, so
  the rest may hold secrets.
- `docker compose --verbose` shows their whole output once they exit (up to
  4 MiB of each stream), except the standard output of `env`: that is the
  values, often secrets, so it is never shown, not even when a line of it is
  malformed.
- A process they start in the background (`./helper &`) keeps running.
  Redirect its output: once the command exits, nobody reads it.
- On a `timeout`, the command and everything it started are killed; also
  when Compose dies (`kill -9`). When Compose is stopped with SIGTERM or
  Ctrl-C, it kills composehost at once, and the command's shell dies with
  it; a process the command put in the background may then finish.
- Compose interpolates `$VAR` in the compose file from *its* environment
  when it reads the file. Write `$$VAR` for a variable the command should
  expand itself, as in `$$(git ...)` above.

**Repeated `up`s:** Compose calls `up` again on every `docker compose up`, so
every step must be safe to repeat. `mkdir`, `owner`, `mode` and the waits
are: they change only what is not already right. Your `run` command must be
too.

## Commands

```
composehost check [-f FILE]...   show the project's steps; look at the waits once
composehost compose ...          what Compose calls (metadata, up, stop, down)
composehost version
```

`check` takes Compose's own global flags (`-f`, `-p`, `--project-directory`,
`--env-file`, `--profile`) and reads the project through `docker compose
config` with them, so it sees what Compose sees. It also catches a
misspelled option (`mdoe:`). Compose passes only the options composehost
declares, so it would drop such an option without a word. Unlike the
provider, it shows the commands in full. Exit codes:
- 0: everything is fine;
- 1: an error, such as a bad option or value;
- 2: wrong usage;
- 3: a wait is not met yet.

## How it works

Compose runs `composehost compose metadata` to learn the options. Then, on
`up`, it runs `composehost compose --project-name=P up --mkdir=./data ...
host`, before the dependent services.

Compose runs it in its caller's working directory, which need not be the
project's (`docker compose -f /srv/app/compose.yaml up` from anywhere), and
does not say which directory the project is in. So composehost reads the
arguments, environment and directory of the Compose process that started it
(from /proc), skipping the Docker CLI's flags (`docker --context X
compose ...`), from its logical working directory (`$PWD`), and applies
Compose's own rules:
1. `--project-directory`;
2. else the first compose file's directory (`-f`, else `COMPOSE_FILE`
   from the environment or the env file: `--env-file`, `COMPOSE_ENV_FILES`,
   or `.env` unless `COMPOSE_DISABLE_ENV_FILE`);
3. else the nearest directory, from the working directory up, with a
   compose file.

If it cannot tell (an unknown flag, say), it fails rather than guess.

**The progress messages:**
- composehost reports progress and errors as JSON lines on its standard
  output, which Compose shows in its own progress output.
- The `env` values go back as `rawsetenv` messages, which Compose adds to
  the dependent services' environments.
- A non-zero exit fails `up`.

`stop` and `down` are called the same way.

**Directories, safely as root.** composehost often runs as root, on
directories that containers, as other users, can change. So `mkdir` walks
each path one directory at a time (openat with O_NOFOLLOW), and sets the
owner and mode on the opened directory. A symbolic link is followed only
where the directory holding it belongs to root, or to the user composehost
runs as, and no one else can write to it. Elsewhere it is refused, so a
container user cannot swap in a link to make root chown or chmod something
else.

## Limits

These are from Compose's provider design, and were checked with Compose
5.6.0:
- **`-p NAME` without `-f` skips the provider on `stop` and `down`.** Compose
  then works from the containers alone, even in the project directory.
  `-p NAME -f compose.yaml`, `COMPOSE_PROJECT_NAME`, or `name:` in the file
  all work.
- **With `env`, every `up` recreates the dependent containers.** Compose
  counts the values as a change. Without `env`, running dependents are left
  alone.
- **`restart` calls nothing,** and neither do the restarts dockerd does at
  boot (Compose is not involved then).
- **The steps run where `docker compose` runs.** With a remote engine
  (`DOCKER_HOST`, contexts), composehost acts on the local machine. A UI
  that bundles its own Compose inside a container (Dockge, Portainer) needs
  composehost in that container, and then acts there.
- **`owner` needs root**, unless you already own the directories.
- **Linux only** for the provider: finding the project directory needs
  /proc.

## Testing

`make test` runs the unit tests.

`make test` covers links in directories others can change, and creating
directories under a strict umask. Run as root (as CI does), it also covers
a link in a directory a container user owns, and real owners.

`make test-compose` (root, with dockerd running) runs the end-to-end tests
against real dockerd and Compose. They check:
- `up` creates the directories with their owner and mode, runs `run`, and
  passes `env` values. The container sees all of them, and a parent
  directory keeps 0755;
- `--verbose` does not show `env` values;
- the steps happen in the project directory when Compose is run from
  elsewhere with `-f`, from a subdirectory, with `--project-directory`,
  with `COMPOSE_FILE`, or as `docker --context default compose`;
- when Compose is stopped (SIGTERM) or killed (SIGKILL) during a command,
  the command does not run on;
- a second `up` changes nothing, and leaves the running dependent alone; a
  wrong mode or owner is repaired;
- `wait-mount` holds the dependent until a tmpfs is mounted, the container
  sees the mounted filesystem, and a directory inside it is made on the
  mount, with its owner;
- the waits time out with a clear message, before the dependent is created;
- a failing `run` fails `up` with its error output;
- `stop` and `down` run their commands, and `-p NAME` without `-f` does not
  (the limit above; the test fails if Compose changes it);
- `check` finds the project from a subdirectory, catches a misspelled
  option, and reports unmet and met waits;
- installed only as the Docker CLI plugin `docker-composehost`, Compose
  finds and runs it, and `docker composehost` works.

CI runs them on every change, with Compose v5.6.0.

## Roadmap

- `upload`: copy local files into a volume on a remote engine
  ([compose#11867](https://github.com/docker/compose/issues/11867)). It needs
  the CLI's context, which Compose does not pass to providers.
- `wait-http`: wait for a URL to answer.
- `template`: render a config file from environment values before `up`.

## License

MIT, see [LICENSE](LICENSE).
