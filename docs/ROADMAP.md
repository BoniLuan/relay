# Roadmap

Use [STATUS.md](STATUS.md) as the completed/pending checklist. This document
records milestone scope and acceptance criteria.

## 0 — Scaffold (implemented)

Standard-library HTTP server, JSON process logs, graceful termination, health
routes, loopback Compose and optional Kubernetes scaffold examples.

## 1 — Durable authenticated ingress (implemented)

- Administrative client provisioning; bearer token hashes and destination ownership.
- Explicit PostgreSQL migration, bounded HTTP input and database deadlines.
- Atomic event/pending-delivery creation and client-scoped, byte-exact idempotency.
- Authenticated event lookup, database-aware readiness and dependency-free liveness.
- Isolated Docker development and ephemeral integration test databases.

Acceptance: unauthorized requests fail; foreign destinations/events return 404;
identical retries return one event; changed requests conflict; concurrent identical
submissions create one event/delivery; commit failure leaves neither accepted event
nor delivery; retry after rollback succeeds; events survive API pool/server restart.
Run `make test-integration` for race-enabled HTTP/database tests and vet.

## 2a — Safe outbound primitives (implemented; not wired to the API)

A conservative public-address policy, DNS-pinned HTTPS attempts, no proxies or
redirects, bounded request/response handling, in-memory HMAC signing and receiver
verification. See [the security contract](DELIVERY_SECURITY.md).

Acceptance: private and mixed DNS answers never reach the dialer; re-resolution
blocks rebinding between attempts; TLS verification remains enabled; redirects
are not followed; response limits and deadlines hold; tampering/expired signatures
fail; secrets are redacted. Tests use only local TLS fixtures.

## 2b — Durable destination signing secrets (implemented)

Migration 002, private AES-256-GCM keyring, canary verification and owner-scoped
staging, activation, rotation/revocation and one-time secret disclosure.
See [signing-secret lifecycle](SIGNING_SECRETS.md).

Acceptance: only owners manage keys; concurrent generation creates one staged
version; commit failure discloses no secret; restart/key rollover preserve keys;
wrong master keys, tampering and ciphertext copying fail; revoked keys are not
reused. A backup/restore procedure is documented; a full restore drill remains open.

## 2c.1 — Queue claims and expiring leases (implemented)

Migration 003, atomic `SKIP LOCKED` claims, fresh per-acquisition tokens, fenced
release and a one-claim diagnostic `relay worker`. No HTTP sends or completion.
See [queue leases](QUEUE_LEASES.md).

Acceptance: independent pools cannot acquire the same live reservation; locked
rows do not block other work; expired reservations can be reclaimed; stale owners
cannot release replacements; failed commits return no lease and leave work pending;
graceful cancellation attempts bounded cleanup. Existing v1/v2 data is preserved.

## 2c.2 — One complete delivery attempt (implemented)

Migration 004, exact payload bytes for new events, explicit `make deliver-once`,
active signing-version pinning, one bounded HTTPS request and atomic safe outcomes.
This milestone originally left expired started work terminal; 2c.3a below adds
bounded retries while preserving unknown history. See [delivery attempts](DELIVERY_ATTEMPTS.md).

Acceptance: local TLS end-to-end tests verify signed persisted payloads; concurrent
workers send once; failures/redirects/limits persist safe outcomes; start commit
failure returns no work; finalization failure cannot invent success; stale tokens
cannot finish; unknown recovery does not resend; migration preserves v1/v2/v3 data.

## 2c.3a — Durable retry scheduling (implemented)

Migration 005, three-attempt budget, due-time claims, equal-jitter backoff and
atomic result/schedule persistence. Expired started work keeps unknown history
and may retry within the same budget. Existing terminal work is not reactivated.
See [retry semantics](RETRIES.md). Each invocation still processes one item.

Acceptance: no early claims; concurrent due claims have one winner; restart keeps
schedules; commit rollback preserves counts/history; repeated failures/crashes stop
at three; key rotation is observed on retries; stable IDs permit receiver deduplication;
v4 history/terminal states survive migration.

## 2c.3b — Continuous worker operation (implemented)

Explicit `make worker-start`, sequential bounded polling, capped process error
backoff, cancellable shutdown and durable destination cooldown (migration 006).
The existing one-cycle and diagnostic commands remain available. See
[continuous worker operation](CONTINUOUS_WORKER.md).

Acceptance: idle/error cycles cannot spin; backoff resets and caps; unavailable
keys pause one destination without consuming attempts or starving another;
stale tokens and failed commits cannot partially defer work; key repair resumes
processing after cooldown; due retries run automatically; in-flight cancellation
records an outcome; a real worker survives a disposable DB outage and stops on SIGTERM.

## 3a — Owner-scoped per-event attempt history (implemented)

Authenticated `GET /api/v1/events/{id}/attempts`, safe metadata, retry schedule,
and deterministic attempt order. A single statement snapshot keeps delivery and
attempt results consistent. The existing three-attempt constraint bounds the
response without pagination. See [history contract](DELIVERY_HISTORY.md).

Acceptance: foreign and missing events return indistinguishable 404 responses;
unattempted events return an empty array; in-flight and unknown results remain
explicit; private fields are absent; reads do not wait on finalization row locks
or expose uncommitted changes; pool restart preserves history; errors fail closed.

