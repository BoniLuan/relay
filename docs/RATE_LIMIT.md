# Authenticated request rate limit

Every matched authenticated API route shares a fixed limit of **120 admitted
requests per UTC minute per client**. Both active bearer tokens consume the same
budget. The limit is always enabled and fixed in this milestone; it is not a
per-token, per-IP, per-route, or per-process allowance.

Authentication happens first. Admission then commits before reading the request
body or invoking the endpoint. An admitted request consumes one unit even if it
later returns a validation error, 404, 409, 503, or an idempotent receipt. Retrying a
previously accepted event therefore still consumes request capacity, but does not
create another event. A denied request does not execute the endpoint or alter its
event, signing key, replay receipt, or delivery history.

Exhaustion returns HTTP 429, a JSON `error`, `Cache-Control: no-store`, and
`Retry-After: N` with a positive integer number of seconds (rounded up to the
next minute). Wait at least that interval, preferably adding client-side jitter,
then retry ingestion/replay with the same idempotency key and exact body. Other
requests may consume the next window first: Retry-After does not reserve capacity.
Do not automatically retry non-idempotent operations after ambiguous failures.

## Persistence and concurrency

Migration 010 adds one reusable `client_request_limits` row per client that has
requested admission, regardless of the number of requests or rotated tokens. A
short transaction inserts the initial row if absent, locks it, samples the
PostgreSQL clock, and commits the updated count. Different API processes using the
same database share the counter. Restarting the API does not reset it. Concurrent
first requests are serialized too; no transaction or row lock spans endpoint work.

Time is sampled after acquiring the lock, so a request queued across a minute
boundary uses the new window. Application-host clock skew does not affect the
budget. Database wall-clock corrections can affect window assignment; this is not
a monotonic or sliding-window algorithm. A fixed window allows up to 240 requests
around a boundary (120 just before, 120 just after); it does not limit every rolling
60-second interval or smooth bursts.

Database failure, lock timeout, or canceled admission returns 503 and the endpoint
is not run. A rolled-back admission does not consume a unit. If a commit succeeds
but its response is lost, capacity may be consumed without executing the endpoint;
retrying remains subject to the normal limit. Admission and event persistence are
separate transactions: a successful counter commit never acknowledges an event.
Event acknowledgment still requires the event/delivery transaction to commit.

## Scope and operational limits

Missing/invalid/revoked bearers return 401 and do not create counter rows. Root,
liveness, readiness, unmatched routes and method mismatches are outside this
limiter. Authentication queries still reach PostgreSQL before admission; this is
not protection against unauthenticated flooding or connection exhaustion. The
API remains private. Edge admission/concurrency controls are future public-deploy
work; no shared Nginx configuration is changed here.

This is not an event quota, pending-delivery cap, storage-retention policy, or
worker dispatch limit. The rate limiter alone does not bound accumulated work.
Capacity quotas are implemented separately; see [QUOTAS.md](QUOTAS.md).
Retention remains a separate milestone. Each authenticated request adds database
round trips and row contention; measure these before raising the limit or adding
infrastructure. No Redis, cleanup job, new port, volume or service is introduced.

## Run and study

Run `make test-integration` for isolated database tests, race detection and vet.
Tests cover concurrent independent pools, persistence across reconnection, window
reset, client isolation, failed commits, cancellation, upgrade from schema 009,
and HTTP admission/error semantics.

For the private development environment, apply `make migrate` explicitly before
starting updated API/worker binaries. Current readiness requires schema 10. Stop
old API binaries during rollout: they do not enforce the new limit. Migration 010
is additive and preserves client credentials and delivery data. Do not run tests
against development or production databases.

Study `internal/storage/rate_limit.go` alongside
`internal/httpapi/router.go`: returning `(retryAfter, error)` distinguishes a
normal policy rejection (429) from failure to establish admission (503). The
transaction and row lock establish concurrency safety; the HTTP wrapper ensures
every registered authenticated route passes the same admission boundary.

Validation on 2026-09-14: the full isolated integration/race suite, vet and
PostgreSQL log-privacy check passed. A subsequent HTTP/race check also passed with
two tokens and two handlers sharing admission while preserving one idempotent
event/delivery; vet passed again. Only disposable databases received migration 010.
