# Implementation status

Last updated: 2026-09-14. This file tracks what exists; [ROADMAP.md](ROADMAP.md)
tracks milestone acceptance criteria and remaining work. Implemented does not mean
publicly deployed. The public domain serves a static project page; the API remains
private and workers are explicitly opt-in.

## Implemented

- [x] Go HTTP server, health/readiness, bounded requests, structured logs, graceful shutdown.
- [x] Isolated Docker Compose development/test databases and explicit PostgreSQL migrations.
- [x] Administrative client provisioning, hashed bearer tokens, destination ownership.
- [x] Atomic event/delivery ingestion, commit-before-ack, client-scoped byte-exact idempotency.
- [x] Authenticated event lookup without payload disclosure.
- [x] Encrypted signing secrets with staging, activation, rotation and revocation.
- [x] DNS-pinned HTTPS, SSRF policy, no redirects/proxies, request and response limits.
- [x] Signed webhook attempts, durable leases, fenced completion and crash recovery.
- [x] Three-attempt persisted retry budget with jitter and stable receiver deduplication IDs.
- [x] Opt-in continuous worker, bounded polling/error backoff, destination key cooldown.
- [x] Owner-scoped per-event attempt history with consistent, safe metadata.
- [x] Isolated integration/race tests, worker process failure tests and log-privacy checks.
- [x] English institutional site, TLS, portfolio links and LinkedIn artwork.

## Remaining work, in order

- [ ] Paginated owner-scoped delivery listing, with status/destination filters.
- [ ] Controlled replay with eligibility, authorization, idempotency, limits and audit history.
- [ ] Client bearer-token rotation and revocation.
- [ ] Rate limits, per-client quotas and bounds on pending work.
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
