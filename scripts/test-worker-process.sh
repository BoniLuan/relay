#!/bin/sh
# Real process failure verification against Relay's disposable test database only.
set -eu
cd "$(dirname "$0")/.."
if [ -n "$(docker compose -f compose.test.yaml ps -aq)" ]; then
 echo 'Run this test separately: relay-test is already in use.' >&2; exit 1
fi
lease_test_image=${RELAY_TEST_IMAGE:-relay:lease-check}
lease_test_url='postgres://relay_test:relay_test_ephemeral@relay-test-db:5432/relay_test?sslmode=disable'
lease_test_dir=$(mktemp -d /tmp/relay-lease-process.XXXXXX)
lease_test_containers=''
cleanup() {
 for lease_test_id in $lease_test_containers; do docker rm -f "$lease_test_id" >/dev/null; done
 docker compose -f compose.test.yaml down
 rm -f "$lease_test_dir/blocked.log" "$lease_test_dir/reclaimed.log"
 rmdir "$lease_test_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
sql() { docker compose -f compose.test.yaml exec -T relay-test-db psql -X -At -v ON_ERROR_STOP=1 -U relay_test -d relay_test -c "$1"; }
wait_for() {
 lease_test_tries=0
 while [ "$lease_test_tries" -lt 150 ]; do
  if [ "$(sql "$1")" = t ]; then return 0; fi
  lease_test_tries=$((lease_test_tries+1));sleep 0.1
 done
 echo 'Timed out waiting for database lease state' >&2;return 1
}
start_worker() {
 docker run -d --network relay-test_default --read-only --cap-drop ALL --security-opt no-new-privileges:true --memory 128m --cpus 0.5 \
  -e RELAY_DATABASE_URL="$lease_test_url" "$lease_test_image" worker --lease-duration 10s
}
docker compose -f compose.test.yaml up -d --wait relay-test-db
docker run --rm --network relay-test_default -e RELAY_DATABASE_URL="$lease_test_url" "$lease_test_image" migrate
# Synthetic fixtures, never production data. No valid bearer/signing key is needed.
sql "INSERT INTO clients(id,name,token_hash) VALUES('00000000-0000-4000-8000-000000000001','lease process test',decode(repeat('11',32),'hex'));
 INSERT INTO destinations(id,client_id,url) VALUES('00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001','https://example.com');
 INSERT INTO events(id,client_id,destination_id,idempotency_key,request_hash,payload) VALUES('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002','process-test',decode(repeat('22',32),'hex'),'{}');
 INSERT INTO deliveries(event_id) VALUES('00000000-0000-4000-8000-000000000003');" >/dev/null
lease_test_first=$(start_worker);lease_test_containers="$lease_test_first"
wait_for "SELECT status='leased' AND lease_expires_at>clock_timestamp() FROM deliveries"
lease_test_old_token=$(sql 'SELECT lease_token FROM deliveries')
docker run --rm --network relay-test_default -e RELAY_DATABASE_URL="$lease_test_url" "$lease_test_image" worker > "$lease_test_dir/blocked.log"
case "$(cat "$lease_test_dir/blocked.log")" in *'no delivery available'*) ;; *) echo 'Second process acquired a live lease' >&2;exit 1;; esac
# SIGKILL cannot run application cleanup. The committed claim must remain.
docker kill --signal KILL "$lease_test_first" >/dev/null
[ "$(docker wait "$lease_test_first")" = 137 ]
[ "$(sql "SELECT status='leased' AND lease_token='$lease_test_old_token' FROM deliveries")" = t ]
wait_for "SELECT lease_expires_at<=clock_timestamp() FROM deliveries"
docker run --rm --network relay-test_default -e RELAY_DATABASE_URL="$lease_test_url" "$lease_test_image" worker --lease-duration 1s > "$lease_test_dir/reclaimed.log"
case "$(cat "$lease_test_dir/reclaimed.log")" in *'delivery lease acquired'*) ;; *) echo 'Expired claim was not recovered' >&2;exit 1;; esac
[ "$(sql "SELECT lease_token<>'$lease_test_old_token' AND status='leased' FROM deliveries")" = t ]
wait_for "SELECT lease_expires_at<=clock_timestamp() FROM deliveries"
# A new process receiving SIGTERM should release before its deadline.
lease_test_graceful=$(start_worker);lease_test_containers="$lease_test_containers $lease_test_graceful"
wait_for "SELECT status='leased' AND lease_expires_at>clock_timestamp() FROM deliveries"
docker stop --timeout 5 "$lease_test_graceful" >/dev/null
[ "$(docker wait "$lease_test_graceful")" = 0 ]
[ "$(sql "SELECT status='pending' AND lease_token IS NULL AND lease_owner IS NULL AND lease_expires_at IS NULL FROM deliveries")" = t ]
[ "$(sql 'SELECT count(*)=1 FROM deliveries')" = t ]
echo 'Worker processes: live-claim exclusion, SIGKILL expiry recovery and SIGTERM release passed'
