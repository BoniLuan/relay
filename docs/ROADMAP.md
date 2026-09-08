# Roadmap

## 0 — Scaffold (implemented)

- Runnable standard-library HTTP server with graceful termination and health routes.
- Loopback-only Compose environment and small Kubernetes lab manifests.
- No background service started automatically by scaffolding.

## 1 — Understand deployment

Optional deployment learning runs independently in [platform-lab](https://github.com/BoniLuan/platform-lab). Follow [Relay’s deployment guide](KUBERNETES.md) to load its image into the shared cluster, inspect a Pod and Service, replace a Pod, and inspect probes. Remove Relay’s namespace when done; cluster lifecycle belongs to the platform repository. Explain each manifest field before adding abstractions. Application development can proceed without Kubernetes.

## 2 — Durable event ingestion

Specify the event/destination API, authentication and ownership model, then add PostgreSQL, explicit migrations, validation and idempotency keys. Test duplicate submissions and transaction failures. Allocate a new database port only if host access is required and update the shared registry.

## 3 — Reliable delivery

Add a worker with durable job claims, leases, HMAC signing, SSRF-safe requests, bounded retries and attempt history. Test receiver errors, timeouts, worker crashes and duplicate delivery. Add a synthetic receiver fixture with a deliberate local-test policy.

## 4 — Operate and demonstrate

Add delivery metrics, traceable event IDs, replay commands, backup/restore instructions and a reproducible failure demo. Introduce a separate worker Deployment, configuration and Secrets; learn migration Jobs and persistent storage using disposable data.

## 5 — Public portfolio deployment

Choose Compose or a separately planned Kubernetes hosting environment based on experience. Define DNS/TLS routing, authentication, quotas and backups. Demonstrate integration with one existing project. Keep the lab disposable and production state separate.
