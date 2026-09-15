#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export RELAY_RUN_UID="$(id -u)" RELAY_RUN_GID="$(id -g)"
mkdir -p .local
exec 9>.local/restore-drill.lock
flock -n 9 || { echo "Another restore drill is running." >&2; exit 1; }
# Pin the project independently of caller COMPOSE_PROJECT_NAME and .env.
compose() { docker compose --env-file /dev/null -p relay-restore-drill -f compose.restore.yaml "$@"; }
docker info >/dev/null
# Refuse to adopt or remove resources from an earlier/concurrent drill.
existing=$(docker ps -aq --filter label=com.docker.compose.project=relay-restore-drill)
if [ -n "$existing" ]; then
    echo "Restore drill containers already exist; inspect them before retrying." >&2
    exit 1
fi
if docker network inspect relay-restore-drill_default >/dev/null 2>&1; then
    echo 'Restore drill network already exists; inspect it before retrying.' >&2
    exit 1
fi
mkdir -p .local/go-cache-test .local/go-mod
# Compilation may download Go modules. The actual drill has no external network.
docker run --rm --memory 768m --cpus 1 --user "$RELAY_RUN_UID:$RELAY_RUN_GID" \
  -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  -e GOCACHE=/src/.local/go-cache-test -e GOMODCACHE=/src/.local/go-mod \
  golang:1.27-bookworm go test -c -o .local/restore-drill.test ./internal/delivery
cleanup() { compose down; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
compose up --abort-on-container-exit --exit-code-from verify --attach verify
