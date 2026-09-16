# Implementation status

Last updated: 2026-09-16. This file tracks what exists; [ROADMAP.md](ROADMAP.md)
tracks milestone acceptance criteria and remaining work. Implemented does not mean
publicly deployed. The personal VPS now serves the institutional page and
authenticated public API, with its continuous worker enabled. See [deployment](DEPLOYMENT.md).

## Implemented

- [x] Go HTTP server, health/readiness, bounded requests, structured logs, graceful shutdown.
- [x] Isolated Docker Compose development/test databases and explicit PostgreSQL migrations.
- [x] Administrative client provisioning, hashed bearer tokens, destination ownership.
- [x] Administrative token rotation/revocation, two-active-token cutover and recovery (migration 009).
- [x] Atomic event/delivery ingestion, commit-before-ack, client-scoped byte-exact idempotency.
- [x] Authenticated event lookup without payload disclosure.
- [x] Encrypted signing secrets with staging, activation, rotation and revocation.
- [x] DNS-pinned HTTPS, SSRF policy, no redirects/proxies, request and response limits.
- [x] Signed webhook attempts, durable leases, fenced completion and crash recovery.
- [x] Three-start automatic retry budget with jitter and stable receiver deduplication IDs.
- [x] One controlled replay per terminal failed event, with audit/idempotency and up to three additional starts.
- [x] Opt-in continuous worker, bounded polling/error backoff, destination key cooldown.
- [x] Owner-scoped per-event attempt history with consistent, safe metadata.
- [x] Paginated owner-scoped delivery listing with status/destination filters (migration 007).
- [x] Isolated integration/race tests, worker process failure tests and log-privacy checks.
- [x] English institutional site, TLS, portfolio links and LinkedIn artwork.
- [x] Durable per-client rate limit: 120 authenticated requests/minute, HTTP 429 (migration 010).
- [x] Per-client capacity quotas, bounded open work and authenticated usage inspection.
- [x] Administrative Prometheus-format snapshot of persisted queue/attempt state.
- [x] Opt-in private exporter, monitoring integration configuration, alert rules and isolated recovery demo.
- [x] Relay development DB/exporter deployed and integrated with shared Prometheus (2026-09-15).

## Remaining work, in order

- [x] Optional Telegram routing deployed through a private Relay Alertmanager.
- [x] Verify Telegram firing and resolved message receipt end to end.
- [x] Full isolated backup/restore drill, including signing master keys.
- [x] Coordinated 30-day history/idempotency retention with bounded administrative cleanup (migration 011).
- [x] Personal public API deployment with bounded edge access and authorized signed HTTPS integration.
- [ ] Scheduled off-host backups and independent secret recovery.
- [ ] Automate reviewed releases and ongoing operational checks.

Kubernetes is optional and cluster management belongs to `platform-lab`.
At-least-once delivery permits duplicates; receivers must deduplicate. A bounded
retry policy cannot guarantee successful delivery.

## Review record

### Per-event history — 2026-09-14

Reviewed commit `122958d`: HTTP authentication and owner filtering, nullable result
fields, query snapshot consistency, existing index/budget bounds, error/log privacy,
and lifecycle/HTTP tests. No blocking correctness issue found in the inspected
change. The full isolated integration suite with race detector, vet and PostgreSQL
log-privacy check passed for that commit. This is a scoped code review, not an
independent security audit. Cross-event browsing remains a separate milestone.

### Delivery listing — 2026-09-14

Reviewed owner predicates, parameterized SQL construction, UUID/timestamp cursor
ordering, filter/client context validation, page limits, nullable metadata, and
migration 007. No blocking issue found in the inspected change. The status filter
uses current data, not a snapshot spanning pages; sparse matches may scan more
rows than the response limit, bounded by the request deadline. These limitations
are explicit in [DELIVERY_LIST.md](DELIVERY_LIST.md).

The full isolated integration/race suite, vet, and PostgreSQL log-privacy check
passed. The timestamp-tie pagination test was also rerun after removing its
dependency on the current date. No development/production migration was executed.

### Controlled replay — 2026-09-14

Reviewed transaction locking, ownership, exact-request idempotency, unique lifetime
grant, audit metadata, unchanged event identity, and worker budget/backoff math.
No blocking issue found in the inspected scope. Unknown terminal results remain
ineligible by policy; replay does not imply public production readiness.

