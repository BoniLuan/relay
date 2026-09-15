# API contract

All `/api/v1/*` endpoints require `Authorization: Bearer <token>`. Requests with a
body require `Content-Type: application/json`. JSON bodies must use valid UTF-8,
are limited to 64 KiB and at most 64 nested objects/arrays (including the envelope);
unknown fields and multiple JSON documents are rejected. HTTPS destination URLs
are at most 2048 bytes, with a hostname and no userinfo or fragment. This is format
validation, **not an SSRF policy**. The API never resolves or fetches a URL; the explicit sending worker applies
[the outbound policy](DELIVERY_SECURITY.md).
Never put credentials in destination URLs or event payloads.

## Example

Set `RELAY_TOKEN` to the token from `make client NAME=local`, without saving it in
tracked files. On a shared shell, use `read -rs RELAY_TOKEN; echo` to avoid shell
history. The following requests are local only:

```bash
curl --fail-with-body http://127.0.0.1:18081/api/v1/destinations \
  -H "Authorization: Bearer $RELAY_TOKEN" -H 'Content-Type: application/json' \
  --data-binary '{"url":"https://example.com/webhooks"}'
# Copy the returned id into the following body.
cat > /tmp/relay-event.json <<'JSON'
{"destination_id":"REPLACE_WITH_DESTINATION_UUID","payload":{"type":"demo.created","value":42}}
JSON
curl --fail-with-body -i http://127.0.0.1:18081/api/v1/events \
  -H "Authorization: Bearer $RELAY_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-1' --data-binary @/tmp/relay-event.json
# Repeat the identical command: 200, same event id, no new delivery.
# Use the returned event id:
curl --fail-with-body http://127.0.0.1:18081/api/v1/events/EVENT_UUID \
  -H "Authorization: Bearer $RELAY_TOKEN"
unset RELAY_TOKEN
```

| Operation | Success | Response fields |
| --- | --- | --- |
| `POST /api/v1/destinations` with `url` | 201 | `id`, `url`, `created_at` |
| `POST /api/v1/events` with `destination_id`, `payload` | 201 new / 200 duplicate | `id`, `destination_id`, `status`, `created_at` |
| `GET /api/v1/events/{id}` | 200 | Same event metadata; payload is not returned |
| `POST /api/v1/events/{id}/replay` | 201 new / 200 duplicate | Immutable replay receipt; requires idempotency key and duplicate-risk acknowledgment; see [replay contract](REPLAY.md) |
| `GET /api/v1/deliveries` | 200 | Paginated owner-scoped summaries with status/destination filters; see [listing contract](DELIVERY_LIST.md) |
| `GET /api/v1/events/{id}/attempts` | 200 | Owner-scoped delivery status, retry schedule, and up to six ordered attempt records; see [history contract](DELIVERY_HISTORY.md) |

`payload` accepts JSON values representable by PostgreSQL 17 JSONB, including
explicit `null`; omitting it is invalid. Escaped NUL (`\u0000`), unpaired Unicode
surrogates and numbers beyond PostgreSQL numeric limits return 400. Rejected input
does not reserve its idempotency key. Large integers are not converted to float64;
valid surrogate pairs and the literal text `\\u0000` are accepted.
IDs in requests must be lowercase UUIDs. Events start `pending` and may become
`leased`, `attempting`, `retry_wait`, `succeeded`, `failed` or `unknown`; see
[the delivery state contract](DELIVERY_ATTEMPTS.md). Ingestion acknowledges durable
acceptance, not completed HTTP delivery. The event POST
returns a `Location` header for lookup; duplicates also return
`Idempotency-Replayed: true`.

## Idempotency and failures

`Idempotency-Key` is required on event POSTs: 1–128 ASCII letters, digits, `.`, `_`,
`:` or `-`. Its scope is the authenticated client, across all that client's
destinations. Store and retry the **exact body bytes**: whitespace, field order,
number formatting or a changed destination/payload produce a different SHA-256
request digest and therefore 409 when the key already exists. There is no JSON
canonicalization. The database preserves payload semantics as JSONB and, from migration 004,
exact payload bytes for sending. Legacy events use JSONB rendering.

Keys remain reserved while their event is retained: at least 30 days after its
final terminal outcome, and indefinitely while work is open. Administrative
[retention cleanup](RETENTION.md) removes history and the key together. Only after
that commit does reusing the key create a new event with a new ID; the old event,
history and replay URLs return 404. Merely reaching 30 days does not free the key.
Different clients may use the same key. A different key creates a new event even
if the payload matches. Destination registration is not idempotent.

