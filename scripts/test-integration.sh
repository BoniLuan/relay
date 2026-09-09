#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export RELAY_TEST_UID="$(id -u)" RELAY_TEST_GID="$(id -g)"
cleanup() { docker compose -f compose.test.yaml down; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose -f compose.test.yaml up --abort-on-container-exit --exit-code-from relay-tests
# Check the actual server log, not only an in-memory application logger. Only
# synthetic data is used in this dedicated tmpfs database.
review_logs=$(docker compose -f compose.test.yaml logs --no-color relay-test-db)
case "$review_logs" in
  *RELAY_LOG_SENTINEL*) echo 'FAIL: PostgreSQL logged a synthetic payload marker' >&2; exit 1 ;;
esac
case "$review_logs" in
  *'injected commit failure'*) echo 'PostgreSQL log privacy: passed (operational error retained)' ;;
  *) echo 'FAIL: expected operational error was absent from PostgreSQL logs' >&2; exit 1 ;;
esac
