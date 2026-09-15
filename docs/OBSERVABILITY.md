# Private exporter, monitoring integration and recovery demo

Relay now provides an opt-in `relay metrics-server` process. Its dedicated HTTP
listener exposes `GET /metrics` and `GET /livez`; it does not mount API routes.
The exporter uses the same 13 persisted-state gauges as `relay metrics`. See
[METRICS.md](METRICS.md) for their meanings and limitations.

## Private exporter

Native default: `127.0.0.1:18082`. `RELAY_METRICS_ADDR` overrides the listener;
Compose sets `:9091` inside the container and publishes **no host port**. Keep
native listeners on loopback and never route the exporter through the public edge.

With Relay's development database already running and migrated:

```bash
make metrics-start
make metrics-stop
```

The commands use `compose.yaml` plus `compose.metrics.yaml`. The exporter joins
Relay's existing backend network and the external `vigil-monitoring` network with
alias `relay-metrics`. Only this process joins monitoring; the API/database remain
on their existing networks. `--no-deps` does not start the database or workers.
The exporter mounts no signing keyring and introduces no new credential. Its
current database connection uses the existing Relay role; the HTTP surface offers
only fixed aggregate reads. A dedicated read-only database role is future hardening.

The monitoring bridge is the trust boundary: its members can read cross-client
aggregates without bearer authentication. This is not an Internet-facing service.
The command opens no public domain or TLS route. `GET /livez` checks the process,
not the database. A failed database collection returns HTTP 503 from `/metrics`,
causing Prometheus `up` to become zero. There is no stale-data fallback.

Only one collection runs at a time; overlapping scrapes receive 503. Each query
has a five-second deadline, with bounded HTTP header/read/write/shutdown timeouts.
No delivery lock spans a scrape. SIGTERM cancels active collection and stops the
listener. The live gauge query scans retained records, so a slow database can
make a scrape fail; avoid duplicate independent collectors against this process.

## Integrating the existing Prometheus without changing Vigil sources

The shared Prometheus currently belongs to Vigil's observability Compose project.
Relay supplies an **optional override**, a scrape fragment and alert rules. It does
not edit Vigil files, restart shared containers or enable monitoring automatically.

`make monitoring-prepare` uses Python 3 and PyYAML to read the current shared
configuration, preserve its global settings, jobs, alert routes and rule files,
and append Relay's job/rules into ignored `.local/prometheus.integrated.yml`.
The source SHA-256 is recorded beside the output. Existing Relay jobs are rejected
to avoid accidental duplication. Review the generated configuration locally;
it can contain secrets if the shared source ever gains credentials.

```bash
make monitoring-prepare
make test-alerts
# Check the generated configuration and rule mounts using the deployed version.
docker run --rm --network none \
  -v "$PWD/.local/prometheus.integrated.yml:/etc/prometheus/prometheus.yml:ro" \
  -v "$PWD/deploy/monitoring:/etc/prometheus/relay:ro" \
  --entrypoint /bin/promtool prom/prometheus:v3.14.0 \
  check config /etc/prometheus/prometheus.yml
```

Activation is a separate deployment operation, after starting Relay's exporter
and reviewing the merged config against the current source. Recheck the source
hash immediately before activation; regenerate and revalidate if it changed.
Use the existing observability environment file without printing its contents:

```bash
docker compose --env-file /home/luan/.config/vigil/observability.env \
  -f /home/luan/projects/vigil/deploy/observability/compose.yaml \
  -f /home/luan/projects/relay/deploy/monitoring/shared.override.yaml \
  config --quiet
# Activates only the existing Prometheus service; preserves its data volume.
docker compose --env-file /home/luan/.config/vigil/observability.env \
  -f /home/luan/projects/vigil/deploy/observability/compose.yaml \
  -f /home/luan/projects/relay/deploy/monitoring/shared.override.yaml \
  up -d --no-deps --force-recreate prometheus
```

Recreation is deliberate: the generated file is atomically replaced and an old
file bind mount would otherwise retain its previous inode. Do not use a reload
alone after regenerating the root configuration. Preserve this override in future
Prometheus deployments, and regenerate whenever the Vigil source changes; the
merged file is a snapshot, not a live include of that source.

After deployment verify all existing targets remain healthy, `relay-metrics` is
up, and the four Relay rules appear with no evaluation errors. Query examples
through the private Prometheus API or existing Grafana datasource:

```promql
up{job="relay-metrics"}
relay_deliveries{job="relay-metrics"}
relay_expired_delivery_leases{job="relay-metrics"}
ALERTS{service="relay",alertstate="firing"}
```

