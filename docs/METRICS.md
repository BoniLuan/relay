# Operational metrics: durable snapshot

This first observability milestone provides `relay metrics`, an administrative
one-shot snapshot in Prometheus text exposition format. It reports persisted
queue and attempt state across API/worker processes, including work retained
through crashes. For the opt-in HTTP exporter, integration and alerts, see [OBSERVABILITY.md](OBSERVABILITY.md).

## Run

With the private development database running and migrated to schema 010:

```bash
make metrics
```

The Make target runs the existing Compose administrative service with the updated
binary, without starting its dependencies or allocating a TTY. Build diagnostics
go to stderr. Native invocation is `relay metrics` with `RELAY_DATABASE_URL` configured.
No bearer or destination signing secret is used to read the aggregates. The
existing admin Compose keyring mount still needs to exist as documented in the
README. No new port, service, volume, network, migration or shared Prometheus
configuration is needed. Database access is administrative; do not publish this
cross-client aggregate output through a client-authenticated API route.

## Series and semantics

| Metric | Type | Meaning |
| --- | --- | --- |
| `relay_deliveries{state}` | gauge | Retained deliveries in each current state |
| `relay_delivery_attempts{state}` | gauge | Retained attempts in each current state |
| `relay_open_delivery_oldest_age_seconds` | gauge | Age since creation of the oldest open delivery, or zero when empty |
| `relay_expired_delivery_leases` | gauge | Expired `leased` / `attempting` reservations awaiting worker recovery |

The delivery states are `pending`, `leased`, `attempting`, `retry_wait`,
`succeeded`, `failed`, and `unknown`. Attempt states are `started`, `succeeded`,
`failed`, and `unknown`. Every state is emitted, including zeros: exactly 13
samples in four metric families. There are no client, destination, event, token,
URL, HTTP-path or error-message labels.

All values are gauges over retained records. Do not apply counter `rate()` to
these series or interpret failed attempts as failed events. Retention can reduce
counts; replay changes a delivery's current state without deleting old attempts.
The oldest-open metric includes reserved, attempting and delayed-retry work. It
uses original delivery creation time even after replay, not the age of the replay
round or time overdue. Signing-key cooldown also remains open work. Expired leases
can be transient; a positive value alone is not proof that the worker is down.

## Consistency and failure handling

One read-only SQL statement collects all values using one MVCC snapshot and the
PostgreSQL clock. A concurrent commit is either visible or not visible to that
statement; the command never claims or repairs work and takes no delivery row
locks. Uncommitted attempts and outcome changes are not reported. Negative ages
from future timestamps are clamped to zero. Wall-clock adjustments can affect age.

Collection has a five-second deadline. A database failure produces a nonzero exit
and a sanitized error, with no metric output; it must not be interpreted as an
empty queue. Output is built only after successful collection. A downstream I/O
failure can still truncate transmission, so consumers must check exit status.
The command performs aggregate scans of retained tables. Output cardinality is
fixed, but query work grows with data; measure it before deploying frequent
scrapes. There is no collection cache or scheduled polling in this milestone.

Do not serve an old saved snapshot as live telemetry. Automated textfile
publication, if added later, needs atomic replacement and freshness monitoring.
This command does not provide scrape timestamps, worker heartbeats, HTTP request
counters, latency histograms, alert rules, or dashboards. The private exporter and optional monitoring integration are now implemented
separately; see [OBSERVABILITY.md](OBSERVABILITY.md).

## Verification and code study

Run `make test-integration` for isolated PostgreSQL tests, race detection and vet.
Tests cover empty queues, persisted status counts, expired leases, old open work,
real persisted failed attempts, reconnection, cancellation, fixed series output,
argument validation and fail-closed collection without private error disclosure.

Study `internal/storage/metrics.go` for the single-statement snapshot and
`cmd/relay/metrics.go` for bounded exposition. The distinction between a current
state gauge and a lifetime counter matters: a successful retry can move a delivery
to success while its earlier failed attempt remains in history.

Validation on 2026-09-15: the full isolated integration/race suite, vet and
PostgreSQL log-privacy check passed. CLI tests also passed after adding a
subprocess check that startup failures leave metric stdout empty.
