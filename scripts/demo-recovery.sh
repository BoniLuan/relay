#!/bin/sh
# Only the dedicated tmpfs relay-test database is paused or modified.
set -eu
cd "$(dirname "$0")/.."
if [ -n "$(docker compose -f compose.test.yaml ps -aq)" ]; then
 echo 'relay-test is in use; run the demo separately.' >&2;exit 1
fi
demo_dir=$(mktemp -d /tmp/relay-observability.XXXXXX)
demo_containers=''
demo_db=''
demo_url='postgres://relay_test:relay_test_ephemeral@relay-test-db:5432/relay_test?sslmode=disable'
demo_image=${RELAY_DEMO_IMAGE:-relay:observability-demo}
cleanup() {
 if [ -n "$demo_db" ];then docker unpause "$demo_db" >/dev/null 2>&1 || true;fi
 for id in $demo_containers;do docker rm -f "$id" >/dev/null;done
 docker compose -f compose.test.yaml down
 rm -f "$demo_dir/alerts.yml" "$demo_dir/prometheus.yml"
 rmdir "$demo_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Same alert expressions; accelerated holds and scraping ONLY in this demo.
python3 - "$demo_dir" <<'PY'
import sys,yaml
from pathlib import Path
out=Path(sys.argv[1]);rules=yaml.safe_load(Path('deploy/monitoring/alerts.yml').read_text())
for group in rules['groups']:
 for rule in group['rules']: rule['for']='5s'
(out/'alerts.yml').write_text(yaml.safe_dump(rules))
(out/'prometheus.yml').write_text(yaml.safe_dump({'global':{'scrape_interval':'1s','evaluation_interval':'1s'},'rule_files':['/demo/alerts.yml'],'scrape_configs':[{'job_name':'relay-metrics','scrape_timeout':'900ms','static_configs':[{'targets':['relay-demo-metrics:9091']}]}]}))
PY
chmod 755 "$demo_dir"
chmod 644 "$demo_dir/alerts.yml" "$demo_dir/prometheus.yml"
docker compose -f compose.test.yaml up -d --wait relay-test-db
demo_db=$(docker compose -f compose.test.yaml ps -q relay-test-db)
sql() { docker compose -f compose.test.yaml exec -T relay-test-db psql -X -At -v ON_ERROR_STOP=1 -U relay_test -d relay_test -c "$1"; }
docker run --rm --network relay-test_default -e RELAY_DATABASE_URL="$demo_url" "$demo_image" migrate >/dev/null
demo_exporter=$(docker run -d --network relay-test_default --network-alias relay-demo-metrics --read-only --cap-drop ALL --security-opt no-new-privileges:true --memory 128m --cpus 0.5 -e RELAY_DATABASE_URL="$demo_url" -e RELAY_METRICS_ADDR=:9091 "$demo_image" metrics-server)
demo_containers="$demo_exporter"
demo_prom=$(docker run -d --network relay-test_default --read-only --cap-drop ALL --security-opt no-new-privileges:true --memory 256m --cpus 0.5 --tmpfs /prometheus:rw,mode=1777 -v "$demo_dir:/demo:ro" prom/prometheus:v3.14.0 --config.file=/demo/prometheus.yml --storage.tsdb.path=/prometheus)
demo_containers="$demo_prom $demo_containers"
prom_query() { docker exec "$demo_prom" wget -qO- "http://127.0.0.1:9090/api/v1/query?query=$1"; }
wait_metric() {
 tries=0
 while [ "$tries" -lt 60 ];do
  if prom_query "$1" 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["status"]=="success" and len(d["data"]["result"])==1 and float(d["data"]["result"][0]["value"][1])==float(sys.argv[1])' "$2" 2>/dev/null;then return;fi
  tries=$((tries+1));sleep 1
 done
 echo "Timed out checking $1" >&2;exit 1
}
wait_alert() {
 tries=0
 while [ "$tries" -lt 60 ];do
  if docker exec "$demo_prom" wget -qO- http://127.0.0.1:9090/api/v1/alerts 2>/dev/null | python3 -c 'import json,sys; a=json.load(sys.stdin)["data"]["alerts"]; active=any(x["labels"]["alertname"]==sys.argv[1] and x["state"]=="firing" for x in a); assert active==(sys.argv[2]=="firing")' "$1" "$2" 2>/dev/null;then return;fi
  tries=$((tries+1));sleep 1
 done
 echo "Timed out waiting for alert $1 $2" >&2;exit 1
}
wait_metric up 1
echo '1. Prometheus is scraping the private exporter; no host ports are published.'
# A diagnostic worker leases a synthetic event but never sends HTTP.
sql "INSERT INTO clients(id,name) VALUES('00000000-0000-4000-8000-000000000001','observability demo');
 INSERT INTO destinations(id,client_id,url) VALUES('00000000-0000-4000-8000-000000000002','00000000-0000-4000-8000-000000000001','https://example.com');
 INSERT INTO events(id,client_id,destination_id,idempotency_key,request_hash,payload) VALUES('00000000-0000-4000-8000-000000000003','00000000-0000-4000-8000-000000000001','00000000-0000-4000-8000-000000000002','demo',decode(repeat('22',32),'hex'),'{}');
 INSERT INTO deliveries(event_id) VALUES('00000000-0000-4000-8000-000000000003');" >/dev/null
demo_worker=$(docker run -d --network relay-test_default -e RELAY_DATABASE_URL="$demo_url" "$demo_image" worker --lease-duration 5s)
demo_containers="$demo_worker $demo_containers"
tries=0
while [ "$(sql "SELECT count(*) FROM deliveries WHERE status='leased'")" != 1 ];do
 tries=$((tries+1));[ "$tries" -lt 30 ]||exit 1;sleep 0.1
done
docker kill --signal KILL "$demo_worker" >/dev/null
wait_metric relay_expired_delivery_leases 1
wait_alert RelayExpiredLeases firing
echo '2. SIGKILL left a durable lease; expiry is visible and RelayExpiredLeases fired.'
# Reclaim with a new diagnostic process, then SIGTERM to release gracefully.
demo_recovery=$(docker run -d --network relay-test_default -e RELAY_DATABASE_URL="$demo_url" "$demo_image" worker --lease-duration 30s)
demo_containers="$demo_recovery $demo_containers"
tries=0
while [ "$(sql "SELECT count(*) FROM deliveries WHERE status='leased' AND lease_expires_at>clock_timestamp()")" != 1 ];do
 tries=$((tries+1));[ "$tries" -lt 30 ]||exit 1;sleep 0.1
done
docker stop --timeout 5 "$demo_recovery" >/dev/null
[ "$(sql "SELECT count(*) FROM deliveries WHERE status='pending' AND lease_token IS NULL")" = 1 ]
wait_metric relay_expired_delivery_leases 0
wait_alert RelayExpiredLeases resolved
echo '3. A new process reclaimed the lease; graceful release resolved the alert.'
docker pause "$demo_db" >/dev/null
wait_metric up 0
wait_alert RelayMetricsUnavailable firing
echo '4. Pausing only the disposable database caused scrape failure and an availability alert.'
docker unpause "$demo_db" >/dev/null
wait_metric up 1
wait_alert RelayMetricsUnavailable resolved
[ "$(sql 'SELECT count(*) FROM events')" = 1 ]
[ "$(sql 'SELECT count(*) FROM delivery_attempts')" = 0 ]
echo '5. Database recovery restored fresh scrapes and resolved the alert; the original event remains.'
echo 'Demo passed. No outbound webhook was sent. Production alert holds remain unchanged.'
