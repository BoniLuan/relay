# Relay

A small Go service for durable webhook event ingestion, built as a backend engineering portfolio project.

**Implemented:** bearer authentication, client-owned HTTPS destination registration,
PostgreSQL migrations, atomic event + pending delivery persistence, client-scoped
idempotency, authenticated event lookup, health endpoints and graceful shutdown.
Destination signing secrets support encrypted storage, staged activation, rotation
and revocation; see [the lifecycle guide](docs/SIGNING_SECRETS.md).
**Implemented, opt-in:** one signed HTTPS attempt with durable outcome and safe
unknown-result recovery and [bounded persisted retries](docs/RETRIES.md). Run it explicitly with `make deliver-once`; see
[delivery attempts](docs/DELIVERY_ATTEMPTS.md). The lease-only diagnostic remains
available through `make worker` and [queue leases](docs/QUEUE_LEASES.md).
**Implemented, opt-in:** a [continuous worker](docs/CONTINUOUS_WORKER.md) with
bounded polling, shutdown and destination cooldown. Start it with `make worker-start`.
**Implemented:** [owner-scoped per-event attempt history](docs/DELIVERY_HISTORY.md),
including safe outcomes and retry schedules.
**Implemented:** [paginated owner-scoped delivery listing](docs/DELIVERY_LIST.md)
with status/destination filters and bounded keyset pages.
**Implemented:** [one controlled replay per failed event](docs/REPLAY.md), with
idempotent authorization, preserved history and up to three additional starts.
**Implemented:** [administrative token rotation and revocation](docs/CLIENT_TOKENS.md),
with up to two active credentials per client and one-time disclosure after commit.
**Implemented:** [durable per-client request rate limits](docs/RATE_LIMIT.md),
with a shared 120-request fixed minute window and HTTP 429 / Retry-After.
**Implemented:** [per-client capacity quotas and usage](docs/QUOTAS.md):
20 destinations, 1,000 retained events and 100 open deliveries.
**Implemented:** [administrative operational metric snapshots](docs/METRICS.md)
in Prometheus text format (`make metrics`).
**Implemented, opt-in:** [private metrics exporter, monitoring configuration,
alert rules and an isolated recovery demo](docs/OBSERVABILITY.md). Shared monitoring
activation is an explicit deployment step; the development DB/exporter integration
was activated on this VPS on 2026-09-15 (see the deployment record in the guide).
**Implemented:** [Telegram alerts](docs/NOTIFICATIONS.md) with confirmed firing/recovery,
and an [isolated backup/restore drill](docs/BACKUP_RESTORE.md) including master keys.
**Implemented, opt-in:** [30-day history and idempotency retention](docs/RETENTION.md),
with bounded administrative previews and explicit cleanup.
**Not implemented:** public API deployment and scheduled off-host backups.

The English [project site](https://relay.boniluan.com) presents the implementation
and illustrative delivery flows. It exposes no backend API; see the
[site deployment guide](docs/SITE.md).

See [implementation status](docs/STATUS.md) for the completed/pending checklist
and [roadmap](docs/ROADMAP.md) for milestone acceptance criteria.

## Run locally with Docker Compose

Read `/home/luan/projects/INFRASTRUCTURE.md` on the shared VPS first. Relay uses
only its own network/database/volume; the API binds to `127.0.0.1:18081` and the
database publishes no host port. Kubernetes is optional and not needed here.

```bash
cp .env.example .env
# Replace the placeholder in .env with the output of: openssl rand -hex 24
chmod 600 .env
make keyring
make db
make migrate
make register-keyring
make client NAME=local
# Save the returned token securely: this is its only display.
make up
curl --fail http://127.0.0.1:18081/readyz
```

`make migrate` builds the application and runs the migration explicitly. Repeating
it is safe. The API verifies the registered keyring before listening and never
applies migrations. Readiness checks PostgreSQL, schema version 11 and the loaded
keyring; `/livez` checks only the HTTP process.
`make client` is a local administrative operation with database access, not a
public registration route. Tokens have 256 random bits; only SHA-256 hashes are
stored. Use the [token lifecycle guide](docs/CLIENT_TOKENS.md) to issue a replacement,
verify it, and revoke the old token without changing the client identity.

For API examples and exact status/idempotency rules, see [the API contract](docs/API.md).
For the implementation and study path, see [architecture](docs/ARCHITECTURE.md).

```bash
make test-integration   # Isolated tmpfs PostgreSQL, HTTP tests, race detector, vet
make test-worker-process # Separate run: real worker crash and shutdown recovery
make test-restore       # Isolated dump/restore, master keys and signed delivery
make down               # Stops only Relay development; preserves its DB volume
```

The integration command uses project `relay-test`, no host ports and no development
credentials. It removes its containers/network afterward. It needs Docker and may
download Go modules. Tests inject transaction failures into this disposable database.
Do not point them at development or production. Package execution is sequential
because one suite installs a temporary database trigger to force commit failure.

With Go 1.27 installed, `make test` and `make vet` run local checks; database tests
skip unless the dedicated test environment is configured. The host does not need
Go when using `make test-integration`. Native `make run` requires a Relay-only
`RELAY_DATABASE_URL` and a private `RELAY_KEYRING_FILE`; the provided Compose
database deliberately has no host binding.
`RELAY_HTTP_ADDR` defaults to `127.0.0.1:18081` for native execution.

## Structure

```text
cmd/relay/                    API, admin and opt-in worker commands
internal/httpapi/             authentication, validation, HTTP contract tests
internal/storage/             SQL, transactions and real PostgreSQL tests
internal/delivery/            outbound security/signing primitives and TLS tests
internal/secrets/             AES-GCM keyring and private-file loading
internal/worker/              lease diagnostic, attempt orchestration and continuous polling
internal/storage/migrations/  embedded, explicit versioned SQL
compose.yaml                  persistent development environment
compose.test.yaml             disposable integration environment
docs/ROADMAP.md               phased milestones and acceptance criteria
deploy/kubernetes/            historical scaffold manifests; not current deployment
```

No public hostname or shared proxy connection is configured. Both worker modes
require explicit commands; `make up` starts only API/database. The existing
Kubernetes examples need database/secret planning before they can run this version;
cluster lifecycle belongs to [platform-lab](https://github.com/BoniLuan/platform-lab).
Do not use the scaffold manifests as a production deployment.

The active transport policy is documented in [delivery security](docs/DELIVERY_SECURITY.md).
The API never sends webhooks itself; the separate sending command commits attempt
metadata before network I/O and records the outcome afterward.
