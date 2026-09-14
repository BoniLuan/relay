# Implementation status

Last updated: 2026-09-14. This file tracks what exists; [ROADMAP.md](ROADMAP.md)
tracks milestone acceptance criteria and remaining work. Implemented does not mean
publicly deployed. The public domain serves a static project page; the API remains
private and workers are explicitly opt-in.

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

## Remaining work, in order

- [ ] Operational metrics, alerts and reproducible failure/recovery demo.
- [ ] Full isolated backup/restore drill, including signing master keys.
- [ ] Coordinated history retention and ingestion-idempotency expiry policy.
- [ ] Public API deployment hardening and one explicitly authorized real integration.

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
