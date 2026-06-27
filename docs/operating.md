# Operating Squadron

What you need to know to run Squadron somewhere that matters.

- [Configuration](#configuration)
- [Ports](#ports)
- [Data and persistence](#data-and-persistence)
- [Production checklist](#production-checklist)
- [Backup and restore](#backup-and-restore)
- [Upgrading](#upgrading)
- [Observability](#observability)

## Configuration

Squadron reads its config from a YAML file. The default
[`squadron.yaml`](../squadron.yaml) in the repo ships sane defaults. Point
at a different file with `--config /path/to/squadron.yaml` or
`SQUADRON_CONFIG=/path/...`.

Most fields can also be overridden by environment variables — viper maps
nested keys to underscore-separated upper-case names (e.g.
`server.http_port` ⇄ `SERVER_HTTP_PORT`).

A few env vars sit outside the viper-mapped tree because they're
standalone secrets, not config fields:

- `SQUADRON_CONFIG` — path to the YAML config file (overrides
  `--config`).
- `SQUADRON_SECRETS_KEY` — 32-byte AES-GCM key Squadron uses to
  seal cloud credentials and PATs at rest. Required when
  discovery features are enabled; auto-generated to
  `~/.squadron/secrets-key` if missing on a single-node
  deployment.
- `SQUADRON_GITHUB_WEBHOOK_SECRET` — HMAC secret for the
  `POST /api/v1/webhooks/github` listener that records
  `recommendation.pr_merged` audit events when Squadron-opened
  PRs land. The handler reads this once at startup and caches
  it. If empty, the route still mounts but responds with 503 +
  a humanized "secret not configured" message rather than a
  silent no-op. See
  [webhook-listener.md](./webhook-listener.md) for the full
  setup walkthrough.
- `SQUADRON_DISCOVERY_CROSS_CLOUD_CITATIONS` — opt-in (default
  off). When set to `true`, the discovery proposer's
  verdict-learning loop pools a small, capped, origin-labeled set
  of recent decline/merge verdicts from OTHER cloud scopes, so a
  decline recorded on (say) AWS can be cited on a later GCP
  recommendation that shares the pattern (`recommendation_kind`).
  Off preserves strict per-provider verdict isolation. Bounded:
  at most a couple of cross-cloud citations per block, each
  labeled `[seen on <provider> / <scope>]`, and still gated by the
  connection's existing learn opt-out.
- `SQUADRON_DISCOVERY_SCAN_INTERVAL` — opt-in (default off). A Go
  duration (e.g. `6h`) that turns on the continuous-discovery
  scheduler: Squadron re-runs + persists AWS discovery scans for
  every connected account on this cadence, so scan history accrues
  automatically. Unset / `<=0` keeps scans on-demand only. NOTE:
  auto-scanning real cloud accounts on a timer has cost + API-rate
  implications — set it deliberately. Values below 15m are raised to
  the 15m floor. Covers all four clouds (AWS/GCP/Azure/OCI); the
  first sweep fires after one interval (not at startup). After each scheduled
  scan Squadron diffs it against the previous one and, on any change, records a
  `discovery.scan_drift_detected` audit event (forwards via SIEM like any audit
  event) — proactive drift without polling. The event payload carries the
  change totals, capped added/removed resource id lists, and
  instrumentation_regressions (resources whose OTel turned off).
- `SQUADRON_DISCOVERY_DRIFT_COOLDOWN` — opt-in (default off). A Go duration
  that caps how often a single scope emits a drift event. Useful when scanning
  frequently but wanting fewer drift alerts (e.g. scan every 15m, alert at most
  hourly). Unset / <=0 means every changed sweep emits.
- `SQUADRON_DISABLE_AUTH` — dev-only override that bypasses
  Bearer token enforcement. Do NOT set this in production.

Sections in `squadron.yaml`:

```yaml
server:
  http_port: 8080      # UI + REST API
  opamp_port: 4320     # OpAMP WebSocket

otlp:
  grpc_endpoint: 0.0.0.0:4317
  http_endpoint: 0.0.0.0:4318
  # What address Squadron offers to agents for their own self-telemetry.
  # Empty = derive from grpc_endpoint with 0.0.0.0 → localhost.
  agent_grpc_endpoint: ""
  agent_http_endpoint: "squadron:4318"

storage:
  app:
    type: sqlite
    path: ./data/app.db
  telemetry:
    type: duckdb
    path: ./data/telemetry.db

retention:
  raw_metrics: 24h
  raw_logs: 24h
  rollups_1m: 7d
  rollups_5m: 30d

rollups:
  enabled: true
  interval_1m: "*/1 * * * *"
  interval_5m: "*/5 * * * *"

logging:
  level: info     # debug | info | warn | error
  format: json    # json | console

worker:
  queue_size: 10000
  workers: 3
  timeout: 5s
```

## Ports

| Port  | Protocol           | Purpose                                 |
|-------|--------------------|-----------------------------------------|
| 8080  | HTTP               | UI + REST API. Also serves `/metrics`.  |
| 4320  | WebSocket          | OpAMP server.                           |
| 4317  | gRPC               | OTLP receiver.                          |
| 4318  | HTTP               | OTLP receiver.                          |

In Docker Compose, all four are exposed by default. In production, run
Squadron behind a reverse proxy and only expose the ports you need
externally (typically 8080 + 4320 + one OTLP port).

## Data and persistence

Two databases under `storage.*.path`:

- **`app.db` (SQLite).** Agents, groups, configs, rollouts, alert rules,
  audit events. Small (< 100 MB for most fleets), critical to back up.
- **`telemetry.db` (DuckDB).** Raw traces/metrics/logs plus 1m and 5m
  rollups. Can be very large depending on traffic and retention; expect
  to size accordingly.

Both files are written in-place. Squadron does not tolerate the data
directory being mounted on a non-POSIX-compliant filesystem (some FUSE
mounts, network drives without flush guarantees). Use a local volume.

The Docker image runs as UID 1001 and writes its data dir to
`/app/data` (SQLite + DuckDB stores and the auto-generated
`secrets.key`). Mount a volume there:

```bash
docker run -d \
  -v squadron-data:/app/data \
  ghcr.io/devopsmike2/squadron:latest
```

## Production checklist

Before pointing real traffic at Squadron:

- [ ] **Enable API auth.** Set `auth.enabled: true` in `squadron.yaml`.
      On first start Squadron emits a bootstrap token to stderr; copy
      it, sign in to the UI, create properly-labeled tokens, and revoke
      the bootstrap one. See [Authentication](./auth.md). If you'd
      rather use OIDC/SSO, front Squadron with a reverse proxy that
      enforces auth and leave the in-app auth off — both layers
      compose.
- [ ] **Persistent volume on a local filesystem.** See above.
- [ ] **Retention budget.** Audit + telemetry data grows. Decide your
      retention windows for raw metrics/logs (default 24h is fine for
      small fleets; tune up for forensics, down for cost).
- [ ] **Webhook URLs for alerts and rollouts.** Squadron's notifications
      fire-and-forget; pair them with a destination that's actually
      monitored (Slack, PagerDuty webhook, your incident bot).
- [ ] **`SQUADRON_GITHUB_WEBHOOK_SECRET` set if you use the IaC
      PR-merged listener.** v0.89.23 added an inbound webhook route
      that records `recommendation.pr_merged` audit events when
      Squadron-opened PRs land. Without the secret env var set, the
      route mounts but returns 503 on every delivery — operators
      see the failure in the GitHub repo's Recent Deliveries log
      rather than a silent no-op. Generate with
      `openssl rand -hex 32`. Full setup in
      [webhook-listener.md](./webhook-listener.md).
- [ ] **Scrape `/metrics`.** Squadron exposes its own Prometheus metrics.
      At minimum, watch `squadron_opamp_connected_agents`,
      `squadron_rollouts_in_progress`, and the worker pool queue depth.
- [ ] **Cap the number of in-flight rollouts.** Squadron will happily
      run many concurrent rollouts but the engine evaluates all of them
      every 5s. Hundreds is fine; thousands is untested.
- [ ] **Read the [audit log](./audit-log.md) when something is weird.**
      90% of "Squadron did something I didn't expect" is answerable from
      a 10-second audit query.

## Backup and restore

The application database is a single SQLite file. Hot-copy it with
`sqlite3 .backup`:

```bash
sqlite3 /data/app.db ".backup '/backup/squadron-app-$(date +%F).db'"
```

The telemetry database is similar; DuckDB supports a `COPY DATABASE`
statement for online backups:

```bash
duckdb /data/telemetry.db "EXPORT DATABASE '/backup/squadron-telemetry-$(date +%F)' (FORMAT PARQUET);"
```

There's no first-class "Squadron backup" feature today. The simplest
disaster-recovery plan:

1. Cron the two commands above nightly. Ship the artifacts off-host.
2. To restore: stop Squadron, replace `app.db` and `telemetry.db` in the
   data directory, start Squadron.

A managed `/api/v1/admin/backup` endpoint is on the roadmap.

## Upgrading

Squadron does in-place upgrades. The release process is:

1. Pull the new image.
2. Stop the running container.
3. Start the new image against the same volume.

The application schema is migrated automatically on startup. Audit log
entries from older versions remain readable. Telemetry rollups created
under older schemas continue to work.

Always read the release notes before upgrading. Breaking changes (rare)
are flagged in the GitHub release body.

## Observability

Squadron is observable about itself:

- **Prometheus metrics** at `GET /metrics`. The metrics are scoped under
  `squadron_*` so they're easy to identify on a shared Prometheus.
- **Audit log** at `GET /api/v1/audit/events` — see
  [Audit log](./audit-log.md).
- **Structured logs** to stdout in JSON by default. Each log line has a
  level, timestamp, and contextual fields (`rollout_id`, `agent_id`, etc.)
  that make grep-based debugging tolerable.
- **SSE event stream** at `GET /api/v1/events`. The UI uses this for live
  updates; you can subscribe directly for custom dashboards or alerting.

Squadron can also emit its own state changes as OpenTelemetry traces
into your existing observability stack — see
[Self-monitoring](./self-monitoring.md).
