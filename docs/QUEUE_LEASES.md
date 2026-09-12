# Queue leases — step 1

## Implemented boundary

Migration 003 adds delivery reservations. `relay worker` is deliberately a
**one-claim diagnostic process** in this step: it claims at most one delivery,
observes its lease and exits. It does not send HTTP, read signing secrets, create
attempts, retry a delivery or mark anything delivered. An empty or fully reserved
queue makes it exit successfully. This is not yet a continuously draining worker.

Only an explicit command starts the Compose worker profile. `make up` continues
to start the API/database only. The lease worker has no keyring mount, host port,
public route or connection to another project's network.

## Storage contract

| State | Meaning |
| --- | --- |
| `pending` | No reservation; eligible for a claim |
| `leased`, expiry in the future | Reserved by one token/owner |
| `leased`, expiry reached | Eligible for reclamation, even if the old process lives |

The database requires all three lease fields (token, owner, expiry) for `leased`
and no lease fields for `pending`. Existing events and deliveries remain intact
when upgrading from v1 or v2. Each acquisition generates a fresh random token,
including when the same owner reacquires the same event.

`ClaimDelivery` uses a short READ COMMITTED transaction:

1. Select the oldest eligible unlocked delivery, breaking ties by event ID, with
   `FOR UPDATE SKIP LOCKED`.
2. Set its token, owner, expiry and `leased` status in the same statement.
3. Return the lease only after commit succeeds; release the database connection.

There is no transaction held while the worker waits. Concurrent workers skip
locked rows instead of waiting on each other. Ordering is best effort, not strict
FIFO under concurrency. `ErrNoDelivery` means no eligible unlocked row was visible
at that moment, not that all work has been completed.

PostgreSQL's clock determines eligibility and expiry. The worker's observation
timer uses a conservative monotonic budget starting before the claim request,
so it does not compare the database clock with the host wall clock. Connection
wait/commit latency consumes that budget. The process may therefore finish its
observation slightly before the database expiry. Only the database grants the
next claim; no scheduler or sweeper is needed to reset expired rows.

`ReleaseDelivery` sets the row back to `pending` only when event ID, owner, token
and unexpired lease all still match. An expired or stale caller gets `ErrLeaseLost`
and cannot clear the next worker's reservation. Expiry is checked after acquiring
the row lock, so time spent waiting for that lock cannot authorize a stale release.
Any future completion/retry update
must enforce the same condition; an event ID or worker ID alone is insufficient.
Tokens are excluded from JSON/normal formatting and never written to worker logs.

If a claim commits but the response is lost, its caller cannot infer ownership.
The reservation simply expires. If a process is killed, closing its connection
does not release its already committed reservation. A later claim recovers it
after the stored deadline. A living but stalled process can also lose its lease.
This prevents stale database updates; it does not make future HTTP effects
exactly once. Receiver deduplication will still be required.

## Run and inspect

Use the existing development setup and credentials from [the setup guide](SIGNING_SECRETS.md).
Apply the new migration explicitly before upgrading the API or running the worker:

```bash
make migrate
make up
make worker                   # Observe one lease for up to 30 seconds
make worker WORKER_LEASE=5s    # Shorter diagnostic run
```

Submit an event first using [the API examples](API.md). No active signing secret
is needed to reserve it in this diagnostic step. Two workers competing for that
single event produce one acquisition and one `no delivery available` result.

The CLI also accepts `relay worker --lease-duration 30s` with a Relay-only
`RELAY_DATABASE_URL`. Durations must be whole milliseconds from 1ms through 5m.
The worker checks the current schema, requires no encryption key and opens no
HTTP listener. Database claim operations have a five-second deadline.

On SIGINT/SIGTERM, cleanup gets a fresh three-second context and tries to release
the lease. If it is expired, already replaced or the DB cannot be reached, recovery
uses expiration. Normal observation completion leaves the row `leased`; it becomes
claimable when the database deadline arrives. This deliberate behavior makes crash
recovery inspectable without inventing a successful delivery.

Read-only inspection (omits the ownership token and all payloads):

```bash
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose exec -T relay-db psql -U relay_dev -d relay_dev -c \
  "SELECT event_id,status,lease_owner,lease_expires_at,lease_expires_at <= clock_timestamp() AS expired FROM deliveries ORDER BY created_at,event_id;"
```

Authenticated event lookup and duplicate event submission can now report `leased`.
They expose neither owner/token nor secret/payload contents. A lease is reservation
metadata, not a delivery outcome. There is no event deletion or retry counter here.

## Verification and study

`make test-integration` uses only the isolated tmpfs database. Tests cover multiple
connection pools racing for one/many deliveries, skipping a locked row, actual
clock expiry, expiry while release waits for a row lock, pool disappearance,
stale-owner release after reacquisition, schema
invariants, canceled claims, deferred commit failure and v1/v2 upgrades. Worker
unit tests check cancellation with a fresh bounded cleanup context and ensure
that observation never invents a delivery result or exposes raw database errors.

`make test-worker-process` additionally builds the real binary, starts competing
containers, kills the holder with SIGKILL, waits for actual database-clock expiry,
verifies reacquisition, and checks SIGTERM release. Run it separately from
`make test-integration`: both use the same disposable `relay-test` project. No
host ports or signing credentials are used; containers are removed afterward.

Study `internal/storage/leases.go`, then its database tests, followed by
`internal/worker/worker.go`. Notice the difference between a row lock (held until
commit), a lease (durable metadata with a deadline) and the token (a condition that
rejects stale updates). See PostgreSQL's [locking clause](https://www.postgresql.org/docs/17/sql-select.html#SQL-FOR-UPDATE-SHARE)
and [clock functions](https://www.postgresql.org/docs/17/functions-datetime.html#FUNCTIONS-DATETIME-CURRENT).

The separate [signed attempt command](DELIVERY_ATTEMPTS.md) now uses these leases.
It adds `attempting` and terminal states; the diagnostic never claims or releases
started/terminal work. Retries, renewal, polling and replay remain planned.
