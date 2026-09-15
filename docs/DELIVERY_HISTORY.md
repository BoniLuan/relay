# Owner-scoped event attempt history

Implemented endpoint: `GET /api/v1/events/{id}/attempts`.
It uses the existing bearer authentication and is available only on the private
API. The public institutional site does not proxy this route.

## Run and inspect

Use the local setup in the README to provision a client, destination, signing key,
and event. Save the event ID from ingestion. Then, with that client's token:

```bash
curl --fail-with-body "http://127.0.0.1:18081/api/v1/events/$EVENT_ID/attempts" \
  -H "Authorization: Bearer $RELAY_TOKEN"
```

A new event returns HTTP 200 and an empty array, not `null`:

```json
{
  "event_id": "11111111-1111-4111-8111-111111111111",
  "status": "pending",
  "attempt_count": 0,
  "max_attempts": 3,
  "next_attempt_at": null,
  "replay": null,
  "attempts": []
}
```

After explicitly authorized delivery attempts, each array item contains:

| Field | Meaning |
| --- | --- |
| `id` | Stable attempt UUID, distinct from the stable event ID |
| `attempt_number` | Committed start number, ascending from 1 through at most 6 |
| `state` | `started`, `succeeded`, `failed`, or `unknown` |
| `signing_version` | Version pinned when this attempt started; no credential |
| `started_at` | Database timestamp of the durable attempt start |
| `finished_at` | Recorded completion/recovery timestamp, or `null` while started |
| `http_status` | Observed HTTP status if available; otherwise `null` |
| `error_code` | Safe classification, or `null` before completion/on success |

The delivery-level `status` can differ from its latest attempt state: a failed
attempt can leave the delivery in `retry_wait`. `next_attempt_at` is a scheduled
eligibility time, not a promised send time; it is `null` outside `retry_wait`.
A destination cooldown can delay otherwise eligible work and is not included in
this endpoint. `attempt_count` counts committed starts, not proven HTTP sends.

Error codes are `http_status`, `destination`, `network`, `response`, `input`,
`canceled`, or `interrupted`. They contain no raw errors or receiver response text.
A response-read failure may retain an observed 2xx status while the attempt is
`failed`; HTTP status alone is not the delivery outcome. `unknown` means recovery
could not determine the result. Its finish timestamp records recovery, not the
actual instant of a worker crash or receiver action. Receivers must deduplicate.

## Boundaries and consistency

- Missing/invalid credentials return 401. Missing, foreign-owned, or malformed
  event IDs return the same 404 response, including events with no attempts.
- Database failures return 503, never an empty successful history. Authentication
  and history share the existing five-second request deadline.
- Responses use `Cache-Control: no-store`. URL, payload, request/response headers,
  raw error details, signing material, and lease owner/token are not returned or
  added to logs.
- One parameterized SQL statement checks ownership and reads delivery scheduling
  and all attempts from the same PostgreSQL statement snapshot. It acquires no
  row locks and never claims, recovers, retries or replays work. A worker may
  commit new information immediately after the snapshot; refetch to observe it.
- The existing unique `(event_id, attempt_number)` index and number constraint
  bound the array to six entries and order it deterministically. No pagination,
  migration, dependency or new index is necessary for this per-event endpoint.
  A future cross-event list requires its own bounded pagination design.
- No master keyring is needed to read persisted metadata. Token authentication
  still applies; this is not a public diagnostics endpoint.

## Validation and study path

Run `make test-integration`. It uses only the dedicated tmpfs Relay test database,
executes the suite with the race detector, runs vet, and checks database log privacy.
The full integration/race suite, vet, and PostgreSQL log-privacy check passed on
2026-09-14; the disposable test stack was removed afterward.
History tests cover empty/leased/started/retry/success/failure/unknown states,
ownership, missing IDs, ordering, null fields, restart persistence, cancellation,
private-data omission, and a reader during an uncommitted finalization transaction.
HTTP tests exercise real PostgreSQL ownership checks and verify error mapping,
authentication, and no-store responses.

Read `internal/httpapi/history.go` for the thin HTTP boundary, then
`internal/storage/history.go` for the explicit response types and consistent
query. Nullable fields use pointers so JSON distinguishes absent outcomes from
zero values. `internal/storage/history_test.go` follows a failed attempt, an
interrupted retry, and a successful final attempt without sending external HTTP.

[Paginated listing](DELIVERY_LIST.md) and [controlled replay](REPLAY.md) are now
implemented separately. `replay` exposes an immutable receipt or `null`;
`max_attempts` starts at three and may become four, five, or six after an explicit
replay grant. Reads never reset retry budgets. Retention, metrics and public API
deployment remain planned.

## Retention

History is retained with its event and replay/ingestion receipts for at least
30 days after the final terminal outcome. Open work does not expire. After
administrative cleanup commits, this endpoint returns 404; see
[retention policy](RETENTION.md).
