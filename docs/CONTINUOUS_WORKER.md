# Continuous worker — milestone 2c.3b

## Start and stop explicitly

Complete [key provisioning](SIGNING_SECRETS.md) and configure a receiver you
control before starting delivery. Stop old workers before applying migration 006:

```bash
make worker-stop
make migrate
make up
make worker-start
make worker-logs
# Stop processing; leave API/database running:
make worker-stop
```

`worker-start` launches the existing `relay-delivery-worker` Compose service in
profile `delivery`, using `relay worker --send --continuous`. `make up` continues
to start only API/database. `make deliver-once` explicitly overrides the service
command to make one bounded cycle; `make worker` remains the lease diagnostic.
Avoid running the diagnostic against work intended for the live sender, because
it reserves events without sending them. `make down` stops the Relay project and
preserves its database volume.

There are no new host ports, shared networks, credentials or persistent volumes.
The worker keeps the existing read-only keyring mount, unprivileged UID/GID,
128 MiB / 0.5 CPU limits and ten-second stop grace period. Restart policy remains
`no`: an invalid keyring/schema at startup exits and requires correction followed
by an explicit start. No Kubernetes or public endpoint is involved.

The native equivalent, with Relay-only database/keyring environment variables:

```bash
relay worker --send --continuous --poll-interval 1s --lease-duration 30s
```

`--continuous` requires `--send`. Poll intervals must be whole milliseconds from
1 to 30 seconds. Sending leases retain their 15-second to 5-minute bounds.

## Polling and failure behavior

One process executes one cycle at a time. A cycle recovers at most one expired
attempt or claims/prepares/sends/finalizes one event. It then waits at least the
poll interval, even when the queue is busy. There are no per-event goroutines,
unbounded buffers or overlapping sends within one process. Independent processes
can compete through the existing `SKIP LOCKED` claim protocol.

The default one-second pause limits the maximum cycle rate to one per second per
process, plus operation time. Empty queues wait as well; idle messages use debug
level, so the default logger does not emit one line per idle poll.

A failed database/preparation/finalization cycle doubles the wait to a maximum of
30 seconds. At the default interval consecutive errors wait 2, 4, 8, 16, 30, 30…
seconds. A successful cycle, including an empty queue, resets the wait to the
configured interval. Logs record the backoff without forwarding raw dependency
errors. This process backoff is separate from a delivery's persisted retry deadline.

Due [retries](RETRIES.md) now run automatically while the process is active. Their
three-start limit, persisted jitter, stable event IDs, per-attempt signing versions
and unknown-outcome history are unchanged. One recovery cycle still sends no HTTP;
a subsequent cycle may claim work whose persisted deadline has passed.

## Unavailable destination keys

Migration 006 adds `destinations.delivery_paused_until`. When preparation finds no
active key or cannot decrypt the selected key, the worker performs a short
transaction that:

1. Locks the delivery and destination in the same order as attempt preparation.
2. Checks the current token, owner, `leased` status and expiry after lock waits.
3. Returns the unstarted delivery to `pending` and pauses new destination claims
   for 60 seconds, committing both changes together.

No attempt row is created and no HTTP budget is consumed. Claims skip **all**
events for the paused destination, allowing other destinations to progress. This
filter also applies to the diagnostic and one-cycle sender. `FOR UPDATE OF q`
locks only candidate delivery rows, not joined event/destination rows.

The pause is durable across process restarts. Fix/provision the key and wait for
the deadline; claims become eligible automatically. Key activation does not clear
the pause early. No sweeper is needed, and an elapsed timestamp may remain stored.
Another failed preparation renews the 60-second pause. Already claimed work is not
canceled by a later destination pause. Multiple workers can therefore observe a
missing key concurrently before the pause is committed, but cannot consume HTTP
attempts for that missing key or repeatedly claim paused work.

A stale/expired lease cannot pause a destination or release a newer claim. A
failed deferral commit preserves the original lease and no partial pause; later
expiry or a confirmed claim recovers it. A generic start-commit error is not treated
as a known key failure: existing release fencing prevents requeueing an uncertain
started attempt. Database errors use the process backoff described above.

The keyring file is loaded only at startup. Updating that file requires restarting
the worker. Startup verifies registered canaries; a missing/wrong master key can
prevent startup globally. Per-destination cooldown is for preparation failures
observed by a running, configured worker, not a bypass of keyring verification.

## Shutdown and observation

SIGINT/SIGTERM cancels polling waits and in-flight HTTP. No new cycle starts after
cancellation is observed. Existing attempt finalization/preparation cleanup uses a
fresh bounded three-second context. If the database cannot confirm the result,
lease expiration and unknown recovery apply. SIGKILL cannot run cleanup; durable
state is recovered by a later worker within the same attempt budget.

A running container is not a delivery-success or readiness guarantee. Observe
safe outcome/backoff logs and persisted state; there is no worker health HTTP
endpoint or metrics service yet. Attempt-history and cooldown inspection are
currently local administrative operations; owner-facing APIs remain planned.

```bash
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose exec -T relay-db psql -U relay_dev -d relay_dev -c \
  "SELECT id,delivery_paused_until FROM destinations WHERE delivery_paused_until>clock_timestamp();"
```

See [retry inspection](RETRIES.md#inspection-and-validation) for counts and history.
Logs omit secrets, signatures, URL/payload contents and raw SQL/network errors.

## Verify and study

`make test-integration` covers loop wait/backoff/reset, cancellation, invalid
configuration, cooldown rollback and fencing, blocked-destination isolation, key
repair, actual automatic retry, and shutdown during HTTP against local TLS only.
Existing tests continue to cover bounded retries, DNS/TLS/SSRF, concurrency and
migration preservation through schema version 5.

`make test-worker-process` runs separately against the disposable Relay database.
It tests the diagnostic's SIGKILL/SIGTERM behavior, loads a private test keyring in
the real continuous binary, verifies destination cooldown, pauses only the test DB
to force an outage, verifies resumed processing, and stops the worker with SIGTERM.
All its containers, network and private temporary files are removed afterward.

Study `internal/worker/continuous.go`: the `select` between a timer and `ctx.Done()`
makes waiting cancellable; the synchronous loop bounds concurrency. Then read
`internal/storage/cooldown.go`: releasing a reservation and pausing a destination
must commit together. `internal/worker/attempt.go` continues to own one cycle.

This milestone does not add replay, owner history, fair scheduling, quotas,
metrics, autoscaling, lease renewal or a production deployment. Stop here for review.
