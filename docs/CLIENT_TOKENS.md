# Administrative client token lifecycle

Client identity is stable; bearer credentials are replaceable. Token management
uses database-backed CLI commands, not client-authenticated HTTP endpoints. A
compromised API token cannot issue another token through Relay's HTTP API.

## Rotation without interrupting an integration

Each client may have **at most two active tokens**. Both authorize the same client,
its destinations, events, history, and ingestion idempotency namespace. Rotate in
three deliberate steps: issue, install/verify, then revoke the previous token.
There is no automatic expiration or grace-period timer in this milestone.

Apply migration 009 before using the updated binary. Stop old API/admin processes
before upgrading; the old `clients.token_hash` column is removed and there is no
legacy authentication fallback. Update worker binaries consistently for schema
readiness, although token revocation does not affect worker delivery authorization.

With the README's private Docker Compose setup:

```bash
make migrate
# Direct Compose commands need the same user IDs that Make normally supplies.
export RELAY_RUN_UID="$(id -u)" RELAY_RUN_GID="$(id -g)"
# Set CLIENT_ID to the metadata UUID from create-client; it is not a credential.
docker compose run --rm relay-admin list-client-tokens "$CLIENT_ID"
docker compose run --rm relay-admin issue-client-token "$CLIENT_ID"
```

Successful issuance prints `client_id`, `token_id`, and the plaintext `token`
once, only after the transaction commits. Store the bearer in the integration's
secret configuration, not source control. The `relay-admin` Compose service has
Docker logging disabled. With native/custom execution, prevent terminal capture,
command-output collection and container log retention for credential output.
Never pass a bearer as a positional CLI argument.

Verify the replacement against the private API before revoking the old token:

```bash
read -rs RELAY_TOKEN; echo
printf 'header = "Authorization: Bearer %s"\n' "$RELAY_TOKEN" | \
  curl --config - --fail-with-body http://127.0.0.1:18081/api/v1/deliveries
unset RELAY_TOKEN
# OLD_TOKEN_ID is metadata from the list command, not the bearer value.
docker compose run --rm relay-admin revoke-client-token "$CLIENT_ID" "$OLD_TOKEN_ID"
```

Revocation is idempotent: repeating the same client/token IDs succeeds without
changing the recorded revocation timestamp. It can revoke the last active token.
The operator can then recover access with `issue-client-token` using database
administrative access; no old bearer or signing master key is required by the CLI
code. Compose still follows its existing admin container/keyring mount setup.

## Metadata and bounded listing

`list-client-tokens CLIENT_ID` returns JSON metadata: `id`, `client_id`, `state`,
`created_at`, and nullable `revoked_at`. It returns active tokens first, then newest
revoked records, at most 50 rows. This always includes both active tokens, even
if they are older than many revoked records. This is not an exhaustive audit
export. Retention/export policy remains separate work.

Migration 009 preserves every existing token's hash and creation time. The initial
token's metadata ID equals its client ID; new initial provisioning follows the
same convention. Later tokens receive new UUIDs. IDs are identifiers, never
credentials. A revoked record is retained, is never reactivated, and cannot be
used to reconstruct the bearer. Only a new random token can restore access.

## Authentication and revocation semantics

Each authentication reads `client_tokens` by SHA-256 digest and accepts only an
unrevoked record. There is no application token cache. Requests that authenticate
after a revocation commits return 401 with that credential. A request whose
authentication snapshot precedes the commit may finish; revocation does not cancel
in-flight requests. Existing events and outbound worker jobs remain authorized
work, and signing keys are unaffected. Revoking API access is not a client-wide
kill switch or a way to cancel deliveries.

The API returns the same 401 for unknown and revoked tokens. Foreign clients
remain isolated. An API outage or database query failure remains a 503 rather
than successful or cached authentication.

## Transactions, concurrency and uncertain results

Client provisioning atomically inserts the client and its first token. Issuance
and revocation lock the client row, serializing changes for that client. Two
nullable, unique active slots enforce the cap in PostgreSQL as well: a revoked
record has no active slot, allowing another token to occupy that slot while the
old metadata remains. Database CHECK constraints explicitly reject an active row
with a NULL slot; SQL NULL logic must not bypass this invariant.

Only SHA-256 hashes of 256-bit random bearers are stored. Tokens and digests are
never returned by metadata listing or included in application error logs. A failed
issuance/provisioning commit returns empty credentials. A rolled-back revocation
leaves the old token active; a lost commit response has an uncertain outcome, so
inspect metadata or repeat revocation. Storage errors are sanitized before CLI logs.

Issuance is not idempotent and plaintext cannot be recovered. If the CLI response
is lost, inspect metadata before retrying: the new active record may already
exist. Revoke the undisclosed token by ID, then issue another. Two active tokens
block further issuance until one is explicitly revoked. If an initial
`create-client` response is lost, inspect client records administratively before
creating a replacement client. Repeated revocation is safe after an uncertain
response. None of these operations rotates destination signing secrets.

## Validation and code study

`make test-integration` exercises old-token migration, both active tokens, the
hard cap under concurrent requests, revocation/issuance races, recovery after all
tokens are revoked, missing/foreign metadata IDs, transaction failures, canceled
issuance, bounded safe metadata and HTTP access/idempotency through rotation.
Tests use only isolated PostgreSQL databases with race detection and log privacy.

`make test-worker-process` also exercises the real administrative binary's issue,
list, revoke and recovery commands, then checks worker process behavior. Synthetic
plaintext is discarded through pipes with Docker logging disabled; only metadata
IDs are retained. All containers/database data are temporary and cleaned up.

Read `internal/storage/tokens.go`, migration 009, and `cmd/relay/tokens.go` together.
They show why revocation is a persisted state change, why a token is disclosed only
after commit, and why the client UUID survives credential replacement.

Validation on 2026-09-14: both commands passed, including race detection, vet,
PostgreSQL log privacy and real worker crash/outage recovery. Migration 009 was
applied only to disposable test databases; the test stack was removed afterward.
