# Architecture

## Implemented: durable ingress

```text
client -- bearer token --> HTTP API --> PostgreSQL
                                      clients (token hash)
                                      destinations (client-owned)
                                      events + deliveries (one transaction)
```

One Go module and one binary expose `api` (default), administrative commands and
`worker`. The worker runs separately from the API, in the same repository.
There is no broker or in-memory queue. PostgreSQL owns durable state.

`internal/httpapi` owns protocol validation, authentication and status mapping.
Its small `Backend` interface permits failure-path HTTP tests. `internal/storage`
contains parameterized SQL using pgx's bounded connection pool. This is a concrete
storage implementation, without generic repository or service layers.

Each client has a random bearer token stored only as a SHA-256 digest. The
administrative CLI displays the raw token once. Each destination belongs to one
client; an event's composite foreign key enforces the same ownership in SQL.
Destination URLs are immutable. Signing credentials have a separate versioned
[lifecycle](SIGNING_SECRETS.md) and are stored only as AES-GCM ciphertext.

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

Migrations are embedded SQL but run only through `relay migrate`. Pending
migrations and their version records commit atomically under a transaction advisory lock.
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

## Implemented: one signed attempt

`make worker` remains a lease-only diagnostic. `make deliver-once` starts a separate
process that either recovers one expired attempt as unknown or sends one pending
event. See [delivery attempts](DELIVERY_ATTEMPTS.md) for exact states and deadlines.

The worker commits `started` plus the selected signing version before HTTP, then
uses `internal/delivery` outside the transaction and commits the result afterward.
The destination lock serializes active-key selection with lifecycle changes.
Completion checks the lease token/owner and expiry after acquiring the row lock.
There is no database connection held during network I/O.

The sender enforces [the outbound policy](DELIVERY_SECURITY.md) on every attempt.
Migration 004 retains original payload bytes for new events, while older events
use JSONB rendering. Signatures cover the stable event ID and exact sent bytes.

A crash between receiver acceptance and result commit cannot be resolved from
PostgreSQL alone. Expired started attempts become `unknown`, never automatically
pending. Failures are also terminal in this milestone. At-least-once delivery is
the direction for future bounded retries; exactly-once is not promised. Stable
event IDs permit receiver deduplication. Owner-facing history, retry scheduling,
controlled replay and metrics remain planned.

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

## Versioned signing secrets

The keyring is an API startup dependency, loaded from a private file and checked
against encrypted database canaries. Registration is explicit; startup never
modifies keys or runs migrations. Migration 002 preserves v1 data. Lifecycle
operations lock the destination row to serialize concurrency. Staging exports
plaintext only after commit; metadata reads do not decrypt credentials.

## Delivery reservation boundary

Migration 003 adds `leased` state, owner UUID, fresh acquisition token and database
expiry. Claims commit before returning and hold no connection during observation.
Release uses event ID plus owner/token and an unexpired deadline, rejecting stale
processes after recovery. The one-claim CLI is opt-in and needs no signing keyring.
Signed-attempt completion uses the same ownership condition. Future retry/replay
updates must preserve it as well.
