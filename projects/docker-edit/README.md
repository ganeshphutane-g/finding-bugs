# docker-edit

Change an existing Docker container's ports, environment, mounts, restart policy,
labels, image or networks, without retyping its `docker run` command. docker-edit
makes the container again exactly as it was, with only your changes, shows you the
difference first, and puts the old container back if the new one fails.

```console
$ docker edit shop --image nginx:1.29-alpine -p 8443:443 -e API_TOKEN=rotated
shop (nginx:1.27-alpine, running)
  + env ACME_VERSION: 0.3.1
  ~ env API_TOKEN: (hidden) -> (hidden)
  ~ env NGINX_VERSION: 1.27.5 -> 1.29.8
  ~ env NJS_VERSION: 0.8.10 -> 0.9.6
  ~ image: nginx:1.27-alpine -> nginx:1.29-alpine
  + port 443/tcp: 8443
shop will be stopped and made again with these changes; its volumes and their data stay.
Go ahead? [y/N] y
stopping shop
creating the new shop
starting the new shop
Done: shop (2dcabbcaeb99) runs with the changes.
```

The other settings stay as they were: the published port 8080, the named volume
and its data, and the restart policy. The `NGINX_VERSION` lines are the new
image's own defaults, which apply because they were never yours.

## Why

Docker cannot change a container after it is created, apart from a few resource
limits (`docker update`). To add a port or change a variable, you remove the
container and run it again with every option retyped. Miss one (a network alias, a
fixed IP, an anonymous volume, a label) and the service breaks or loses data.

