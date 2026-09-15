# Controlled replay

Implemented: `POST /api/v1/events/{id}/replay`, with existing bearer authentication.
This private endpoint authorizes asynchronous work; it never sends HTTP itself.

## Eligibility and lifetime limit

Only an owned delivery in terminal `failed` state with at least one committed
attempt and no prior replay is eligible. Pending, leased, attempting, retry-wait,
successful and terminal unknown deliveries are rejected. Malformed or missing
IDs and other clients' events return 404. Legacy terminal rows with zero starts
are not eligible.

There is **one manual replay per event for its entire lifetime**, not one per key,
destination, day or failure. It grants up to three additional committed starts
from the current count. A non-retryable failure can terminate this round early.
Unused original attempts are not carried forward. Examples:

| Original terminal failure after | Replay attempt numbers | Maximum lifetime starts |
| --- | --- | --- |
| 1 start | 2, 3, 4 | 4 |
| 2 starts | 3, 4, 5 | 5 |
| 3 starts | 4, 5, 6 | 6 |

Existing history is never deleted or renumbered. The event ID, destination, exact
payload bytes and ingestion idempotency remain unchanged. A replay cannot edit
or redirect the original event. The currently active signing key is pinned at
each new start; all DNS/SSRF/TLS and HTTP limits still apply. Destination cooldowns
are respected, and unavailable signing keys do not consume starts.

Unknown terminal outcomes deliberately remain ineligible in this first replay
policy. Even `failed` can mean the receiver performed the operation: response
errors and worker finalization failures can hide remote effects. Receivers must
verify signatures and atomically deduplicate the stable event ID with their
business operation. Replay is not a command to execute that operation again.

## Request and receipt

After inspecting the event history and correcting the cause of failure:

```bash
curl --fail-with-body -i "http://127.0.0.1:18081/api/v1/events/$EVENT_ID/replay" \
  -H "Authorization: Bearer $RELAY_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: recovery-1' \
  --data-binary '{"acknowledge_duplicate_risk":true}'
```

The acknowledgment must be present and true. No payload/URL override is accepted.
Body limits and unknown-field validation are the same as ingestion. The
idempotency key follows the existing 1–128 character syntax, but its scope here
is **this event's replay operation**, separate from event ingestion. Keys can be
reused on different events without linking their replay receipts.

The first committed request returns 201 with immutable receipt metadata:

```json
{
  "id": "33333333-3333-4333-8333-333333333333",
  "event_id": "11111111-1111-4111-8111-111111111111",
  "requested_at": "2026-09-14T12:00:00Z",
  "previous_attempt_count": 3,
  "max_attempts": 6
}
```

Repeat the same key and exact request bytes to get 200, the same receipt, and
`Idempotency-Replayed: true`. This works after processing starts or finishes;
it never grants more budget or requeues work again. A different key or changed
bytes returns 409 once a replay exists. Earlier rejected requests reserve no key.

A receipt confirms committed authorization, not successful delivery or its current
status. Inspect `GET /api/v1/events/{id}/attempts`: `replay` is `null` before any
replay, otherwise contains this receipt. `max_attempts` is the delivery's current
lifetime ceiling; the ordered `attempts` array remains bounded at six.

| Status | Meaning |
| --- | --- |
| 201 | Replay receipt and work authorization committed together |
| 200 | Identical request recovered the existing receipt |
| 400 | Invalid key/body or duplicate-risk acknowledgment absent/false |
| 401 | Missing/invalid bearer token |
| 404 | Event absent, malformed, or owned by another client |
| 409 | Ineligible delivery, or prior replay with different key/body |
| 413 | Request body exceeds 64 KiB |
| 503 | Database/transaction failure; retry the same key and exact body |

Responses are `Cache-Control: no-store`. Request/response loss around commit is
ambiguous: recover with the original key/body, not a new replay key. No secret,
URL, payload, raw database error, idempotency key or request digest is returned
in the receipt or added to application logs.

## Transaction and worker behavior

`ReplayDelivery` first checks ownership and locks the delivery row. Claims,
starts, completion and recovery use the same row lock. It checks for a previous
receipt before evaluating current eligibility, so duplicate requests remain
idempotent even after a worker changes state. Concurrent different requests have
one winner; concurrent identical requests recover the same receipt.

One transaction inserts the audit record and changes the failed delivery to
`pending`, setting `attempt_limit = attempt_count + 3`. It does not reset counters,
leases, original creation time, or attempt history. Existing terminal failures
have no live lease or retry schedule. A commit failure returns no receipt and
leaves no partial authorization. Audit records include the authenticated client,
timestamp, previous count and hashed key/request; no update/delete API exists.

The worker uses the persisted limit for claims, starts, retries and crash recovery.
Each round has the same retry policy: 5–10 seconds after its first failed start,
10–20 after its second (upper bounds exclusive). Attempt numbers stay global;
backoff uses the position within the current round. Old lease holders cannot
finish or release replay work. Repeated crashes consume the additional budget.
No service, keyring, rate-limit exemption or outbound-policy bypass is created.

## Upgrade and validation

Migration 008 introduces the audit table, a per-delivery limit (default three),
and a maximum of six historical attempt numbers. It does not requeue existing
terminal rows. Stop all old API and worker processes before migrating and deploy
the updated binary consistently: older workers assume a fixed three-start budget.
Run `make migrate` explicitly; current readiness requires schema 10. Start the API
with `make up`; start sending only when intended with `make worker-start` or
`make deliver-once`. No development/production migration is run by tests.

`make test-integration` uses the isolated tmpfs database and local TLS receivers.
Tests cover ownership, invalid input, live/success/unknown rejection, concurrent
requests, immutable duplicate receipts, rollback at commit, stale leases,
unchanged ingestion idempotency, limits after early and exhausted failures,
backoff in both rounds, crashes, restarts, and historical migrations. An end-to-end
HTTP test verifies commit failure and retry, stable payload/ID, key rotation,
signed TLS delivery, receiver deduplication and safe history/logging.

Study `internal/storage/replay.go` for the transaction, `attempts.go` for the
budget-aware worker changes, and `internal/delivery/replay_integration_test.go`
for the full flow. [Administrative token lifecycle](CLIENT_TOKENS.md) is now implemented. General
quotas, retention and public API
operation remain separate milestones; this feature is not public-production readiness.

Validation on 2026-09-14: `make test-integration` and `make test-worker-process`
passed. All temporary databases and worker containers were removed afterward.

New replay grants also require a free client open-delivery slot. Existing replay
receipts bypass the capacity check, preserving retry safety at full capacity.
The client admission lock precedes the delivery lock; see [QUOTAS.md](QUOTAS.md).

## Retention window

A replay grant clears the delivery's terminal retention clock. Its final outcome
starts a fresh 30-day window for the event, attempts and replay/ingestion receipts.
Duplicate receipt reads do not extend retention. After coordinated cleanup,
replay of that event returns 404; see [retention](RETENTION.md).