## 3b — Paginated owner-scoped delivery listing (implemented)

Authenticated `GET /api/v1/deliveries`, keyset cursor, default 20 / maximum 100
results, status/destination filters and owner page indexes (migration 007).
See [listing contract](DELIVERY_LIST.md). Page snapshots are independent;
concurrent status changes can alter filter membership.

Acceptance: no cross-client data access; tied timestamps traverse deterministically;
newer insertions do not shift subsequent pages; cursors preserve client/filter
context; malformed input fails closed; old data survives migration; private
fields are absent; reads never claim or replay work.

## 3c — Controlled failed-delivery replay (implemented)

One owner-authorized replay per event lifetime, after terminal failure, with
exact-request idempotency and explicit duplicate-risk acknowledgment. Migration
008 adds durable audit receipts and a three-start additional budget (six total
at most). History and event identity are preserved. See [replay contract](REPLAY.md).

Acceptance: only the owner can replay eligible terminal failures; concurrent
requests yield one grant; duplicate receipts survive worker progress; failed
commits grant nothing; current keys and outbound policy apply; old leases cannot
finish replay work; failures/crashes stop at the persisted limit; original data
survives migration and ingestion idempotency remains intact.

## 3d — Administrative client token lifecycle (implemented)

Migration 009 preserves existing bearer hashes in `client_tokens`. Administrative
issue/list/revoke commands support a two-active-token cutover window, idempotent
revocation and recovery after all credentials are revoked. Client identity stays
stable. See [token lifecycle](CLIENT_TOKENS.md).

Acceptance: old tokens survive migration; both cutover tokens authenticate as the
same client; concurrent issuance cannot exceed two; revoked tokens fail new HTTP
authentication; failed commits expose no credentials or partial revocation;
metadata excludes hashes/plaintext and stays bounded; recovery works without an
old bearer; worker jobs and ingestion idempotency are unaffected.

## 3e — Authenticated request rate limit (implemented)

Migration 010, one durable fixed-minute counter per client across all tokens and
API processes. Admit at most 120 requests per UTC minute before endpoint work;
excess returns 429 with Retry-After. See [rate-limit semantics](RATE_LIMIT.md).

Acceptance: concurrent pools cannot exceed the window budget; reconnecting does
not reset it; old windows reset; clients remain independent; failed admission
commits never invoke the endpoint; 401 and health requests do not consume capacity;
legacy credentials survive migration. Fixed-window boundary bursts and the lack
of pre-authentication flood protection are explicit limitations.

## 3f — Per-client capacity quotas (implemented)

Fixed limits of 20 destinations, 1,000 retained events and 100 open deliveries.
Client-row admission locks serialize ingestion and replay without adding worker
counters. Authenticated usage exposes one owner-scoped snapshot. See
[capacity quotas](QUOTAS.md); schema 010 already provides the necessary indexes.

Acceptance: concurrent admissions never overfill the final slot; retries and live
leases remain counted; terminal work releases open capacity but retains events;
duplicate receipts remain available at capacity; quota or commit failure retains
no partial event or replay authorization; other clients remain independent.

## 3g.1 — Durable operational metric snapshot (implemented)

Administrative `relay metrics` emits fixed-cardinality Prometheus gauges for
retained delivery/attempt states, oldest open work and expired leases. One database
snapshot, a five-second deadline, no tenant labels or new infrastructure. See
[metric semantics](METRICS.md).

Acceptance: empty states emit zeros; persisted outcomes and expired leases are
visible across connections; database failure emits no misleading snapshot;
collection never claims work or exposes private identifiers.

## 3g.2 — Private observability and recovery demo (implemented, opt-in)

Dedicated exporter, preserved shared-Prometheus configuration via optional Relay
Compose override, four tested alert rules, and disposable Prometheus/worker/DB
failure demo. See [operations and activation](OBSERVABILITY.md). No automatic
shared deployment or external notification route.

Acceptance: failed scrapes expose no stale snapshot; collection concurrency is
bounded; existing scrape settings survive integration generation; alert holds and
resolution are tested; SIGKILL lease recovery and database outage produce real
Prometheus observations; the demo cleans up without touching other applications.

## 3g.3 — Restore and lifecycle hardening (planned)

Perform a full isolated backup/restore drill and design coordinated retention.

Acceptance: replay authorization and limits are tested; secrets/payloads are absent
from logs; a backup restores into an isolated database; the demo explains duplicate
delivery and recovery. Decide retention and idempotency expiry together.

## 4a — Public institutional site (implemented)

English static project site at `relay.boniluan.com`, dedicated origin TLS, and
portfolio links. The API remains private. See [site operations](SITE.md).

## 4b — Public API deployment and optional Kubernetes (planned)

Choose deployment based on measured resource needs. Plan DNS/TLS, credentials,
backups and one explicitly authorized integration. Kubernetes learning remains
independent in platform-lab; update application manifests with database/configuration
requirements only when that work is chosen.

Acceptance: existing VPS services are preserved; infrastructure allocations are
recorded; public authentication/quotas and restore procedures are demonstrated.

Stop after each milestone for code review and learning before starting the next.
