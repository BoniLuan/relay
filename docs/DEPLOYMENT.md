# Personal VPS deployment

## Active release (2026-09-16)

- Public API: `https://relay.boniluan.com/api/v1/`, with bearer authentication.
- Institutional page: `/`; signed synthetic receiver: `/demo/hook`.
- Core image: `relay:6475be2`; demonstration image: `relay-demo:20260916`.
- PostgreSQL migrated through 011. Existing `relay-dev` project, database and
  volume were preserved; the name remains historical, not a disposable sandbox.
- API, continuous worker, database, exporter, Alertmanager and demo receiver run
  with `restart: unless-stopped`. Tests still use separate disposable projects.
- API/worker use `relay_runtime`, without superuser, role/database creation, event
  deletion or schema-migration writes. Exporter uses read-only `relay_metrics`
  without access to signing secrets. Administrative tools retain the owner role.

The deployment combines `compose.yaml`, `compose.metrics.yaml`,
`compose.notifications.yaml` and `compose.deploy.yaml`. Image selections are in
ignored `.local/deploy.env`; database credentials stay in `.env`.

```bash
make deploy-check
make deploy-status
make deploy-up
```

These commands preserve the active overlays and selected images. `deploy-up`
starts existing releases; it does not build images or apply migrations. Root
`make up`, `make down` and individual development targets are not the VPS release
workflow: using the base Compose file alone can remove deployment networking or
restart settings. Never use `down -v` for this live database.

## Credentials and backups

The owner credential is in `.local/operator/credentials` (0600, directory 0700).
It authenticates as the personal operator client. The demonstration uses a
separate client in `.local/demo/credentials`. Read credentials locally when needed;
never put them in Git, URLs, screenshots, chat or collected command logs.

`scripts/provision-runtime-roles.py` explicitly provisions the two runtime roles
on the Relay-only database and generates their passwords in `.env`. It is
idempotent and never prints credentials. New migrations that add tables may
require reviewed grant updates; do not grant schema ownership to runtime roles.

The API/worker load `.local/keyring.json` read-only. Independent master-key copies
are in `/home/luan/.config/relay/master-key-backups/`. The receiver reads only its
own `.local/demo/secrets/signing-secret`, never the master keyring or database.
It persists ID/hash/time receipts in `.local/demo/data/receipts.json`; no payload
is retained there. All credential/state files are private.

Before migration, a custom-format database dump and rollback image tags were
saved. A second coordinated dump was taken after integration with API/worker
stopped and then restarted. Ignored `.local/deploy-current` names the deployment
record directory containing these dumps. Key backups are separate from dumps.
These are **same-VPS safety copies**, not an off-host disaster-recovery solution.
Scheduled off-host backups, secret escrow and operational restore drills of those
backups remain follow-up work; see [BACKUP_RESTORE.md](BACKUP_RESTORE.md).

## Public boundary

Boniluan's canonical `nginx/relay.conf` owns the public routes and existing TLS
certificate. Only the Relay API and receiver join `web-proxy`, with unique aliases
`relay-public-api` and `relay-demo-receiver`. PostgreSQL/worker stay off the edge;
metrics remain on the existing private monitoring network. API host port remains
loopback-only `127.0.0.1:18081`; no other host port was opened.

The proxy limits body size to 64 KiB, bounds body/header/proxy timeouts, applies
5 requests/second per source IP with burst 20 and a 10-connection limit, and returns
429 on those limits. The durable 120/minute per-client limit and capacity quotas
still apply inside Relay. Proxy retries are disabled, including for POST. Responses
are not cached and raw Relay request lines are not retained by the edge.
Critical proxy errors remain in the bounded container log. This sacrifices
edge request-level diagnostics; application logs, history and private metrics
remain available. The edge trusts only its existing Cloudflare IP ranges
for forwarded client addresses.

`/metrics`, `/readyz`, `/livez`, `/.env` and unspecified routes are not exposed.
No self-registration or administrative CLI operation is public. Use HTTPS directly;
the HTTP virtual host only redirects and serves ACME challenges.

Cloudflare returned 403 for the default Python urllib User-Agent during the live
check, but accepted `RelayIntegration/1.0`. Use an explicit descriptive User-Agent
for this integration. This is an observed edge-policy difference, not a Relay
401; Cloudflare configuration was not changed. Origin and Cloudflare TLS were
verified without disabling certificate checks.

## Verified real integration

The owner explicitly authorized synthetic events to the same-VPS receiver through
public HTTPS. Destination registration and secret activation were done privately;
the event was ingested through the public API and delivered by the actual worker
with normal DNS pinning, SSRF checks, TLS and HMAC signing. No outbound-policy test
bypass was enabled. Live checks passed:

- First accepted submission 201; exact retry 200 with the same event ID; changed
  bytes under the same ingestion key 409.
- One signed delivery attempt, HTTP 204, with a durable receiver receipt.
- After receiver restart, replaying the signed event returned 204 without changing
  its receipt; altered unsigned bytes returned 401, altered validly signed content
  with the same ID returned 409.
- Another client's event lookup returned 404; a body over 64 KiB returned 413.
- A bounded unauthenticated burst produced 401 and 429 responses.
- Restricted runtime roles completed another public signed delivery. Role checks
  confirmed no administrative flags, no event DELETE or migration UPDATE, and
  no metrics writes/signing-secret reads.
- All five monitoring targets and existing public sites remained healthy.

The receiver is an optional synthetic demonstration, not a general business
integration. It accepts at most 1,000 distinct signed event IDs and then returns
507. It serializes receipt updates, writes/renames and fsyncs state before success,
and applies the normal five-minute signature timestamp window. Unit tests cover
bad signatures, expiry, durable deduplication, storage failure, unsafe persisted
state and state-size limits with race detection.

## Future release procedure

1. Review/test the change. Build uniquely tagged images and preserve the running
   image IDs/tags; do not silently replace a tag in `.local/deploy.env`.
2. Quiesce API/worker and administrative writers. Save a coordinated PostgreSQL
   dump and independently protected keyring backup; verify the artifacts.
3. Apply migrations explicitly using the pinned new core image and the existing
   deployment Compose files. Apply reviewed runtime grants with
   `python3 scripts/provision-runtime-roles.py` and verify runtime permissions.
   Register/verify keys when required. Never create a
   new keyring to replace a lost one.
4. Update image selections and run `make deploy-up`. Check readiness locally,
   public authentication, private metrics, worker history and shared-site health.
5. For edge changes, preserve the current `boniluan-web` image, build and run
   `nginx -t` with the existing mounts, then recreate only Boniluan's `web` service.

Rollback starts by stopping new traffic/work and restoring the prior image
selection/configuration, provided it is compatible with the migrated schema.
Never restore a dump over the active database automatically: it can discard new
accepted events. Restore into fresh isolated resources and plan cutover. The edge
rollback tag for this deployment is recorded locally with prefix
`boniluan-web:before-relay-api-`; existing certificate volumes are preserved.

Retention is available but no destructive cleanup or scheduler was run during this
rollout. Inspect an explicit preview before cleanup; see [RETENTION.md](RETENTION.md).
Kubernetes remains optional and belongs to `platform-lab`.