The full integration suite passed with race detector, vet and PostgreSQL log
privacy, including a local TLS replay with key rotation and receiver deduplication.
Real process checks passed for live-claim exclusion, SIGKILL recovery, SIGTERM
cleanup, destination cooldown and temporary test-database outage recovery. The
test stack was removed. Migration 008 was applied only to disposable databases.

### Administrative client tokens — 2026-09-14

Reviewed credential disclosure after commit, owner-bound revocation, per-client
locking, database enforcement of two active slots, legacy credential migration,
metadata bounds and sanitized CLI errors. No blocking issue found in the inspected
scope. Revocation does not cancel already-authenticated requests or worker jobs;
uncertain commit responses require metadata inspection or idempotent revocation.

`make test-integration` passed with race detection, vet and PostgreSQL log privacy.
`make test-worker-process` passed for the real token CLI and worker crash, shutdown
and database-outage recovery. Migration 009 ran only on disposable databases; the
test stack was removed. The private running API/database was not upgraded.

### Authenticated request rate limit — 2026-09-14

Reviewed admission order, per-client row locking, clock sampling after lock
acquisition, transaction failure behavior, Retry-After, counter persistence and
migration 010. No blocking issue found in the inspected scope. Fixed-window
bursts, database admission cost, and missing pre-authentication protection are
explicit limitations; quotas and pending-work bounds were still open at that milestone.

`make test-integration` passed with race detection, vet and PostgreSQL log privacy.
After adding the two-token/two-handler HTTP ingestion check, the HTTP package was
rerun with race detection and `go vet ./...` passed again. Duplicate requests
retained one delivery while consuming the shared allowance. All databases were
disposable and the test stack was removed; no running private API/database was
upgraded. Worker process checks were not repeated because dispatch is unchanged.

### Per-client capacity quotas — 2026-09-14

Reviewed client-before-delivery lock ordering, all open states (including delayed
retries and expired leases), provisional-event counting, idempotent receipt
precedence, rollback, owner-scoped usage and structured 409 responses. No blocking
issue found in the inspected scope. These are application-enforced record limits,
not a disk quota; retention, key/token history bounds and mixed-version writer
rollout remain explicitly documented limitations.

`make test-integration` passed after correcting a UUID cast in a synthetic fixture.
The final suite includes race detection, vet and PostgreSQL log privacy, concurrent
destination/event admission, ingestion versus replay for the last open slot, and
real worker transaction completion releasing capacity without counter hooks.
Existing worker recovery tests also passed. Disposable databases/containers were
removed. No new migration, development-data change or deployment was required.

### Durable operational metrics — 2026-09-15

Reviewed single-statement snapshot consistency, database-clock age calculation,
fixed labels/cardinality, gauge semantics, collection deadline and clean stdout
on startup/collection errors. No blocking issue found in the inspected scope.
Aggregate scans grow with retained data; this is an administrative snapshot, not
a private HTTP exporter, heartbeat or monitoring/alert deployment.

`make test-integration` passed with race detection, vet and PostgreSQL log privacy.
After separating metric diagnostics onto stderr, the CLI package was rerun with
race detection, including a subprocess test for startup failure. The Make command
was inspected with `make -n metrics`; it was not run against development data.
The disposable test stack was removed. No migration or shared infrastructure
change was needed.

### Private exporter, integration and recovery demo — 2026-09-15

Reviewed private listener separation, bounded collection concurrency, no stale
responses, generated-config preservation, fixed alert labels/holds and isolated
failure cleanup. No blocking issue found in the inspected scope. Shared bridge
access is trusted; a dedicated read-only database role, notifications and actual
shared-monitoring activation remain documented deployment/hardening work.

`make test-integration` passed with race detector, vet and PostgreSQL log privacy.
`make test-alerts`, the Python integration-preservation test, merged Prometheus
config validation and both Compose configuration checks passed. `make
demo-recovery` verified a real Prometheus scrape, SIGKILL lease expiry, alert
firing, process recovery, resolution, disposable DB outage and recovery. All test
containers/network were removed. Vigil's repository and running shared monitoring
were unchanged; shared INFRASTRUCTURE.md records the new opt-in allocation.

### Shared monitoring activation — 2026-09-15

Initialized a new Relay-only development database with migrations 001–010 and
fresh credentials in ignored mode-0600 `.env`; no prior Relay volume existed.
Started only the database and private exporter, with no published host ports.
Applied the reviewed override to the existing Prometheus service, preserving
`vigil-prometheus-data`. No API, delivery worker or signing keyring was started.

