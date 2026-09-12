# Roadmap

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

## 3 — Operation and demonstration (planned)

Add owner-scoped delivery history, controlled failed-delivery replay, token
rotation/revocation, quotas, metrics, backup/restore and a reproducible failure demo.

Acceptance: replay authorization and limits are tested; secrets/payloads are absent
from logs; a backup restores into an isolated database; the demo explains duplicate
delivery and recovery. Decide retention and idempotency expiry together.

## 4 — Portfolio publication and optional Kubernetes (planned)

Choose deployment based on measured resource needs. Plan DNS/TLS, credentials,
backups and one explicitly authorized integration. Kubernetes learning remains
independent in platform-lab; update application manifests with database/configuration
requirements only when that work is chosen.

Acceptance: existing VPS services are preserved; infrastructure allocations are
recorded; public authentication/quotas and restore procedures are demonstrated.

Stop after each milestone for code review and learning before starting the next.