- [moby/moby#21282](https://github.com/moby/moby/issues/21282), "Make docker update
  support port management as well", has been open since 2016 (50 👍).
  [moby/moby#8838](https://github.com/moby/moby/issues/8838), "How to set an
  environment variable on an existing container?", has 104 👍. The Stack Overflow
  question "How do I assign a port mapping to an existing Docker container?" has
  the same answer: recreate it.
- **runlike** and **docker-autocompose** print a `docker run` command or a Compose
  file for you to fix up and run by hand. They do not stop, swap or roll back, and
  they freeze the image's defaults into the command, so a new image keeps the old
  image's `NGINX_VERSION`.
- **Portainer** can duplicate and edit a container, but only in its web UI.

## What it does

```
docker edit [OPTIONS] CONTAINER

  -p, --publish [IP:]HOST:CONTAINER[/PROTO]   --unpublish CONTAINER[/PROTO]
  -e, --env NAME=VALUE                        --unset-env NAME
  -v, --volume SOURCE:/PATH[:OPTIONS]         --unmount /PATH
      --mount type=...,source=...,target=...
      --restart no|always|unless-stopped|on-failure[:N]
  -l, --label NAME=VALUE                      --unlabel NAME
      --image IMAGE
      --network NAME[:ALIAS,...]              --disconnect NAME

      --dry-run   show the changes and stop      -y, --yes   do not ask
      --keep-old  keep the old container, stopped and renamed, afterwards
      --wait 30s  how long the new container must run, or become healthy
      --discard-changes   go ahead although files changed inside the container
```

Each change can be given several times. `docker edit --help` explains every option.

### The plan

Before anything changes, docker-edit lists every setting that will differ: `+`
added, `-` removed, `~` changed. The list compares the container as it is with
the container as it will be, including what the (new) image supplies, so nothing
changes that is not on the list. `--dry-run` stops after the plan. Values of
variables and labels whose names look secret (containing `PASS`, `TOKEN`, `KEY`,
`SECRET`, `AUTH`, `CREDENTIAL`, `COOKIE`, `SESSION` or `PRIVATE`) are shown as
`(hidden)`, and so are passwords in URLs (`postgres://app:(hidden)@db/app`). A
secret in another setting, such as the command line, is shown as it is.

### Exact recreation

The new container is created from the old one's full description, not from a
fixed list of options, so settings added in newer Docker versions carry over too.
The integration tests create containers of many shapes, recreate them with no
change and compare `docker inspect` field by field:
- the command, entrypoint, user, working directory, healthcheck and init;
- published ports, binds, named volumes, tmpfs mounts and devices;
- anonymous volumes, which are reattached so their data stays;
- capabilities, security options, sysctls, ulimits, limits and log settings;
- every network with its aliases, fixed IPs and MAC address (on the first
  network only one you set; see Limits);
- DNS settings, extra hosts, legacy links and labels;
- `host` and `none` networking.

The container's ID, creation time, an automatic hostname and an automatic MAC
address are new, as they would be with `docker run`.

**Image defaults.** Variables, labels, the command and other settings the
container only has because its image set them are not frozen in, so `--image`
picks up the new image's defaults. Your own settings stay. (A setting equal to
the old image's default cannot be told apart from it, so it follows the new
image.) If Docker has already removed the old image, its defaults are unknown:
variables and labels the new image sets are then taken from the new image, and
the plan says which.

**A moved tag.** If `nginx:1.27` now names a newer image than the one the
container runs (after a `docker pull`), an edit keeps the image the container
runs and says so. To move to the newer image, use `--image nginx:1.27`; the plan
shows both image IDs.

### The swap

1. Stop the old container and rename it `NAME.old-<time>`.
2. Create the new one under the original name and start it (only if the old one
   was running).
3. Wait until it has run for 3 seconds, or until it is healthy if it has a
   healthcheck (up to `--wait`).
4. Remove the old container, or keep it with `--keep-old` (its restart policy is
   then set to `no`, so it never starts by itself and competes with the new one).

If any step fails, or the new container exits or turns unhealthy, docker-edit
removes the new container, gives the old one back its name and starts it again,
and prints the end of the new container's log:

```console
$ docker edit api --image api:2 -y
api (api:1, running)
  ~ config Cmd: ["sleep","3600"] -> ["sh","-c","echo config file missing: /etc/api.yaml; exit 3"]
  ~ image: api:1 -> api:2
api will be stopped and made again with these changes; its volumes and their data stay.
stopping api
creating the new api
starting the new api
put the old container back
docker edit: the new container stopped (exit code 3); its last output:
  config file missing: /etc/api.yaml
```

Ctrl-C during the swap does the same. A change to the restart policy alone needs
no recreation: it is made in place with `docker update`.

### What it refuses

docker-edit stops with the reason, and changes nothing, when recreating would lose
something:
- **Files changed inside the container** but outside its volumes (what `docker
  diff` shows, apart from `/tmp`, `/run` and caches). They would go with the old
  container. Move them into a volume, or pass `--discard-changes`.
- **Containers started with `--rm`**: stopping one deletes it, so there would be
  nothing to roll back to.
- **Paused containers**: unpause first.
- **`--unmount` where the image declares a `VOLUME`**: Docker would mount a new,
  empty volume there. Mount something else in its place instead.
- **Docker Compose containers**: edit the Compose file and run `docker compose
  up` instead.
- **Containers others depend on**: those that share its network (`--network
  container:NAME`) would lose it, and legacy `--link`s to it would follow the old
  container when it is renamed and break when it is removed.

## Install

docker-edit is a single binary with no dependencies, and works as a Docker CLI
plugin (`docker edit`) or on its own (`docker-edit`).

With Go 1.24 or newer:

```sh
git clone https://github.com/ganeshphutane-g/finding-bugs
cd finding-bugs/projects/docker-edit
make install          # builds and copies docker-edit to ~/.docker/cli-plugins/
docker edit version
```

It works with Docker Engine 20.10 or newer (API 1.41+) on Linux and macOS. It
connects the same way the docker CLI does: `DOCKER_HOST`, `DOCKER_CONTEXT`, the
current context, then `/var/run/docker.sock`. Docker's own `--context`, `-H` and
`--tls`/`--tlsverify` flags work too (`docker --context prod edit web -p 80:80`).
For client certificates use `DOCKER_CERT_PATH` or a context; `--tlscacert`,
`--tlscert` and `--tlskey` are not supported.

## Examples

```sh
docker edit web -p 8080:80                       # publish another port
docker edit web --unpublish 80 -p 127.0.0.1:8080:80     # only on localhost
docker edit api -e LOG_LEVEL=debug --unset-env DEBUG_SQL
docker edit db --restart unless-stopped          # in place, no restart
docker edit app -v /srv/uploads:/app/uploads:ro
docker edit app --image myapp:2.4.1 --dry-run    # what would change?
docker edit app --network backend:api,api-v2 --disconnect legacy
docker edit app --image myapp:2.5 --keep-old -y  # keep app.old-<time> to compare
```

Exit codes: 0 done (or nothing to change), 1 error or refused, 2 bad usage, 130
interrupted (the old container is put back).

## Limits

- **Downtime.** The container is stopped before the new one starts, because both
  usually need the same ports and name. Expect a few seconds, or the container's
  stop timeout.
- **Not for Compose or Swarm.** Compose containers are refused; Swarm services
  have `docker service update`.
- **MAC addresses.** Since API 1.44 Docker no longer reports whether a MAC
  address was set or made up. docker-edit asks as API 1.43 for the first
  network's, so an automatic one there stays automatic; a future Docker that
  drops API 1.43 makes docker-edit stop with an error rather than lose a set one.
  On further networks the current MAC address is always kept.
- **Changing what `docker update` changes** (memory, CPU) is left to
  `docker update`.

## Roadmap

- `--cpus`, `--memory` and friends passed to `docker update` in place.
- `--hostname`, `--user`, `--entrypoint`, `--cmd` and other settings.
- Podman, whose API is mostly compatible.
- A JSON plan (`--json`) for scripts.

## Development

```sh
make test          # vet and unit tests
make test-docker   # + integration tests against your Docker daemon
```

The integration tests create and recreate containers named `de-...` and build
their own small images from `alpine:3` (and pull `nginx:alpine` once). CI runs them
on GitHub's runners.

## License

MIT. See [LICENSE](LICENSE).