Verified all five targets healthy, all four Relay rules evaluated without errors
and inactive, unchanged IDs for all other running containers, and healthy Relay
DB/exporter containers. Runtime Compose uses both the Vigil base and Relay
override: retain that combination for subsequent monitoring deployments.
Development DB/exporter restart policy remains `no`; restart them explicitly
after a host restart. No external notifications or production API deployment.

### Telegram routing activation — 2026-09-15

Started Relay's dedicated Alertmanager v0.34.0 without host ports or database
access. Local interactive setup verified channel posting permission and stored
credentials outside Git (directory 0700, files 0600). Prometheus now discovers
`relay-alertmanager:9093`; readiness passed. Configuration and receiver routing
checks passed, including rejection of another service's alert. Both monitoring
configuration-preservation tests passed. All five scrape targets remain up;
other running container IDs and the Prometheus data volume were preserved.
Initial activation checks sent no messages. Subsequent live verification sent
an isolated, explicitly marked synthetic alert with two-minute automatic expiry.
The operator confirmed both FIRING and RESOLVED in the channel. Alertmanager
recorded two Telegram notifications and zero failures; all five scrape targets
remained up. See [notification operations](NOTIFICATIONS.md).

### Isolated backup/restore drill — 2026-09-15

`make test-restore` exercises real PostgreSQL custom-format dump/restore on two
disposable tmpfs instances with an internal network and no published ports.
The source database and live key files are removed before recovery. Separate
private key backup preserves both historical and active master keys. Verification
covers missing/wrong/incomplete keys, canaries/readiness, client authentication,
idempotency, exact attempt history, and pending delivery through a local signed
HTTPS fixture without resending completed work. See [BACKUP_RESTORE.md](BACKUP_RESTORE.md).
Off-host storage, scheduled backups, backup retention and PITR remain future operations.

Review: pinned all drill Compose commands to an explicit project and disabled
implicit `.env` loading; refuse leftover containers as well as the network.
The complete drill passed with a conflicting `COMPOSE_PROJECT_NAME` environment
value, including post-restore idempotency conflict rejection and exact history
comparison. Temporary containers/network were removed; shell syntax and diff
checks passed. The drill uses a static test binary without the race detector.

### Coordinated history retention — 2026-09-15

Policy: at least 30 × 24 hours after the final terminal outcome; open deliveries
never expire. Migration 011 maintains terminal clocks, resets them on replay,
and grants existing terminal rows a full migration-time grace period. The
`prune-history` CLI defaults to a preview, supports explicit `--apply` and deletes
at most 100 events per client/transaction. Attempts, replay receipts, payloads
and ingestion keys expire together; destination/signing keys are preserved.

`make test-integration` passed with race detection, vet and PostgreSQL log privacy.
Tests cover preview/bounds, live/recent/foreign preservation, replay reopening,
receipt reuse only after deletion, commit rollback, concurrent cleaners, locked
rows and ingestion/replay races. `make test-restore` passed with both signing
master keys and exact preservation of terminal clocks after dump/restore.
Disposable resources were removed. Migration 011 and cleanup were not run on
the active VPS database; no scheduler or shared infrastructure change was made.
See [retention contract and operations](RETENTION.md).

Review completed: no blocking correctness issue found in client/delivery lock
ordering, atomic child-first deletion, migration grace or replay clock handling.
Repeated `make test-integration` passed with race detection, vet and PostgreSQL
log privacy; repeated `make test-restore` preserved master keys and terminal
clocks. Documentation clarifies that batch size bounds deletions rather than all
rows scanned, and retained-history gauges may decrease after cleanup.

### Personal public deployment — 2026-09-16

Backed up the existing database, created/backed up the master key, applied migration
011, registered keys and started API/continuous worker with pinned images and
restart policies. Updated only Relay's Nginx virtual host; added edge rate/body/
timeout limits and disabled proxy retries/caching. Metrics and administrative
paths remain private. Owner and demo credentials are stored locally outside Git.

An explicitly authorized synthetic event traversed public HTTPS and the actual
signed worker. Receipt durability, duplicate ingestion, receiver deduplication
after restart, signature rejection, ownership, 413 and edge 429 were verified.
Receiver tests passed with race detection and vet. Five monitoring targets and
other public sites remained healthy; unrelated container IDs were preserved.
Runtime roles now separate API/worker writes, read-only exporter access and
administrative migrations. A signed delivery with the restricted role passed;
negative privilege checks passed. A coordinated post-deployment database/key
backup was taken. No retention deletion or Kubernetes action was performed. See [DEPLOYMENT.md](DEPLOYMENT.md) for active
resources, credentials, limitations and future release/rollback procedure.
