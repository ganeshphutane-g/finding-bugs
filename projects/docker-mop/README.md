# docker-mop

See exactly what `docker prune` will delete **before** it deletes anything, then
delete exactly that list and nothing else. Also: rename and copy Docker volumes,
with every file checked.

```console
$ docker mop plan system --all --volumes
Would remove 2 containers, 1 network, 1 volume, 3 images. Frees at least 102.8MB.

CONTAINERS (2)
ID            NAME         SIZE     CREATED                 STATE
0bc897aab7e6  test-runner  4.096kB  less than a minute ago  created
241a2ac85636  migrate      8.192kB  less than a minute ago  exited

NETWORKS (1)
ID            NAME          CREATED                 NOTE
653fb6fc7f85  old_test_net  less than a minute ago

VOLUMES (1)
NAME           SIZE  CREATED                 KIND
e32b91961ead…  0B    less than a minute ago  anonymous

IMAGES (3)
ID            TAGS            SIZE     CREATED                 NOTE
6cab8152ce35  worker:latest   1.752kB  less than a minute ago
76b60bf40845  webapp:1.4      96.02MB  less than a minute ago
fd7dc98638c8  busybox:latest  6.809MB  5 months ago

Notes:
  - Build cache is not included. Docker cannot preview it; use 'docker builder prune' for that.

Nothing was removed. To remove exactly these, run: docker mop prune system --all --volumes
```

## Why

- `docker system prune`, `docker image prune -a` and `docker volume prune` have no
  dry run. People have asked for one since 2017
  ([moby/moby#30623](https://github.com/moby/moby/issues/30623)). Deleting the wrong
  volume means losing data.
- Docker cannot rename or copy a volume
  ([moby/moby#31154](https://github.com/moby/moby/issues/31154)). The usual
  workaround is a hand-typed `docker run … cp -a`, which needs an image from Docker
  Hub and never checks that the copy is complete.

## What it does

| Command | What it does |
| --- | --- |
| `docker mop plan [what]` | Lists what the matching `docker … prune` would remove and how much space it would free. Removes nothing. |
| `docker mop prune [what]` | Shows the same list, asks, then removes exactly those objects. Anything that changed in between is skipped and reported. |
| `docker mop volume copy SRC DST` | Copies a volume into a new one and verifies every file. |
| `docker mop volume rename SRC DST` | Copy, verify, then remove the old volume. |

`what` is `system` (the default), `containers`, `images`, `volumes` or `networks`.
The flags are Docker's own: `-a/--all`, `--volumes` (for system), and
`--filter until=24h`, `--filter label=key[=value]`, `--filter label!=key[=value]`.
Add `--format json` for scripts.

### How the plan is kept honest

The plan follows the rules in Docker's own prune code: which container states count
as stopped, which images are dangling, anonymous versus named volumes, the order
system prune runs in (so a stopped container's image and volumes are included), how
label filters combine, and the differences between the classic and containerd image
stores.

The test suite checks this against the real thing. It sets up a mix of objects on a
real Docker daemon, makes a plan, runs Docker's actual prune, and checks that exactly
the planned objects disappeared. That is done for every prune type and filter, on
both image stores.

### What prune promises

- It removes only the objects in the list you confirmed, one by one, never with
  force.
- If something changed after the list was made, it is skipped and reported rather
  than removed. That covers a container that was started, an image that gained a tag
  or came into use, a tag moved to a newer image, a volume that was recreated or
  taken into use, and a network that gained a running container.

Here `webapp:1.4` was rebuilt while the prompt was waiting. The old image it named
was gone, so it was skipped, and the new `webapp:1.4` was left alone:

```console
$ docker mop prune images --all
...
Remove these 4 objects? [y/N] y

Removed 3 images. Freed at least 60.03MB.

Skipped 1 object:
  - image webapp:1.4: the image no longer exists
```

### Volume copy and rename

```console
$ docker mop volume rename pgdata postgres_data
Creating volume postgres_data
Copying pgdata to postgres_data
Verifying postgres_data against pgdata
Removing volume pgdata
Renamed pgdata to postgres_data: 300 files, 90MB, verified in 5.1s.
```

- No image is pulled. A tiny empty helper image is created locally, and the data
  moves through containers that are created but never started.
- Every path is compared between source and copy: type, permissions (including
  setuid and sticky bits), owner, size, SHA-256 of the content, symlink and hard-link
  targets, modification times and file capabilities. Rename deletes the old volume
  only after that check passes. If anything fails, the new volume is removed again
  and the old one is untouched.
- Copy refuses while a running container uses the volume. Rename refuses while any
  container uses it, running or stopped, and names those containers:

  ```console
  $ docker mop volume rename pg postgres_data
  docker mop: volume "pg" is used by container: db (running); remove it (or recreate it with the new volume name) first
  ```
- Labels are copied. Docker Compose labels are dropped unless you pass
  `--compose-project NAME`. That sets the labels so Compose project `NAME` owns the
  new volume, which must then be named `NAME_<volume>`.

## Install

docker-mop is a single binary with no dependencies. Docker looks for CLI plugins in
`~/.docker/cli-plugins/`.

With Go 1.22 or newer:

```sh
git clone https://github.com/ganeshphutane-g/docker-mop
cd docker-mop
make install          # builds and copies docker-mop to ~/.docker/cli-plugins/
docker mop version
```

Or download a release binary for your platform, rename it to `docker-mop`, make it
executable and put it in `~/.docker/cli-plugins/`. Run `make dist` to build all
platforms yourself.

It works with Docker Engine 20.10 or newer (API 1.41+) on Linux and macOS. It
connects the same way the docker CLI does: `DOCKER_HOST`, `DOCKER_CONTEXT`, the
current context, then `/var/run/docker.sock`. Docker's own `--context`, `-H` and
`--tls`/`--tlsverify` flags work too (`docker --context prod mop plan`). `unix://` and `tcp://` (with TLS)
endpoints are supported. `ssh://` and Windows named pipes are not yet.

## Limits

- **Build cache** is not previewed or pruned. Docker has no way to list exactly what
  `docker builder prune` would remove, and a guess would break the promise above.
- **Image sizes are lower bounds.** Images share layers. A shared layer is only
  freed when every image using it goes, so totals say "at least".
- **Swarm networks** are left alone. On a swarm manager, `docker network prune` also
  removes unused swarm networks; docker-mop does not show or remove them.
- **Volume copy** works for `local` volumes without driver options. Volumes with
  options (NFS, bind devices, tmpfs) keep their data outside Docker and are refused.
  POSIX ACLs and extended attributes other than file capabilities are not carried
  by Docker's archive API, so they are not copied.

## Roadmap

- A build-cache preview, if Docker gains a way to list what
  `docker builder prune` would remove.
- Swarm networks in `plan`.
- Volume copy for volumes with driver options, where the data can be reached.
- Release binaries.

## Development

```sh
make test                                   # unit tests
MOP_INTEGRATION=1 go test ./...             # + tests against your Docker daemon
MOP_INTEGRATION=destructive go test ./...   # + unscoped prunes: throwaway daemons only
```

The integration tests build their own tiny images from a static binary, so they
never pull from a registry. With `MOP_INTEGRATION=1` every prune is limited to
objects the test created (by label). `destructive` also runs prunes with no filter,
which can remove anything unused on the daemon.

## License

MIT. See [LICENSE](LICENSE).
