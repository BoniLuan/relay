# Architecture

## Implemented: durable ingress

```text
client -- bearer token --> HTTP API --> PostgreSQL
                                      clients (token hash)
                                      destinations (client-owned)
                                      events + deliveries (one transaction)
```

One Go module and one binary expose `api` (default), `migrate`, and `create-client`
commands. The future worker will run as a separate process in this same repository.
There is no broker or in-memory queue. PostgreSQL owns durable state.

`internal/httpapi` owns protocol validation, authentication and status mapping.
Its small `Backend` interface permits failure-path HTTP tests. `internal/storage`
contains parameterized SQL using pgx's bounded connection pool. This is a concrete
storage implementation, without generic repository or service layers.

Each client has a random bearer token stored only as a SHA-256 digest. The
administrative CLI displays the raw token once. Each destination belongs to one
client; an event's composite foreign key enforces the same ownership in SQL.
Destinations are immutable in this milestone, and no signing secrets exist yet.

Ingestion checks ownership and JSONB compatibility, inserts an event, and inserts its pending delivery in
one transaction. `UNIQUE (client_id, idempotency_key)` arbitrates concurrent
submissions. `INSERT ... ON CONFLICT DO NOTHING` waits for a competing transaction;
the subsequent query at PostgreSQL's default READ COMMITTED isolation sees its
committed result. A matching request hash returns the original event; a mismatch
returns conflict. The API acknowledges only after `Commit` returns success.

`defer Rollback` closes error paths. A failed/lost commit response is inherently
ambiguous to the caller: retrying the same key resolves whether a record exists.
A deferred-constraint test forces failure at actual commit and verifies rollback;
concurrent tests verify one event/delivery for twelve simultaneous submissions.
HTTP integration tests reopen the application pool and verify durable lookup.

Migrations are embedded SQL but run only through `relay migrate`. The initial
migration and version record commit atomically under a transaction advisory lock.
Migration 001 is append-only: future changes need new numbered migrations and
runner updates. There is no automatic down migration; restore or forward-fix after
a reviewed backup. Readiness checks DB access and the required migration; liveness
is independent. Each HTTP storage operation has a five-second context deadline;
readiness has one second. Server read/write/idle timeouts bound connection use.

## Study the code

1. `cmd/relay/main.go`: explicit dependencies, `context`, signal cancellation,
   goroutines, a buffered error channel and graceful HTTP shutdown.
2. `internal/httpapi/router.go`: interfaces at the consumer boundary, middleware,
   `json.RawMessage`, body limits and `errors.Is` for public error categories.
3. `internal/storage/migrations/001_ingestion.sql`: foreign keys, uniqueness and
   the difference between application validation and database invariants.
4. `internal/storage/store.go`: connection pools, transactions, `defer`, conflict
   handling and why at-least-once requires an explicit idempotency contract.
5. The HTTP and PostgreSQL tests: protocol fakes complement real database failure
   and concurrency tests; neither replaces the other.

## Planned: delivery and operation

A separately started worker will claim pending work transactionally using expiring
leases. Delivery is at least once: a receiver can process a request before the
worker crashes or loses the response. Stable event IDs support receiver deduplication;
Relay does not promise exactly-once delivery.

The isolated `internal/delivery` primitives now implement the initial transport and
in-memory signing contract; see [delivery security](DELIVERY_SECURITY.md). They are
not connected to the API or queue. Durable signing-secret management is still a
mandatory prerequisite to worker activation.

The outbound policy covers SSRF-safe DNS resolution and connection pinning,
private/special-address rejection for IPv4 and IPv6, redirect policy, request
timeouts, response-size bounds and TLS verification. Secret storage/rotation still
needs implementation before worker activation. Format validation of a stored HTTPS
URL is not sufficient; signing and receiver verification now have a tested wire
contract in the isolated package.

Then implement attempt history, retry backoff/jitter with maximum attempts,
lease recovery after crashes, controlled replay and metrics. Synthetic receivers
must use a deliberate test-only policy without weakening production protections.

## Deployment boundaries

Compose development has a Relay-only persistent volume. Tests use a separate
Compose project and tmpfs database, without published database ports. There is
no production deployment, public edge connection or integration with other apps.
Before storing important data, add and exercise a backup/restore runbook.

Kubernetes is optional. Existing scaffold manifests do not provision the database
or configuration this version requires. Cluster management belongs to platform-lab;
application deployment adaptation is a later, explicit task.

## Review regressions

JSON syntax alone does not guarantee JSONB compatibility. `pg_input_is_valid`
checks the target type without throwing a data error; invalid payloads become a
stable `ErrInvalidPayload` mapped to HTTP 400. This costs an additional database
round trip and parse, avoiding a duplicate Go implementation of PostgreSQL numeric
and Unicode rules. Input nesting and UTF-8 are checked before database access.

PostgreSQL terse logging removes error detail/context; SQL and bind-parameter
logging are disabled for ordinary errors. The integration runner checks actual
container logs after a deliberately failed JSONB cast and a deferred constraint
failure. Synthetic payload markers must be absent while the operational error
remains. SQL exception messages must never include payloads or secrets.
