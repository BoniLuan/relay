# Relay

A small Go service for durable webhook event ingestion, built as a backend engineering portfolio project.

**Implemented:** bearer authentication, client-owned HTTPS destination registration,
PostgreSQL migrations, atomic event + pending delivery persistence, client-scoped
idempotency, authenticated event lookup, health endpoints and graceful shutdown.
**Not enabled:** outbound event delivery. Tested HTTPS/signing primitives exist in
`internal/delivery`, but no worker calls them.
**Not implemented:** durable signing secrets, retries, delivery history, replay,
client/key lifecycle management or public deployment. Pending deliveries stay pending.

## Run locally with Docker Compose

Read `/home/luan/projects/INFRASTRUCTURE.md` on the shared VPS first. Relay uses
only its own network/database/volume; the API binds to `127.0.0.1:18081` and the
database publishes no host port. Kubernetes is optional and not needed here.

```bash
cp .env.example .env
# Replace the placeholder in .env with the output of: openssl rand -hex 24
chmod 600 .env
make db
make migrate
make client NAME=local
# Save the returned token securely: this is its only display.
make up
curl --fail http://127.0.0.1:18081/readyz
```

`make migrate` builds the application and runs the migration explicitly. Repeating
it is safe. The API never applies migrations; `/readyz` returns 503 until PostgreSQL
and schema version 1 are available. `/livez` checks only the HTTP process.
`make client` is a local administrative operation with database access, not a
public registration route. Tokens have 256 random bits; only SHA-256 hashes are
stored. There is currently no token rotation/revocation command.

For API examples and exact status/idempotency rules, see [the API contract](docs/API.md).
For the implementation and study path, see [architecture](docs/ARCHITECTURE.md).

```bash
make test-integration   # Isolated tmpfs PostgreSQL, HTTP tests, race detector, vet
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
`RELAY_DATABASE_URL`; the provided Compose database deliberately has no host binding.
`RELAY_HTTP_ADDR` defaults to `127.0.0.1:18081` for native execution.

## Structure

```text
cmd/relay/                    api, migrate and create-client commands
internal/httpapi/             authentication, validation, HTTP contract tests
internal/storage/             SQL, transactions and real PostgreSQL tests
internal/delivery/            outbound security/signing primitives and TLS tests
internal/storage/migrations/  embedded, explicit versioned SQL
compose.yaml                  persistent development environment
compose.test.yaml             disposable integration environment
docs/ROADMAP.md               phased milestones and acceptance criteria
deploy/kubernetes/            historical scaffold manifests; not current deployment
```

No public hostname, shared proxy connection or worker is configured. The existing
Kubernetes examples need database/secret planning before they can run this version;
cluster lifecycle belongs to [platform-lab](https://github.com/BoniLuan/platform-lab).
Do not use the scaffold manifests as a production deployment.

The next-step implementation and its activation requirements are documented in
[delivery security](docs/DELIVERY_SECURITY.md). The primitives do not start network
requests by themselves and are not connected to event ingestion.
