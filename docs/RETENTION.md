# History retention and ingestion-idempotency expiry

## Contract

Relay retains each event, its exact payload bytes, attempts, replay receipt and
ingestion idempotency key together for **at least 30 × 24 hours after its final
terminal transition**. Migration 011 records `deliveries.terminal_at` using the
database clock. Eligible terminal states are `succeeded`, `failed` and `unknown`.
Unknown outcomes still mean that a receiver may have performed a side effect.

Open work (`pending`, `leased`, `attempting`, `retry_wait`) never expires, even if
its event was accepted months ago or its lease expired. A controlled replay
clears the terminal clock; its next terminal outcome starts a fresh 30-day window.
Duplicate ingestion, duplicate replay receipts and reads do not extend that window.

Age makes a record eligible; it does not automatically delete it. Until cleanup
commits, the original key/body still returns the original event and changed bytes
still conflict. Cleanup atomically removes attempts, replay receipt, delivery,
event/payload and its client-scoped ingestion key. After commit:

- Event/history/replay lookups return 404; delivery listings omit the event.
- Reusing the ingestion key is a **new submission**, subject to normal auth,
  rate limits and quotas, with a new event ID and possible new receiver effects.
- Retained-event quota capacity is released; destination and signing-key records,
  clients, bearer credentials and request-rate counters are preserved.

There is no tombstone or permanent deduplication ledger. Clients should use unique
keys for distinct operations and bound retries to the retention guarantee.
Receivers still deduplicate stable event IDs for retries/replay. A new event ID
created after expiry cannot be deduplicated against the old ID alone: applications
needing permanent business deduplication must retain a business-operation ID.
Operational delivery/attempt gauges can decrease after cleanup because they
measure retained records, not lifetime totals.
Backups can retain deleted records; restoring an older snapshot may reintroduce
history and duplicate-delivery risk. See [backup/restore](BACKUP_RESTORE.md).

## Administrative command

```bash
relay prune-history CLIENT_UUID --limit 25          # preview only
relay prune-history CLIENT_UUID --limit 25 --apply  # commit one batch
```

`RELAY_DATABASE_URL` must identify the intended Relay database. The command needs
no signing keyring, exposes no HTTP endpoint and makes no outbound requests.
The batch defaults to 100 events and must be between 1 and 100. A ten-second
command deadline bounds lock waits and database work. It never loops automatically.

For Compose, after explicitly backing up/migrating and building the new binary,
reuse the existing keyring-free diagnostic service as a one-off command runner:

```bash
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose build relay-worker
docker compose run --rm --no-deps -T relay-worker prune-history CLIENT_UUID --limit 25
# Review the target database, policy and preview before explicitly applying:
docker compose run --rm --no-deps -T relay-worker prune-history CLIENT_UUID --limit 25 --apply
```

The command overrides `worker`; it does not start a worker loop or deliver hooks.
Only run `--apply` when the deletion is intended. A preview is not a reservation
or approval token: apply selects eligible records again at its own cutoff.

JSON output contains `applied`, `retention_days`, `cutoff`, `limit`, `selected` and
`deleted`, without event IDs, keys or payloads. Counts describe **one batch**, not
total backlog. Preview briefly takes the same locks but rolls back without writes.
Locked deliveries are skipped; zero selected does not prove that no eligible
records exist. Inspect another preview after contention has cleared. Run subsequent
batches explicitly if needed. The batch limit bounds deletions, not every row
examined by the query; the command deadline also bounds expensive scans.

Deletion is reported only after commit. A commit/connection/output error may
leave the operator uncertain whether deletion committed; inspect a fresh preview
and usage instead of treating an error as proof that history still exists.

## Concurrency and rollout

Cleanup locks the client before deliveries, matching ingestion and replay. A replay
that wins the lock preserves live work; cleanup that wins makes replay return not
found. Ingestion either sees the retained receipt or creates new work after cleanup.
An old duplicate response can precede cleanup immediately once the window expired.
No transaction or lock spans outbound HTTP.

Migration 011 adds an indexed terminal clock, maintained by a database trigger
for completion, unknown-result recovery and replay. Existing terminal rows receive
a full 30-day grace period starting at migration time; migration itself deletes
nothing. Repeating it does not reset the clock. Open rows have no terminal clock.

Apply migrations explicitly before using updated binaries; do not let startup
apply them. Take a coordinated backup first. This milestone implements the
command and tests. Migration 011 was subsequently applied during the
[personal deployment](DEPLOYMENT.md); no cleanup or retention scheduler has been run.
Off-host backup retention, signing-key/token history pruning and global client
limits remain separate work.

## Validation and study

`make test-integration` covers live/recent/foreign preservation, bounded previews,
coordinated expiry, replay clock reset, rollback at commit, concurrent cleaners,
locked rows, ingestion/replay races, and migration grace. CLI tests cover explicit
apply, invalid input and private failures. `make test-restore` exercises full dump/
restore with the updated schema.

Study `internal/storage/retention.go`: the deferred rollback handles every early
return; successful output is built only after `Commit`. Explicit child-first
SQL keeps foreign-key deletion scope visible. The client lock protects admission
without introducing another counter, queue or service. Migration 011 centralizes
the terminal clock so every existing worker transition follows the same policy.
