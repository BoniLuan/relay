# Durable bounded retries — milestone 2c.3a

## Scope and activation

Migration 005 adds durable attempt counts, per-event attempt numbers and retry
schedules. The existing `make deliver-once` processes one eligible event or recovers
one expired attempt, then exits. A due retry is eligible on a later invocation.
The [continuous worker](CONTINUOUS_WORKER.md) now processes due retries
automatically while explicitly running. Without it, waiting does not start HTTP.
`make up` still starts only the API and database. No new service, port, volume,
network, credentials or Kubernetes resources are required.

After reviewing your controlled receiver and its deduplication behavior:

```bash
make migrate
make up
make deliver-once
# Inspect the outcome. Invoke again after next_attempt_at for a scheduled retry.
make deliver-once
```

Do not run older sending binaries alongside migration 005/new workers. Stop any
one-off workers before upgrading, then rebuild via the make targets. Schema-aware
readiness prevents new invocations from starting against an old schema.

## Retry policy

The original automatic round permits **three committed attempt starts**, including
the first send. One explicit [controlled replay](REPLAY.md) can grant up to three
additional starts after a terminal failure. There is no automatic budget reset.

| Observed outcome | Delivery action |
| --- | --- |
| Complete bounded 2xx | `succeeded`; no retry |
| HTTP 408, 429, 500–599 | Schedule retry if budget remains; otherwise `failed` |
| Network, response read/limit, cancellation | Schedule retry if budget remains; otherwise `failed` |
| Input or destination-policy rejection | `failed`; no retry |
| Other non-2xx, including redirects and other 4xx | `failed`; no retry |
| Expired started attempt without confirmed result | Preserve attempt as `unknown`; schedule retry if budget remains, otherwise delivery `unknown` |

No receiver `Retry-After` value is consumed in this milestone. Every repeat applies
the same DNS/SSRF/TLS/time/size policy and re-reads the currently active signing key.
A previously public destination can become disallowed on a later DNS resolution.
There is no fallback to retired/revoked keys or bypass of outbound protections.
Missing/decryption-failed keys prevent attempt start and HTTP, and the existing
bounded preparation cleanup applies. Such failures do not consume the send budget;
the [destination cooldown](CONTINUOUS_WORKER.md) pauses new claims for 60 seconds
without consuming attempts.

After attempt 1, equal-jitter backoff is 5–10 seconds; after attempt 2, 10–20
seconds (upper bounds exclusive, millisecond storage precision). There is no fourth
automatic start without an explicit replay grant; the same backoff applies within
the replay round. The selected interval is persisted using PostgreSQL's clock in the result
transaction. Claims do not recalculate the delay, and process restart does not
reset it. Timing begins when scheduling is written; callers learn it only after
commit. Long commit latency can consume some of the waiting interval.

## State and transaction rules

- `retry_wait` requires a non-null `next_attempt_at` and a positive count below the persisted `attempt_limit`. Other
  states require a null schedule. Waiting deliveries hold no lease.
- Claims select due retries, fresh pending work or expired unstarted leases, with
  budget remaining. The existing row locks, `SKIP LOCKED` and fresh tokens apply.
  Claiming clears the consumed schedule. Selection remains best-effort creation
  order, not strict FIFO or a fair scheduler across clients.
- Attempt start increments `attempt_count` and inserts `attempt_number` atomically.
  Per-event attempt numbers are unique and bounded by six, with only three starts
  authorized initially. A failed start commit
  consumes no attempt; a crash after start commit does, even if no bytes were sent.
- Finalization stores an immutable historical failure and schedules the next retry
  in one transaction. Later attempts insert new rows; they never replace history.
  Completion still requires the current token, owner and unexpired lease.
- Recovery closes one expired started attempt as `unknown` and applies the same
  count limit/backoff atomically. Concurrent recovery cannot schedule it twice.
  The old worker cannot overwrite the schedule or a subsequent attempt's result.
- A claim/release without attempt start consumes no budget. Release may return due
  work to `pending`; the previous delay already elapsed before it was claimed.

The event UUID and stored payload remain stable across retries. Each attempt gets
a new attempt UUID, lease token and freshly computed signature timestamp (two
attempts in the same second may share a timestamp). The selected signing version
is recorded separately for each attempt. Idempotent resubmission returns the same
event/current status without resetting budget or schedule.

## At-least-once effects and unknown outcomes

A receiver may commit before Relay loses its response or finalization fails.
Retrying that event can perform the business operation twice unless the receiver
verifies the signature and atomically deduplicates the event ID with its operation.
A `failed` result also does not prove absence of side effects. No exactly-once
promise is made, and a bounded attempt limit cannot guarantee eventual success.

This milestone deliberately changes recovery of **started** attempts from terminal
unknown to bounded retry when budget remains. Migration does not reactivate any
existing terminal `failed`, `unknown` or `succeeded` deliveries. Their history is
numbered and counted but remains terminal. Existing started attempts retain their
consumed budget and use the new recovery policy after expiry. Controlled replay
of terminal work remains unimplemented; changing an idempotency key creates a new
event and is not a deduplicated retry.

## Inspection and validation

The authenticated event API now also reports `retry_wait`. Attempt-history and
schedule endpoints remain planned. Local administrative inspection omits secrets,
payloads, URLs and lease tokens:

```bash
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose exec -T relay-db psql -U relay_dev -d relay_dev -c \
  "SELECT event_id,status,attempt_count,next_attempt_at FROM deliveries ORDER BY created_at,event_id;"
docker compose exec -T relay-db psql -U relay_dev -d relay_dev -c \
  "SELECT event_id,attempt_number,signing_version,state,http_status,error_code FROM delivery_attempts ORDER BY event_id,attempt_number;"
```

`make test-integration` uses only the disposable Relay database and local TLS
fixtures. Coverage includes retryable/terminal outcomes, delay bounds, actual
clock eligibility, independent pools, concurrent claims/recovery, commit rollback,
three-attempt exhaustion including repeated crashes, v4 history migration, signing
rotation between attempts, and receiver deduplication after lost finalization.
Tests adjust deadlines only inside isolated schemas to avoid waiting for the full
production backoff; one test explicitly exercises a real database-clock deadline.

Study `internal/storage/retries.go` for policy; `attempts.go` for atomic budget and
schedule updates; `leases.go` for due-work selection; and `retries_test.go` plus
`internal/delivery/worker_integration_test.go` for failure behavior. Policy is
applied at persistence, so workers cannot accidentally omit the retry limit.

The next implemented part is the [continuous worker](CONTINUOUS_WORKER.md).
Replay and owner history remain separate milestones.
