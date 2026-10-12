#!/usr/bin/env bash
# Starts local S3 servers for bucketmove's integration tests and writes their
# endpoints and keys to an env file (default: ./test-servers.env).
#
#   scripts/test-servers.sh up [ENV_FILE]    start MinIO, Garage, SeaweedFS, RustFS
#   scripts/test-servers.sh down             stop and remove them
#
# MinIO no longer publishes images, so it runs from a binary: set MINIO_BIN, or
# the script builds the last release with `go install` (about 3 minutes).
set -euo pipefail

PREFIX=bucketmove-test
WORK=${BUCKETMOVE_TEST_DIR:-/tmp/bucketmove-test}
MINIO_VERSION=v0.0.0-20260212201848-7aac2a2c5b7c
GARAGE_IMAGE=dxflrs/garage:v2.1.0
SEAWEED_IMAGE=chrislusf/seaweedfs:latest
RUSTFS_IMAGE=rustfs/rustfs:latest

down() {
	docker rm -f "$PREFIX-garage" "$PREFIX-seaweedfs" "$PREFIX-rustfs" >/dev/null 2>&1 || true
	if [[ -f "$WORK/minio.pid" ]]; then
		kill "$(cat "$WORK/minio.pid")" 2>/dev/null || true
		rm -f "$WORK/minio.pid"
	fi
	rm -rf "$WORK/minio-data"
}

wait_http() { # url
	for _ in $(seq 1 60); do
		if curl -s -o /dev/null "$1"; then return 0; fi
		sleep 1
	done
	echo "timed out waiting for $1" >&2
	return 1
}

up() {
	local env_file=${1:-test-servers.env}
	down
	mkdir -p "$WORK"

	# MinIO (source side)
	local minio_bin=${MINIO_BIN:-$WORK/bin/minio}
	if [[ ! -x "$minio_bin" ]]; then
		echo "building MinIO $MINIO_VERSION (one time, a few minutes)..." >&2
		GOBIN="$WORK/bin" go install "github.com/minio/minio@$MINIO_VERSION"
		minio_bin=$WORK/bin/minio
	fi
	# Four drives: versioning and object lock need erasure coding.
	# MINIO_CI_CD lets them share one disk.
	mkdir -p "$WORK"/minio-data/{1,2,3,4}
	MINIO_CI_CD=1 MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin-secret \
		nohup "$minio_bin" server "$WORK/minio-data/{1...4}" --address 127.0.0.1:19000 --console-address 127.0.0.1:19001 \
		>"$WORK/minio.log" 2>&1 &
	echo $! >"$WORK/minio.pid"

	# Garage
	mkdir -p "$WORK/garage"
	cat >"$WORK/garage/garage.toml" <<TOML
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "0.0.0.0:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
[s3_api]
s3_region = "garage"
api_bind_addr = "0.0.0.0:3900"
root_domain = ".s3.garage.localhost"
[admin]
api_bind_addr = "0.0.0.0:3903"
TOML
	docker run -d --name "$PREFIX-garage" -p 127.0.0.1:13900:3900 \
		-v "$WORK/garage/garage.toml:/etc/garage.toml" "$GARAGE_IMAGE" >/dev/null

	# SeaweedFS
	mkdir -p "$WORK/seaweedfs"
	cat >"$WORK/seaweedfs/s3.json" <<JSON
{"identities":[{"name":"admin","credentials":[{"accessKey":"seaweedadmin","secretKey":"seaweedadmin-secret"}],"actions":["Admin","Read","List","Tagging","Write"]}]}
JSON
	docker run -d --name "$PREFIX-seaweedfs" -p 127.0.0.1:18333:8333 \
		-v "$WORK/seaweedfs/s3.json:/etc/seaweedfs/s3.json" "$SEAWEED_IMAGE" \
		server -s3 -s3.config=/etc/seaweedfs/s3.json -ip.bind=0.0.0.0 >/dev/null

	# RustFS
	docker run -d --name "$PREFIX-rustfs" -p 127.0.0.1:19100:9000 \
		-e RUSTFS_ACCESS_KEY=rustfsadmin -e RUSTFS_SECRET_KEY=rustfsadmin-secret \
		"$RUSTFS_IMAGE" >/dev/null

	for _ in $(seq 1 60); do
		[[ $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19000/minio/health/ready) == 200 ]] && break
		sleep 1
	done

	# Garage needs a layout and a key before S3 works.
	for _ in $(seq 1 30); do
		docker exec "$PREFIX-garage" /garage status >/dev/null 2>&1 && break
		sleep 1
	done
	local node
	node=$(docker exec "$PREFIX-garage" /garage status 2>/dev/null | awk '/^[0-9a-f]{16}/ {print $1; exit}')
	docker exec "$PREFIX-garage" /garage layout assign -z dc1 -c 1G "$node" >/dev/null
	docker exec "$PREFIX-garage" /garage layout apply --version 1 >/dev/null
	local key_out garage_key garage_secret
	key_out=$(docker exec "$PREFIX-garage" /garage key create bucketmove-test)
	garage_key=$(awk '/Key ID/ {print $NF}' <<<"$key_out")
	garage_secret=$(awk '/Secret key/ {print $NF}' <<<"$key_out")
	docker exec "$PREFIX-garage" /garage key allow --create-bucket "$garage_key" >/dev/null

	wait_http http://127.0.0.1:13900
	wait_http http://127.0.0.1:18333
	wait_http http://127.0.0.1:19100

	cat >"$env_file" <<ENV
BUCKETMOVE_TEST_MINIO=http://minioadmin:minioadmin-secret@127.0.0.1:19000
BUCKETMOVE_TEST_GARAGE=http://$garage_key:$garage_secret@127.0.0.1:13900?region=garage
BUCKETMOVE_TEST_SEAWEEDFS=http://seaweedadmin:seaweedadmin-secret@127.0.0.1:18333
BUCKETMOVE_TEST_RUSTFS=http://rustfsadmin:rustfsadmin-secret@127.0.0.1:19100
ENV
	echo "servers are up; endpoints written to $env_file" >&2
}

case "${1:-}" in
up) shift; up "$@" ;;
down) down ;;
*) echo "usage: $0 up [ENV_FILE] | down" >&2; exit 2 ;;
esac