Relay's target labels explicitly say `environment="development"`, `stack="relay"`
and `service="relay"`; they do not inherit Vigil's production identity. Scraping
is every 30 seconds with a seven-second timeout and bounded sample/label counts.
For rollback, recreate **only Prometheus** using its original Compose file without
Relay's override, then stop Relay's exporter. Do not remove any persistent volume.
No Grafana dashboard is provisioned here. Optional Telegram routing is described
in [NOTIFICATIONS.md](NOTIFICATIONS.md).

## Alerts and interpretation

| Alert | Condition and hold | First investigation |
| --- | --- | --- |
| RelayMetricsUnavailable | `up == 0` for 2 minutes | Exporter network/process and database connectivity |
| RelayMetricsTargetMissing | no Relay `up` series for 5 minutes | Missing/renamed scrape job or target |
| RelayOpenWorkAging | oldest open creation age > 15 minutes for 5 minutes | Worker progress, delayed retries and signing-key cooldown |
| RelayExpiredLeases | expired leases > 0 for 2 minutes | Worker recovery and database access |

These rules report firing state inside Prometheus. The optional dedicated
Alertmanager forwards the four Relay alerts to Telegram; see
[NOTIFICATIONS.md](NOTIFICATIONS.md). `promtool` tests validate holds,
brief spikes, the exact age threshold, missing targets and resolution. Gauges are
not lifetime counters: failed historical attempts are not treated as a new-error
rate. An idle queue does not establish worker liveness; heartbeats and HTTP latency
instrumentation are outside this milestone.

Sources: [Prometheus configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/)
and [rule testing](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/).

## Reproducible isolated demo

```bash
make demo-recovery
```

Run separately from integration/process tests. The script refuses an occupied
`relay-test` Compose project. It starts that project's tmpfs PostgreSQL plus
short-lived exporter, diagnostic workers and Prometheus containers on its existing
network, without host ports or persistent Prometheus storage. The demo copies the
production alert expressions but changes holds to **five seconds**, with one-second
scraping; production files and thresholds remain unchanged.

The script asserts each step through PostgreSQL and the real Prometheus API:

1. A fresh private scrape succeeds.
2. A diagnostic worker reserves a synthetic event; SIGKILL leaves its durable
   lease behind. Expiry becomes a metric and the expired-lease alert fires.
3. A replacement process reclaims the reservation, then SIGTERM releases it.
   The expired-lease metric falls to zero and the alert resolves.
4. Only the disposable database is paused. Scraping fails and the availability
   alert fires; no cached queue metrics pretend the database is healthy.
5. Unpausing restores fresh collection and resolves the alert. The original event
   remains persisted. Cleanup removes the demo containers/network and tmpfs data.

This demo sends **no outbound HTTP webhook** and does not invent a successful
receipt. It demonstrates lease and monitoring recovery; signed-delivery retry and
receiver deduplication remain covered by the local TLS integration tests. A crash
**after** an HTTP side effect is ambiguous, so at-least-once delivery can repeat the
same event ID; a receiver must deduplicate it. Metrics cannot prove exactly-once
execution or tell whether a remote side effect occurred.

Tests: `make test-integration`, `make test-alerts`,
`python3 scripts/tests/test_monitoring.py`, and `make demo-recovery`.

Validation on 2026-09-15: all listed tests passed, including the real Prometheus
demo. The generated shared config and Compose override passed validation while
preserving all four existing scrape jobs. The shared override was **not activated**;
no Relay development database or shared service was started/recreated.

## Active VPS deployment — 2026-09-15

The optional integration is now **active**. `relay-dev-relay-db-1` and
`relay-dev-relay-metrics-1` are healthy. The new Relay-only database contains schema
010 and no events at activation. Its newly generated credentials are in ignored
mode-0600 `.env`. No API, delivery worker or signing keyring was provisioned by
this observability deployment. Neither container publishes a host port.

The existing Prometheus was recreated with the override above and retains
`vigil-prometheus-data`. `relay-metrics`, `vigil-api`, `vigil-worker`, `node-exporter`
and `cadvisor` were all verified up; four Relay rules were healthy and inactive.
All other running container IDs remained unchanged. Keep the override in future
Prometheus deploys so Relay's scrape job and rules remain loaded.

The earlier validation-only record describes the state before this activation.
DB/exporter retain the development `restart: "no"` policy. After a host restart,
run `make db` followed by `make metrics-start`; API/workers are not dependencies.
Use the documented original-Compose rollback to disable the shared integration
while preserving all monitoring data.
