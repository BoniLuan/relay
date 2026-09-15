# Telegram notifications

Active since 2026-09-15: private Alertmanager v0.34.0 connected to shared
Prometheus. Configuration, routing, readiness, discovery and all five scrape
targets passed validation. **Live firing/resolved receipt was confirmed by the
operator on 2026-09-15**, with two Telegram notifications and zero failures.

## Setup

Create a bot with BotFather and add it as a channel administrator with posting
permission. Run from the repository directory:

```bash
python3 scripts/telegram-chat-id.py @your_channel
python3 scripts/setup-telegram.py --channel-id=YOUR_NUMERIC_CHANNEL_ID
```

Enter the token only at the hidden prompt. Setup verifies posting permission and
asks for destination confirmation. Credentials are stored outside Git in
`.local/telegram` (0700; files 0600), never overwritten. The helpers reject
redirects and password-prompt echo fallback, and send no messages. For a private
chat, send `/start` to the bot and run `make telegram-setup`.

## Deploy and verify

```bash
make notifications-start
make monitoring-prepare-telegram
```

Validate the generated config with `promtool` and compare it with the previous
config and current Vigil source. Recreate only Prometheus, preserving its volume:

```bash
docker compose --env-file /home/luan/.config/vigil/observability.env \
  -f /home/luan/projects/vigil/deploy/observability/compose.yaml \
  -f /home/luan/projects/relay/deploy/monitoring/shared.override.yaml \
  up -d --no-deps --force-recreate prometheus
```

Verify private Prometheus `/api/v1/alertmanagers`, Alertmanager `/-/ready`, all
five scrape targets and the `vigil-prometheus-data` mount. Always regenerate with
`make monitoring-prepare-telegram` to retain notifications. Never use
`--remove-orphans` or `down -v` on shared services.

Acceptance completed using an explicitly marked synthetic alert with automatic
expiry after two minutes. Both FIRING and RESOLVED reached the channel; all five
scrape targets remained healthy. No shared service was interrupted.

## Routing and operations

- Only the four allowlisted Relay alerts with `service=relay`,
  `job=relay-metrics` and warning/critical severity reach Telegram. Other alerts
  are discarded by this router; existing shared destinations are preserved.
- Messages contain alert name, severity, status and counts, without payloads,
  annotations, credentials or tenant identifiers. Resolved messages are enabled.
- Grouping: alert name/severity/test marker; initial wait 30s, group interval 5m, repeat 4h.
  Delivery depends on Telegram availability and valid credentials; inspect
  notification failures privately.
- Only `vigil-monitoring` is attached; no host port, database access or keyring.
  The unauthenticated API trusts peers on this network.
- State persists in `.local/alertmanager-data` (0700, invoking UID/GID), with
  120h retention and bounded container logs. Restart policy: unless-stopped.
  Relay's development DB/exporter still require manual restart after reboot.

## Rollback

Run `make monitoring-prepare` without Telegram, validate and recreate only
Prometheus using the same override. Relay scraping remains enabled. Then run
`make notifications-stop`; retain credentials, state and all volumes.

See [observability](OBSERVABILITY.md) and the
[Alertmanager configuration reference](https://prometheus.io/docs/alerting/latest/configuration/).

## Synthetic verification

Use the existing Relay alert labels plus `notification_test=true` when injecting
an authorized test into the private Alertmanager API. The template adds
`[TEST - no service outage]`; the grouping label keeps tests separate from real
alerts. Always set a short explicit `endsAt` so a disconnected operator cannot
leave the test firing indefinitely. Do not change real service availability.

With the current settings, expect firing after approximately 30 seconds and
resolved at the next five-minute group interval. Check the Telegram notification
and failure counters before/after, and confirm both messages in the channel.
API acceptance and successful notification counters do not establish human receipt.
