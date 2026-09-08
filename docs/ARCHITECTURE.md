# Architecture

Relay will accept events and deliver signed webhooks to registered destinations. This is the intended design, not current functionality.

```text
Authenticated client -> API -> PostgreSQL: events + deliveries
                                   |
                                 worker -> subscriber HTTP endpoint
                                   |
                              attempt history
```

Use one Go codebase, eventually with `api`, `worker`, and migration commands. PostgreSQL will own durable state and job claiming. Start with a PostgreSQL queue rather than adding a broker. API and worker can then be deployed and sized independently.

An event is accepted only after its database transaction commits. Delivery is at least once: a receiver may process a request before the worker crashes or loses its response. Stable event IDs allow consumers to deduplicate. Do not promise exactly-once delivery.

Implement transaction-safe job claiming, expiring leases, bounded HTTP timeouts, bounded response reads, retry backoff with jitter, maximum attempts and an explicit replay operation. Store attempt outcomes without retaining secrets. Use HMAC signatures with timestamps and document receiver verification and replay protection.

Validate outbound destinations against SSRF, including resolution and redirects, before enabling delivery. Authenticate clients and enforce destination ownership before exposing ingestion publicly. Synthetic receiver tests must not weaken the production policy.

Kubernetes manages processes and deployment; it does not supply these delivery guarantees. Worker shutdown must finish or release claimed work within the termination grace period. Readiness will need to reflect necessary dependencies once storage is implemented; liveness should not fail merely because PostgreSQL is temporarily unavailable.

Future integration: Vigil may submit incident events and FinPulse may submit alerts. Both integrations require explicit application changes. Relay will use its own database, credentials and volumes.
