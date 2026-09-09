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

## 2b — Durable secrets and reliable worker (planned)

Implement the mandatory signing-secret lifecycle described in the security contract
before wiring the tested outbound primitives to queued work.
Add a separately launched worker, atomic claims, expiring leases, bounded request
and response handling, HMAC signatures, attempt history and bounded jittered retries.

Acceptance: concurrent workers cannot own the same active lease; private/special
addresses and redirect/DNS bypasses are blocked; timeouts and oversized responses
are bounded; failures retry to a limit; crashes recover expired leases; a receiver
can verify signatures and deduplicate stable event IDs. No exactly-once claim.

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
