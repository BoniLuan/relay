# Signing secret lifecycle — milestone 2b

Authenticated owners can stage, activate, inspect metadata and revoke destination
signing keys. Migration 002 adds encrypted versioned secrets and master-key
verification records; migration 001 stays unchanged. Existing destinations receive
no automatic key. An opt-in [lease diagnostic](QUEUE_LEASES.md) exists but never reads signing keys
or sends events. A separate [single-attempt command](DELIVERY_ATTEMPTS.md) now
uses the keys. Retries are now [persistently scheduled](RETRIES.md); the [continuous worker](CONTINUOUS_WORKER.md)
is explicitly started with `make worker-start`.

## Development setup

```bash
# First-time setup; preserve existing credentials when upgrading.
cp .env.example .env
# Replace the database password with newly generated hex in .env.
chmod 600 .env
make keyring             # Creates .local/keyring.json, mode 0600; never overwrites
make db
make migrate             # Explicit migration to current schema (version 10)
make register-keyring    # Explicit registration/verification of master keys
make client NAME=local
make up
```

When upgrading, create the keyring only if none exists, back up the database, then
run `make migrate`, `make register-keyring`, `make up`. The API verifies the
registered keyring before listening. Missing files, permissive file permissions,
wrong keys or missing registered keys fail closed. Readiness rechecks loaded keys
against encrypted database canaries. File changes require process recreation;
deleting a file does not erase keys already loaded in memory.

Compose mounts `.local/keyring.json` read-only at `/run/relay/keyring.json`. `make`
passes the invoking user's UID/GID so mode 0600 works without root or world-readable
keys. Before direct `docker compose` commands, export `RELAY_RUN_UID=$(id -u)` and
`RELAY_RUN_GID=$(id -g)`. Native execution needs `RELAY_KEYRING_FILE` and Relay's
own `RELAY_DATABASE_URL`. No database port or shared network is added.

## Owner API

All operations require bearer authentication. Let `BASE` be
`http://127.0.0.1:18081/api/v1/destinations/DESTINATION_ID/signing-secrets`.

| Method and path | Result |
| --- | --- |
| `POST BASE`, JSON `{}` | 201: `version`, `state=staged`, `created_at`, one-time `secret` |
| `GET BASE` | 200: `secrets` array with version/state/created_at only |
| `POST BASE/VERSION/activate`, JSON `{}` | 204: activates staged version, retires old active version |
| `DELETE BASE/VERSION` | 204: revokes staged, active or retired version |

Foreign destinations/versions return 404. A second staged version returns 409.
Activating an already active version and revoking an already revoked version are
idempotent. Retired/revoked versions cannot be reactivated. Responses use
`Cache-Control: no-store`. Metadata reads never disclose ciphertext or plaintext.

Staging discloses the secret only after commit. It cannot be fetched again. If the
response is lost, list metadata, revoke the staged version, then generate another.
A 503 does not prove a commit failed. Destination row locks and partial unique
indexes permit at most one staged and one active version under concurrency.

```bash
# Use a private terminal; save the credential securely for the receiver.
curl --fail-with-body "$BASE" \
  -H "Authorization: Bearer $RELAY_TOKEN" -H 'Content-Type: application/json' \
  --data-binary '{}'
# Install the returned key at the receiver BEFORE activating that version:
curl --fail-with-body -X POST "$BASE/1/activate" \
  -H "Authorization: Bearer $RELAY_TOKEN" -H 'Content-Type: application/json' \
  --data-binary '{}'
```

## Encryption and rotation

Signing secrets have 256 random bits. AES-256-GCM uses a fresh random nonce stored
before the ciphertext. Associated data binds format version, client ID, destination
ID, signing-secret version and master-key ID. Tampering or copying ciphertext to
another destination fails authentication. Go does not guarantee memory zeroization.

The private keyring format is:

```json
{"active":"key-1","keys":{"key-1":"UNPADDED_BASE64URL_32_BYTE_KEY"}}
```

Up to eight master keys are supported. To change the active master key, stop the
API, securely add a fresh key under a new ID while retaining all registered keys,
change `active`, run `make register-keyring`, back up the updated keyring separately,
and recreate the API. Registration verifies existing canaries before adding keys;
it cannot silently replace the bytes behind an existing key ID. New secrets use
the active master key; existing ciphertext uses its recorded key ID. Bulk
re-encryption and removal of registered master keys are not implemented.

Destination rotation is separate: stage a new version, configure the receiver,
then activate. Revocation never falls back to retired versions. The [sending worker](DELIVERY_ATTEMPTS.md) selects/decrypts the active version
under the destination lock, commits its version with attempt start and uses it
for that bounded attempt. Revocation after start cannot retract in-flight work. Retries must re-read the
active version. Rotation/revocation cannot retract bytes already sent. Receivers
need explicitly bounded overlap for ordinary rotation and immediate removal of
compromised keys for emergency revocation. Queue integration tests verify version pinning and subsequent active-key selection.

## Backup and restore

Database dumps contain sensitive event data even though signing keys are encrypted.
Protect database backups and keep the keyring in an independently protected secret
backup, never in the dump or Git. A lost master key cannot be regenerated.

For a coordinated manual development backup, first stop the continuous worker with `make worker-stop` and let all one-off
workers and administrative commands finish; do not start new ones during the backup. Use a new
private backup directory:

```bash
umask 077
export RELAY_RUN_UID=$(id -u) RELAY_RUN_GID=$(id -g)
docker compose stop relay-api
docker compose exec -T relay-db pg_dump -U relay_dev -d relay_dev -Fc > /PRIVATE/BACKUP/relay.dump
# Securely back up .local/keyring.json to a separate secret store.
# Restart only after both backups succeed:
make up
```

Run `make test-restore` for the automated [isolated drill](BACKUP_RESTORE.md).
For a real backup, restore into a separately named disposable
PostgreSQL instance with its own database/role and no shared network or host ports.
Use `pg_restore --no-owner --no-acl` against that empty database, restore the
matching keyring with mode 0600, and point Relay only at those isolated resources.
Run migrations if needed and `relay register-keyring`; verify readiness, metadata
and decryption of a known active key with an isolated test receiver. Never restore
over another application's data. Wrong/missing keys must prevent startup/decryption.

Tests cover restart, key rollover, canary mismatch, tampering, rollback and v1/v2
migration upgrades. The isolated operational drill also exercises real `pg_dump`/
`pg_restore`, both master keys, original delivery history and signed HTTPS delivery.
It does not provision durable off-host backups or a backup schedule.

## Study path

Read `internal/secrets/keyring.go` for authenticated encryption and file handling,
`internal/storage/signing.go` for locking and state transitions, and
`internal/storage/signing_test.go` for concurrency, rollback and tamper cases.
The HTTP layer exports plaintext at one narrowly defined response boundary.
