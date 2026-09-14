# One signed HTTP attempt — milestone 2c.2

## Scope

`make deliver-once` explicitly runs one signed attempt, or recovers one expired
started attempt and exits. This command has no polling or lease renewal. [Controlled replay](REPLAY.md)
is a separate authenticated API operation; public API deployment remains planned. `make up` starts only API/database.
Retries now follow [the bounded schedule policy](RETRIES.md) on later invocations.
`make worker` remains the lease-only diagnostic described in [queue leases](QUEUE_LEASES.md).

The production sender always enforces [the outbound policy](DELIVERY_SECURITY.md):
public HTTPS/443, validated and pinned DNS, certificate verification, no redirects
or proxies, five-second total deadline and bounded headers/response body. Tests
use a local TLS fixture with overrides compiled only into test binaries.

## Durable boundaries

1. Claim an eligible delivery using its short transaction and fresh lease token.
2. In a second short transaction, lock the delivery and destination, decrypt the
   active signing key and pin its version. Insert a `started` attempt and change
   the delivery to `attempting`. Return work only after commit succeeds.
3. Send one POST outside any database transaction. Sign the stable event ID,
   current timestamp and exact bytes being sent.
4. In a final transaction, update both the attempt and delivery. The delivery
   must still have the same token/owner and an unexpired lease. Check expiry
   after obtaining the row lock, not before waiting for it.

| Delivery status | Meaning and next action in this milestone |
| --- | --- |
| `pending` | Eligible for claim |
| `leased` | Reserved, no committed attempt yet; expiry allows reclamation |
| `attempting` | Attempt start committed; HTTP may have happened |
| `succeeded` | 2xx response fully consumed within limits and result committed |
| `retry_wait` | A retryable failure or interruption committed with a future deadline |
| `failed` | Non-retryable failure or exhausted budget; terminal |
| `unknown` | Interrupted work exhausted its budget, or an existing terminal unknown |

At the beginning of each sending invocation, `RecoverAttempt` closes at most one
expired `attempting` attempt as `unknown` with error code `interrupted`, applies
the bounded retry policy and exits.
A following invocation may process pending work or a due retry. Recovery uses row locks
and `SKIP LOCKED`; it cannot change a live attempt. Diagnostic claims never select
`attempting`, `failed`, `unknown` or `succeeded` deliveries.

A crash after start commit but before HTTP also produces `unknown` history. This is
conservative: the database cannot distinguish it from a crash after the receiver
committed its business operation. A failed result commit leaves started work for
recovery; a lost response from a successful result commit may already be terminal.
The worker reports an unconfirmed result in either case. It never resends in the
same invocation or releases started work back to `pending`. Recovery may schedule
a later retry within the persisted budget.

`failed` does **not** guarantee the receiver did nothing: response read errors,
timeouts and cancellations may occur after a side effect. Bounded at-least-once processing requires later worker invocations to process
persisted retries; it does not guarantee eventual success. Exactly-once delivery is not promised. Retries preserve event IDs; controlled replay does too; receivers must durably deduplicate them.
A lease token fences database updates, not remote HTTP side effects.

## Payload and signing versions

Migration 004 adds `events.payload_bytes`. New ingestion stores the exact JSON
`payload` bytes alongside JSONB in the existing transaction. The HTTP envelope
(`destination_id`, `payload`) is not sent. Client-scoped idempotency still hashes
the complete original ingestion request and keeps its existing byte-exact rules.

Older events have no recoverable original formatting: they send PostgreSQL's
JSONB rendering. Large integers remain precise. A legacy rendering that exceeds
64 KiB is rejected by the sender as `input`, with no network request. New accepted
HTTP payloads fit that bound. Receivers verify the bytes they actually receive,
not a reserialized JSON value.

`StartAttempt` shares the destination lock with staging, activation and revocation.
It selects the active version under that lock and records its number with the
attempt. A missing active key or failed decryption prevents HTTP and attempts
a bounded transaction to release the unstarted lease and pause destination claims. The cycle completes with a warning
log; provision/fix the key and wait for the persisted 60-second
[destination cooldown](CONTINUOUS_WORKER.md). Other destinations remain eligible.

