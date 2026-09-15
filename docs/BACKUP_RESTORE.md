# Isolated backup/restore drill

Run `make test-restore` with Docker Compose. The script compiles the Go test
binary, then runs PostgreSQL 17 tools and the binary on the internal
`relay-restore-drill_default` network. Source and restored databases use distinct
containers, a drill-only role/database, tmpfs storage and no host ports. Neither
Relay development data nor other applications' resources are mounted.

## Acceptance

The drill creates synthetic authenticated-client data, two destinations and two
events. One event is delivered before backup; one remains pending. It rotates
the active master key while preserving the historical key, so both are needed.

It then:

1. Writes a custom-format `pg_dump` and a separate private keyring backup.
2. Closes and drops the disposable source database and removes live key files.
3. Restores into the empty second database using `pg_restore --no-owner --no-acl
   --exit-on-error --single-transaction`.
4. Rejects missing, wrong and incomplete master-key sets.
5. Restores the complete keyring (0600), verifies canaries/migrations/readiness,
   authentication, exact event identity, idempotency and delivery history.
6. Delivers remaining work through the actual worker/sender to a local HTTPS
   fixture, verifying HMAC signatures and original payload bytes. Completed work
   must not be resent.

The fixture's DNS/dial substitution exists only in Go test code; production SSRF
checks are unchanged. The drill network has no Internet access. Compilation may
fetch Go modules before the isolated runtime starts.

## Safety and limits

A pre-existing drill network or container causes refusal rather than adoption or
deletion. The project name is pinned independently of `COMPOSE_PROJECT_NAME`,
and the script does not load the project `.env`. A local lock rejects concurrent runs.
The script removes only its own Compose stack on normal exit or interruption.
SIGKILL/host failure can leave resources: inspect `docker compose --env-file /dev/null -p relay-restore-drill -f
compose.restore.yaml ps` before manually using the same command with `down`. Never use
another project's Compose file for cleanup. The lock applies to runs from this checkout.

Database and secret artifacts are separated into mode-0700 temporary directories;
files are 0600. All artifacts are synthetic and disappear with the test container.
This exercises recovery mechanics, not durable off-host backup storage, retention,
encryption at rest of backup archives, scheduled jobs, PITR or an RPO/RTO guarantee.
An actual secret backup must live in an independently protected secret store.

For real maintenance, quiesce API, worker and administrative writers while taking
a coordinated database/keyring backup. Preserve **all registered master keys**;
creating a replacement key cannot decrypt existing ciphertext. Restore only into
fresh isolated resources, validate first, and explicitly plan cutover. See
[SIGNING_SECRETS.md](SIGNING_SECRETS.md) for the manual procedure.

Restoring a historical database may repeat deliveries performed after its
snapshot. Receivers must deduplicate using stable event IDs: restoration does not
provide exactly-once delivery.

## Study path

Read `scripts/test-restore.sh` for resource lifecycle and
`internal/delivery/restore_integration_test.go` for assertions. The test uses
`context.WithTimeout` to bound work, `t.Cleanup` to close resources, and the real
storage/worker interfaces. The independent keyring restores access to ciphertext;
PostgreSQL restores durable identity and delivery state. Both are necessary.
