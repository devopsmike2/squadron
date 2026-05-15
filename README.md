# Squadron

**The control plane for your OpenTelemetry fleet.**

Squadron is an open-source platform for managing OpenTelemetry collectors at
scale. It speaks OpAMP to your agents, ingests their telemetry over OTLP, and
ships a built-in UI for fleet management, configuration, and observability —
all from a single self-hosted binary.

> Squadron is a fork of and derivative work based on
> [Lawrence OSS](https://github.com/getlawrence/lawrence-oss), licensed under
> Apache 2.0. See [`NOTICE`](NOTICE) for full upstream attribution.

## Why Squadron

Running an OpenTelemetry fleet at scale is currently a do-it-yourself problem.
You end up gluing together collector configs by hand, deploying via your own
pipeline, and flying blind on what each agent is actually doing.

Squadron handles the operational layer:

- **Remote agent management** over OpAMP — push configs, restart collectors,
  organize agents into groups, detect drift.
- **Built-in telemetry backend** — collectors send their own telemetry to
  Squadron over OTLP, so you can see how the fleet is performing without
  standing up a separate observability stack.
- **Query and explore** — Squadron QL plus a topology view for traces, metrics,
  and logs.
- **Single binary, no dependencies** — runs as a Go binary or Docker container
  with embedded SQLite + DuckDB. Drop it on a box and you have an OTel control
  plane.

## Project status

Squadron is in active development. The OSS core under Apache 2.0 is free for
any size fleet and self-hostable. A commercial Enterprise tier (advanced agent
management, SSO/RBAC, alerting, priority support) is on the roadmap; a hosted
Squadron Cloud will follow.

## Getting started

### Docker

```bash
docker pull ghcr.io/devopsmike2/squadron:latest

docker run -d \
  --name squadron \
  -p 8080:8080 \
  -p 4320:4320 \
  -p 4317:4317 \
  -p 4318:4318 \
  -v squadron-data:/data \
  ghcr.io/devopsmike2/squadron:latest

open http://localhost:8080
```

### Docker Compose

```bash
git clone https://github.com/devopsmike2/squadron.git
cd squadron
docker compose up -d squadron
open http://localhost:8080
```

### Connect a collector

Point your OpenTelemetry collector at Squadron's OpAMP endpoint and OTLP
receiver:

```yaml
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://localhost:4320/v1/opamp

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
```

Start your collector and it will register with Squadron automatically.

## Documentation

Full documentation lives under [`/docs`](./docs/README.md):

- [Getting started](./docs/getting-started.md) — install Squadron, connect
  a collector, push your first config.
- [Concepts](./docs/concepts.md) — agents, groups, configs, drift.
- [Rollouts](./docs/rollouts.md) — staged deploys with canary selection,
  auto-abort, preview/diff, recipes, templates.
- [Alerts](./docs/alerts.md) — threshold rules over telemetry and fleet
  state, with webhooks.
- [Audit log](./docs/audit-log.md) — every state change, filterable.
- [Authentication](./docs/auth.md) — Bearer tokens, bootstrap flow,
  token lifecycle.
- [squadronctl CLI](./docs/squadronctl.md) — command-line client for
  CI pipelines and scripting.
- [Operating Squadron](./docs/operating.md) — env vars, prod checklist,
  backup, upgrade notes.
- [API reference](./docs/api-reference.md) — REST endpoints with curl
  examples.

## Architecture

Squadron runs as a single process composed of:

- **OpAMP server** (port 4320) — manages collectors via WebSocket, distributes
  configurations, tracks status and capabilities.
- **OTLP receiver** (ports 4317/4318) — accepts traces, metrics, and logs over
  gRPC and HTTP. Receivers hand raw bytes to a bounded worker pool for parsing,
  enrichment, and storage.
- **Storage layer** — SQLite for application data (agents, groups, configs),
  DuckDB for raw telemetry and rollups. The factory pattern makes the storage
  backends pluggable.
- **REST API** (port 8080) — Gin-based JSON API plus the Squadron QL query
  endpoint, with a Prometheus `/metrics` endpoint and SPA-served UI.
- **Web UI** — React/Vite frontend for agent management, configuration
  editing, telemetry exploration, and topology visualization.

## Development

The development container runs the Go backend with hot reload via
[Air](https://github.com/air-verse/air) and the Vite dev server side by side.

```bash
docker compose up -d
docker compose logs -f squadron

# UI dev server on http://localhost:5173, API on http://localhost:8080
```

Local without Docker (requires Go 1.24+, GCC/G++, SQLite dev libraries):

```bash
go install github.com/air-verse/air@latest
make dev
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the full contribution guide.

## License

Apache 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
