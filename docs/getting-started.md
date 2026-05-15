# Getting started

This guide gets you to a running Squadron with one connected collector in
under five minutes.

- [Run Squadron](#run-squadron)
- [Connect a collector](#connect-a-collector)
- [Push your first config](#push-your-first-config)
- [What to do next](#what-to-do-next)

## Run Squadron

The easiest path is Docker. Squadron is published to GHCR as a single
container image with no external dependencies.

```bash
docker pull ghcr.io/devopsmike2/squadron:latest

docker run -d \
  --name squadron \
  -p 8080:8080 \    # UI + API
  -p 4320:4320 \    # OpAMP WebSocket
  -p 4317:4317 \    # OTLP gRPC
  -p 4318:4318 \    # OTLP HTTP
  -v squadron-data:/data \
  ghcr.io/devopsmike2/squadron:latest

open http://localhost:8080
```

You should see the Squadron UI with an empty "Agents" page.

### Docker Compose

If you'd rather drive everything from a compose file:

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
docker compose up -d squadron
open http://localhost:8080
```

The compose file in the repo also defines a development variant that runs
the Go backend with hot reload and the Vite dev server side by side. See
the main README for the dev workflow.

### Without Docker

Squadron is a Go binary. Build from source with `make build` (requires Go
1.24+, a C compiler, and SQLite dev libraries) and run `./squadron`. The
default config writes data under `./data` — change `SQUADRON_DATA_DIR` to
relocate.

## Connect a collector

Point an OpenTelemetry collector at Squadron's OpAMP endpoint (`:4320`) and
OTLP receiver (`:4317`). The minimal config looks like:

```yaml
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://localhost:4320/v1/opamp

receivers:
  hostmetrics:
    collection_interval: 30s
    scrapers:
      cpu: {}
      memory: {}

exporters:
  otlp:
    endpoint: localhost:4317
    tls:
      insecure: true

service:
  extensions: [opamp]
  telemetry:
    metrics:
      readers:
        - periodic:
            exporter:
              otlp:
                protocol: grpc
                endpoint: localhost:4317
                tls:
                  insecure: true
  pipelines:
    metrics:
      receivers: [hostmetrics]
      exporters: [otlp]
```

Start the collector. Within a few seconds the Squadron UI's Agents page will
show it as **online**.

### What just happened

1. The collector opened a WebSocket to Squadron's OpAMP server and identified
   itself with the resource attributes from its config.
2. Squadron created an `Agent` row, assigned it a UUID, and began tracking
   its drift state.
3. The collector started shipping its own self-telemetry (process metrics,
   pipeline counters) to Squadron's OTLP receiver, where it's stored in
   DuckDB for the query engine.
4. An `agent.registered` event landed in the audit log.

## Push your first config

In the UI, open the agent detail page and click **Edit config**. The
built-in YAML editor lints as you type — anti-patterns like a missing
`batch` processor or a `memory_limiter` in the wrong position get flagged
before you save.

Save. Squadron stores the new config as a versioned `Config` row, sends it
over OpAMP, and the collector applies it on the next ACK. The drift
indicator in the UI flips to **synced** when the agent reports the new
config hash back.

A few things to know about configs:

- Configs are immutable. Each save creates a new version with a content hash.
- Each agent can have an agent-specific config, or it can inherit the
  config from the group it belongs to.
- The audit log records the push as a `config.applied` event with the hash
  before and after.

## What to do next

- Group agents together so you can manage configs once and have them apply
  to many. See [Concepts → Groups](./concepts.md#groups).
- Ship a config change to a group safely instead of all-at-once. See
  [Rollouts](./rollouts.md).
- Set up an alert on drift count or error log rate. See [Alerts](./alerts.md).
- Read [Operating Squadron](./operating.md) before running this in prod.