| Status | Meaning |
| --- | --- |
| 400 | Invalid body, URL, event input or idempotency key |
| 401 | Missing/invalid bearer credential |
| 404 | Event/destination absent or owned by a different client |
| 409 | Key already used with different request bytes |
| 413 | Body exceeds 64 KiB |
| 503 | Database/schema unavailable or transaction failure |

Storage errors never become a success response. If a connection fails during
commit, or the HTTP response is lost, the client cannot infer whether acceptance
occurred. Retry with the same key and exact body to recover the durable result.
Authentication and ownership checks precede ingestion. SQL parameters, tokens,
URLs and payloads are not written to application error logs.

At-least-once delivery is the product direction. The current explicit worker makes
one signed attempt per invocation and persists [bounded retries](RETRIES.md) for
selected failures and interrupted work. Later cycles of the explicit [continuous worker](CONTINUOUS_WORKER.md), or
manual one-cycle invocations, process due retries.
Receivers must deduplicate using the stable event ID; ingestion idempotency alone
cannot prevent duplicate receiver side effects. See [delivery attempts](DELIVERY_ATTEMPTS.md)
and [the signing-secret lifecycle](SIGNING_SECRETS.md). [Controlled replay](REPLAY.md) now authorizes one additional bounded round.

## Database log privacy

Both Compose environments use terse PostgreSQL errors and disable ordinary error
statements and bind-parameter logging. This removes DETAIL/CONTEXT fields, where
PostgreSQL can include JSON payload excerpts. Input compatibility is checked with
`pg_input_is_valid` before inserting JSONB. Operational error messages remain;
do not add payloads to SQL exception messages or enable SQL tracing with real data.
Run `make test-integration` to check actual container logs with synthetic markers.
Native/custom deployments must apply the same PostgreSQL logging settings.

## Destination signing credentials

Stage, inspect metadata, activate and revoke signing-secret versions through the
[signing-secret API](SIGNING_SECRETS.md#owner-api). These operations require owner
authentication and do not start outbound delivery. The one-time staging response
is the only endpoint that discloses a signing credential.

## Reservation status

Event lookup and duplicate ingestion may return `status: leased`. An expired
reservation stays `leased` until another claim replaces it; no HTTP delivery is
implied by this state. Lease tokens/owners/deadlines are not included in this API.

`attempting` means a durable attempt started, not confirmed receiver acceptance.
On a subsequent sending invocation, expired started work keeps `unknown` attempt history and may schedule a bounded
retry; see [retry semantics](RETRIES.md). `succeeded` requires a complete bounded 2xx response and committed result.
`failed` and `unknown` do not prove the receiver performed no business operation.
Duplicate ingestion reports these states without creating another attempt.

`retry_wait` means a retry was scheduled in PostgreSQL. Repeated ingestion never
resets its attempt count or deadline. The explicitly started continuous worker processes due retries automatically;
otherwise processing requires another `make deliver-once` invocation.

Missing/decryption-failed signing keys pause new claims for that destination for
60 seconds. Its unstarted delivery remains `pending` without consuming an attempt.
The API currently exposes neither the cooldown deadline nor an early-resume action.

## Attempt history

The [per-event history endpoint](DELIVERY_HISTORY.md) returns safe attempt metadata
and scheduling in one consistent database snapshot. It never sends or replays an
event. The existing event lookup and idempotent ingestion responses are unchanged.

## Client bearer-token lifecycle

[Administrative commands](CLIENT_TOKENS.md) issue replacement credentials, inspect
bounded metadata and revoke tokens. There is no HTTP token-management endpoint.
Unknown and revoked tokens both return 401. Rotation preserves client ownership
and ingestion idempotency; revocation blocks future authentication but does not
cancel already authorized requests or outbound deliveries.

## Request rate limit

All authenticated routes share 120 requests per UTC minute per client, including
both active tokens and idempotent retries. Excess returns 429 with integer-seconds
`Retry-After` and no endpoint execution. Authentication failure remains 401;
admission storage failure returns 503. See [RATE_LIMIT.md](RATE_LIMIT.md) for
fixed-window bursts, retry semantics and remaining public-deployment protections.

## Capacity quotas and usage

Each client has limits of 20 destinations, 1,000 retained events and 100 open
deliveries (including retries, reserved work and in-flight attempts). New
ingestion and replay enforce capacity in their write transaction. A quota
rejection returns 409 with `code: "quota_exceeded"`, `resource` and `limit`,
without Retry-After. Existing exact idempotent receipts remain available.

`GET /api/v1/usage` requires a bearer and returns `destinations`, `events` and
`open_deliveries`, each containing integer `used` and `limit` values. This is a
snapshot for the authenticated client, not a capacity reservation. See
[QUOTAS.md](QUOTAS.md) for exact semantics and retention limitations.
