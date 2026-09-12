# Outbound delivery security — milestone 2a

## Status and boundary

`internal/delivery` implements bounded HTTPS attempts and signing/verification.
The API never calls it. The explicit [one-attempt worker](DELIVERY_ATTEMPTS.md)
now uses this transport with durable destination secrets; the lease-only diagnostic
still never sends HTTP. There is no scheduler or automatic retry.

Run `make test-integration`: delivery tests use local TLS fixtures only, including
the full ingestion/worker/persistence path. No production policy bypass is exposed.

## Connection policy (implemented)

- Only HTTPS on port 443; reject credentials, fragments, zone IDs, ambiguous
  numeric hosts, single-label names and invalid DNS labels. IDNs require ASCII
  punycode. URLs and event bodies are limited to 2048 bytes and 64 KiB respectively.
- Resolve absolute DNS names for each attempt. Reject missing answers and reject
  the entire answer set when any address is disallowed, including mixed A/AAAA.
- IPv4 excludes special-purpose, private, loopback, link-local, shared/CGNAT,
  documentation, benchmarking, multicast and reserved ranges. IPv4-mapped IPv6
  is evaluated as its embedded IPv4 address. IPv6 allows ordinary global unicast
  within `2000::/3`, excluding special protocol, documentation and transition
  ranges. IPv6 zones and NAT64 well-known translation prefixes are blocked.
- Dial the first validated IP directly. Keep the original hostname for TLS
  certificate verification and SNI. No second DNS lookup occurs during connection.
  There is no address fallback in one attempt; later worker retries will re-resolve.
- No environment proxy, redirects, connection reuse or body replay. An attempt
  returns the original 3xx status without following its Location header.
- TLS verification stays enabled, with TLS 1.2 minimum. Deadline: five seconds for
  DNS through response body; TCP connect, TLS handshake and response-header phases
  each have a two-second bound (also constrained by the overall deadline).
- Response headers are capped at 8 KiB; read/discard at most 16 KiB plus one byte
  to detect overflow, including chunked bodies. Automatic decompression is off.
  No receiver body is returned, persisted or logged by this package.

`Outcome.StatusCode` is diagnostic metadata, not a success guarantee. An attempt
succeeds only when its error is nil **and** its status is 2xx. A read failure after
a 2xx is ambiguous; receiver-side deduplication remains necessary.

The address policy is intentionally conservative: it also blocks some special
addresses marked globally reachable. It was checked against the IANA
[IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry/) and
[IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry/) special-purpose
registries on 2026-09-09. Review registry changes before public deployment. These
application checks assume normal IP routing; unusual host routing or custom NAT64
prefixes require a separate deployment/egress review.

Destination registration remains format-only for now. A registered URL is not a
promise that the outbound policy will allow it. The sending worker applies this
policy on every attempt and records a safe `destination` failure code. An
owner-facing attempt-history endpoint remains planned.

## Signing contract (implemented)

Each destination gets its own 32-byte random secret through explicit provisioning. `Secret` keeps bytes
private, redacts fmt output and requires an explicit `Export` for provisioning.
The exported encoding is unpadded URL-safe base64. Neither `Secret` nor `Sender`
writes logs or retains an exported key. Go does not guarantee memory zeroization;
these protections prevent accidental serialization, not process-memory access.

Request headers:

```text
Relay-Event-ID: <stable event UUID>
Relay-Signature: t=<Unix seconds>,v1=<lowercase hex HMAC-SHA256>
```

Sign this exact byte sequence, with literal dots as separators:

```text
ASCII(timestamp) + "." + ASCII(event_id) + "." + exact HTTP body bytes
```

The sender takes a body snapshot for both signing and sending. JSON must not be
reformatted by a receiver before verification. `Verify` uses constant-time MAC
comparison and rejects timestamps outside a ±300-second window, malformed headers
and extra signature components. The event ID is signed to prevent an attacker
from relabeling a captured payload and bypassing receiver deduplication.

Receivers must verify the signature, then atomically deduplicate the event ID with
their business operation. A valid signature alone does not stop replay within the
time window. Each future retry gets a fresh timestamp but the same event ID.

## Secret lifecycle (implemented separately)

[Milestone 2b](SIGNING_SECRETS.md) implements encrypted persistence, one-time owner
disclosure, staged activation, rotation/revocation and master-key canaries. The guide
documents independent key backups and the remaining restore drill. The sending
worker reads the active secret under the destination lock and commits that version
with attempt start. It never falls back to retired/revoked keys. Rotation/revocation
after start cannot cancel a credential already held by an in-flight process.

## Tests and Go study path

- `policy.go`: `netip.Addr`, prefix matching and IPv4 unmapping; private ranges are
  not the only non-public ranges.
- `sender.go`: `context` deadlines, a custom `http.Transport`, dialing a validated
  IP while retaining TLS identity, and a bounded `io.LimitReader`.
- `signature.go`: opaque value types, explicit secret export, HMAC and constant-time
  comparison. The independent known vector checks the wire contract.
- `sender_test.go`: local TLS certificates, injected DNS/dial dependencies and
  behavioral tests for rebinding, mixed DNS answers, redirects, timeouts, proxy
  variables, certificate rejection and response limits. There is no production
  switch to permit private addresses or skip TLS verification.
