# Paginated owner-scoped delivery listing

`GET /api/v1/deliveries` uses existing bearer authentication. It lists one delivery
per event, newest first by immutable `(event.created_at, event.id)`. Event UUID
breaks timestamp ties. Only the authenticated client's deliveries are selected.
This is a private API endpoint, not a route on the public institutional site.

## Queries and response

| Parameter | Default | Contract |
| --- | --- | --- |
| `limit` | 20 | Integer 1 through 100 |
| `status` | All | `pending`, `leased`, `attempting`, `retry_wait`, `succeeded`, `failed`, `unknown` |
| `destination_id` | All owned destinations | Lowercase destination UUID |
| `cursor` | First page | Opaque continuation value from `next_cursor` |

Unknown parameters, repeated/empty parameters, malformed query encoding, invalid
filters, and invalid or mismatched cursors return 400. Missing or foreign-owned
destination filters return indistinguishable 404 responses. Missing/invalid bearer
authentication returns 401 before query validation. Database failures return 503.

```bash
# Start the private API using the README setup. Migration 007 is required.
make migrate
# Set RELAY_TOKEN securely and use an existing owned destination ID if filtering.
curl --fail-with-body --get http://127.0.0.1:18081/api/v1/deliveries \
  -H "Authorization: Bearer $RELAY_TOKEN" \
  --data-urlencode 'limit=20' \
  --data-urlencode 'status=failed'
```

A response has `items` and `next_cursor`:

```json
{
  "items": [
    {
      "event_id": "11111111-1111-4111-8111-111111111111",
      "destination_id": "22222222-2222-4222-8222-222222222222",
      "created_at": "2026-09-14T12:00:00Z",
      "status": "failed",
      "attempt_count": 3,
      "next_attempt_at": null
    }
  ],
  "next_cursor": null
}
```

`items` is always an array. `next_cursor: null` means no further matching row
existed in this query's snapshot. Otherwise send its value unchanged with the
same status/destination filters; `limit` may change between requests:

```bash
curl --fail-with-body --get http://127.0.0.1:18081/api/v1/deliveries \
  -H "Authorization: Bearer $RELAY_TOKEN" \
  --data-urlencode 'limit=20' \
  --data-urlencode 'status=failed' \
  --data-urlencode "cursor=$NEXT_CURSOR"
```

Use an item's `event_id` with `GET /api/v1/events/{id}/attempts` to inspect
[the bounded attempt history](DELIVERY_HISTORY.md). The list omits payloads,
URLs, signing material, leases and receiver response bodies. Responses are
`Cache-Control: no-store`. No query values or raw database errors are added to logs.

## Pagination semantics

The query asks for `limit + 1` rows. It returns at most `limit`, using the extra
row only to decide whether a cursor is needed. There is no total count or offset.
The next query selects tuples strictly older than the last returned tuple, so
newer arrivals do not shift the remaining rows as they would with offset paging.

Each request has a fresh PostgreSQL snapshot. This is browsing, not a point-in-time
export: concurrent state changes can move rows into/out of a status filter, and
an event committed later with an older transaction creation time can become
visible behind the cursor or be missed ahead of it. Start again at the first page
for a refreshed view. There is no database transaction spanning HTTP requests.

Cursors are versioned base64url-encoded positions with client/filter context.
They are neither encrypted nor signed, contain no secret, and are not an
access-control mechanism. Unmodified cursors from another client/filter are
rejected. A caller can construct a valid alternate position within their own
view; every SQL query still checks authenticated ownership. Do not depend on
cursor internals or use them as credentials. Invalid cursor input is bounded
before decoding; unknown cursor fields and trailing JSON are rejected.

## Migration and query bounds

Migration 007 adds `(client_id, created_at DESC, id DESC)` and
`(client_id, destination_id, created_at DESC, id DESC)` event indexes. Existing
delivery data, retries, idempotency and attempt histories are unchanged. Run the
explicit migration before running the new API; current readiness requires schema 9.
Index creation is transactional and may block writes on the events table while
building; use a maintenance window if applying it to a populated live database.
No shared database or production data is used by the test suite.

A status filter is applied on the joined delivery. Sparse status matches may
require scanning many owned events; `limit` bounds returned rows, not all database
work. The existing five-second authenticated request deadline bounds query time.
Further status-query optimization should follow measured workloads. No new
broker, package dependency, service, port, network or volume is introduced.

## Verification and code study

Run `make test-integration` for isolated PostgreSQL tests, the race detector, vet,
and database log privacy. Tests cover ownership, combined filters, empty/final
pages, timestamp ties, newer insertions between pages, cursor validation, canceled
queries, error handling, and migration from populated schema 6 without data loss.

Read `internal/httpapi/list.go` for validation and cursor encoding;
`internal/storage/list.go` for parameterized keyset SQL and the extra-row method;
then the HTTP integration and storage listing tests. [Controlled replay](REPLAY.md) is a separate mutation and is never triggered by a
read.