Rotation/revocation committed before preparation is respected. Rotation or
revocation after preparation commits cannot retract a secret already in a running
process or cancel a request in flight. Receivers should retain the previous key
for their chosen overlap window. Subsequent attempts will read the active version
again; retired/revoked versions are never selected as fallback.

## Deadlines, outcomes and logs

Sending defaults to a 30-second lease and accepts whole-millisecond durations
from 15 seconds to 5 minutes. Claim, recovery and preparation each have a
five-second context deadline. Preparation checks database expiry after lock waits,
requiring eight seconds remaining. Before sending, the worker also checks its
conservative monotonic budget for five seconds of HTTP plus three seconds of
finalization. Insufficient budget skips HTTP and attempts to record `canceled`.

SIGINT/SIGTERM cancels HTTP. Finalization gets a fresh bounded three-second context;
if it fails or ownership expires, later recovery records `unknown` history and applies the retry budget. No transaction
stays open during network I/O. The sending Compose service has a ten-second stop
grace period. SIGKILL cannot run cleanup.

History stores attempt/event/destination IDs, signing version, timestamps, state,
optional HTTP status and a fixed error code. Codes: `http_status` (non-2xx),
`destination` (policy rejection), `network` (including DNS/TLS/deadline failures),
`response` (read/size limit or non-standard status outside 100–599), `input`, `canceled`, and recovery-only `interrupted`.
A 2xx with a response error produces a `failed` attempt; the delivery may enter
`retry_wait`. The receiver body, URL, signature, plaintext
key and raw database/network error are never stored in attempt history or logs.
The internal lease token is stored for fencing, but omitted from logs/inspection.
An exit code of zero means the bounded work was recorded (or no work existed),
not necessarily that the receiver accepted it. Inspect the event/attempt outcome.

## Run and inspect

Complete [development and key provisioning](SIGNING_SECRETS.md), then migrate and
rebuild the API before ingesting new events:

```bash
make migrate
make up
```

Register a destination **you control**, stage its secret, install it at the
receiver, activate it and submit an event using the API guides. This manual command
can deliver to the oldest eligible event from any client in this Relay database:

```bash
make deliver-once
```

The native equivalent is `relay worker --send --lease-duration 30s`, with a
Relay-only `RELAY_DATABASE_URL` and `RELAY_KEYRING_FILE`. It verifies registered
keyring canaries before claiming. The opt-in `relay-delivery-worker` service mounts
the existing private keyring read-only on the existing Relay network, publishes no
port and does not restart automatically. No external receiver is contacted by tests.

Authenticated event lookup and duplicate ingestion report current delivery status.
An owner-facing attempt-history endpoint remains planned. For local administrative
inspection, omit payloads, URLs and lease tokens:

```bash
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose exec -T relay-db psql -U relay_dev -d relay_dev -c \
  "SELECT id,event_id,signing_version,state,started_at,finished_at,http_status,error_code FROM delivery_attempts ORDER BY started_at,id;"
```

## Verify and study

`make test-integration` covers authenticated ingestion through a real worker and
local signed TLS request into persisted results. It checks competing workers,
2xx, receiver errors, redirects, timeout, response overflow, policy rejection, idempotent
resubmission after completion and HTTP success followed by lost finalization and a deduplicated retry.
Storage tests inject start/finalization commit failures, test stale/expired tokens,
concurrent unknown recovery, exact/legacy payloads, key rotation/revocation and
upgrades from schema versions 1 through 4. Existing sender tests cover DNS rebinding,
TLS rejection, timeout, proxy isolation and response limits.

Study `internal/worker/attempt.go` for orchestration and context lifetimes, then
`internal/storage/attempts.go` for transactions and failure boundaries. The small
`AttemptQueue` and `Sender` interfaces describe behavior needed by this worker;
production still uses one storage implementation and one policy-enforcing sender.
See `internal/delivery/worker_integration_test.go` for the full path and
`internal/storage/attempts_test.go` for failures at database commit boundaries.

See [durable retries](RETRIES.md) and the [continuous worker](CONTINUOUS_WORKER.md)
for processing beyond one cycle. [Controlled replay](REPLAY.md) now grants one additional bounded round.
