# Client capacity quotas

Relay enforces the following fixed limits independently for each client. All
credentials and API processes share them; token rotation does not reset usage.

| Resource | Limit | Counted records |
| --- | ---: | --- |
| `destinations` | 20 | Registered destinations |
| `events` | 1,000 | All retained events, including terminal deliveries |
| `open_deliveries` | 100 | `pending`, `leased`, `attempting`, `retry_wait` deliveries |

These are capacity limits, not daily allowances. There is no calendar reset or
per-client override in this milestone. The authenticated request rate limit
remains separate and can reject a request before quota handling.

## Admission and retry behavior

Destination registration requires a destination slot. New event ingestion
requires both a retained-event slot and an open-delivery slot. A new replay grant
requires an open-delivery slot, but retains the same event and does not consume a
second retained-event slot. All admission checks and associated writes are in the
same database transaction. A rejected or rolled-back ingestion retains neither
an event nor an idempotency key; a rejected replay retains no authorization receipt.

Existing ingestion and replay receipts remain readable at full capacity. Retrying
with the original key and exact body returns the existing receipt; changed input
still conflicts. Ownership and replay eligibility checks precede quota checks.
Authentication, input validation, and the request rate limit still apply.

A quota rejection returns HTTP 409 with a stable code and resource, for example:

```json
{"error":"client quota exceeded","code":"quota_exceeded","resource":"open_deliveries","limit":100}
```

There is no Retry-After: capacity availability cannot be predicted from time.
For open-delivery exhaustion, inspect worker progress and signing-key health;
a committed terminal outcome frees a slot. Retry ingestion/replay using the same
key and body after capacity becomes available. Do not spin on 409 responses.

Delayed retries, signing-key cooldown, and expired leases still occupy slots.
Worker recovery or completion must actually change the delivery to a terminal
state (`succeeded`, `failed`, `unknown`) before its slot is freed. Automatic retry,
lease acquisition/release, and attempt start preserve an existing slot rather
than acquiring another. Revoking bearer credentials does not release slots.

Terminal events still count toward retained-event capacity until administrative
[retention cleanup](RETENTION.md) commits. Events become eligible 30 days after
their final terminal outcome; live work is preserved. Worker completion alone
does not free retained-event slots. Cleanup removes history and ingestion keys
together, so reusing an expired key may cause a new delivery. Destination deletion
is still unavailable; retention does not release destination slots.

## Inspect usage

Authenticated `GET /api/v1/usage` returns only the authenticated client's data:

```json
{
  "destinations":{"used":1,"limit":20},
  "events":{"used":35,"limit":1000},
  "open_deliveries":{"used":7,"limit":100}
}
```

No client ID is accepted to select another owner. The result is one PostgreSQL
statement snapshot, not a reservation: subsequent admissions and completions can
change it immediately. Responses omit destination URLs, payloads, credentials and
signing metadata. Authentication errors remain 401, database failures 503, and
request rate-limit exhaustion 429. Usage reads consume a request-rate unit, but
no capacity. The API remains private.

## Concurrency and storage tradeoffs

Destination creation, ingestion and new replay grants acquire the client row with
`FOR NO KEY UPDATE` before locking any destination or delivery. This serializes
operations that add capacity across database connections and API instances while
allowing unrelated foreign-key checks. A worker can preserve or reduce open work
without acquiring this lock; it never moves an existing terminal event back into
open work. Only controlled replay does that, under the admission lock.

Checks count committed rows plus the transaction's provisional event, using the
existing owner indexes. Admission counts stop once the relevant limit is reached;
queries may still scan nonmatching rows, especially for clients with legacy data.
Usage reports exact counts. HTTP database work remains bounded by the existing
five-second request deadline. No lock or transaction spans outbound HTTP.

Direct counting avoids denormalized counters and release hooks in every crash,
retry and completion path. It adds read work and serializes admissions per client;
measure contention before increasing these deliberately small limits. This is a
record-count policy, not an exact disk-byte quota: JSONB, exact payload bytes,
indexes, attempts and other metadata have storage overhead. Token/signing-version
history and global client provisioning are not capped by these quotas.

## Rollout and validation

No new migration is required: schema 010 already contains the rows and owner
indexes needed for these checks. No database data is rewritten or removed. Stop
old API/admin writers before starting updated binaries; old versions and direct
SQL writes do not participate in these application-level checks. Do not mix old
and new admission writers. Existing above-limit clients keep all data and receipt
access; additions are rejected for the exhausted resource. Workers may continue
draining existing work.

Run `make test-integration` with the isolated tmpfs database. Tests cover concurrent
registration, ingestion/replay races for the last slot, all live delivery states,
expired leases, duplicate receipts at capacity, rollback and reuse of rejected
keys, foreign usage isolation and the HTTP 409 contract. Existing tests cover
worker completion, recovery and replay failure behavior. No public endpoint or
shared VPS resource is created by this milestone.

Study `internal/storage/quotas.go`, `Store.Ingest`, and `Store.ReplayDelivery`:
`QuotaError` distinguishes policy rejection from persistence failure, while the
client lock makes the sequence "count, then write" safe under concurrency.

Validation on 2026-09-14: `make test-integration` passed with race detection,
`go vet`, and PostgreSQL log privacy. The final run includes concurrent retained
event admission and a real worker completion transaction freeing replay capacity.
Only disposable databases were used and the test stack was removed.
