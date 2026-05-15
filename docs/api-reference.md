# API reference

Squadron exposes a JSON REST API on port 8080 (configurable via
`server.http_port`). This page is a tour with curl examples; the
authoritative source is the Go handlers under
`internal/api/handlers/`.

All endpoints are unauthenticated today. Run Squadron behind a trust
boundary — see [Operating Squadron](./operating.md#production-checklist).

- [Agents](#agents)
- [Groups](#groups)
- [Configs](#configs)
- [Config tooling](#config-tooling)
- [Rollouts](#rollouts)
- [Alerts](#alerts)
- [Audit](#audit)
- [Events (SSE)](#events-sse)

## Agents

| Method | Path                                | Purpose                                |
|--------|-------------------------------------|----------------------------------------|
| GET    | `/api/v1/agents`                    | List with `?group_id=`, `?status=`, `?drift=` |
| GET    | `/api/v1/agents/:id`                | Get one                                |
| GET    | `/api/v1/agents/stats`              | Fleet counts                           |
| PATCH  | `/api/v1/agents/:id/group`          | Assign to group. Body `{group_id: ""}` |
| POST   | `/api/v1/agents/:id/config`         | Push raw YAML config to a single agent |
| POST   | `/api/v1/agents/:id/restart`        | Send restart command over OpAMP        |

```bash
curl 'http://localhost:8080/api/v1/agents?status=online&limit=50'
```

## Groups

| Method | Path                       | Purpose       |
|--------|----------------------------|---------------|
| GET    | `/api/v1/groups`           | List          |
| POST   | `/api/v1/groups`           | Create        |
| GET    | `/api/v1/groups/:id`       | Get one       |
| DELETE | `/api/v1/groups/:id`       | Delete        |

## Configs

| Method | Path                             | Purpose                                  |
|--------|----------------------------------|------------------------------------------|
| GET    | `/api/v1/configs`                | List with `?agent_id=`, `?group_id=`     |
| POST   | `/api/v1/configs`                | Create                                   |
| GET    | `/api/v1/configs/:id`            | Get one                                  |
| GET    | `/api/v1/configs/latest`         | Latest config for an agent or group      |

## Config tooling

| Method | Path                                | Purpose                                                                |
|--------|-------------------------------------|------------------------------------------------------------------------|
| POST   | `/api/v1/configs/lint`              | Lint a YAML body. Returns findings.                                    |
| GET    | `/api/v1/configs/schemas`           | OTel collector JSON schemas for the YAML editor's completion engine.   |
| GET    | `/api/v1/configs/templates`         | Curated YAML snippet library.                                          |
| GET    | `/api/v1/configs/templates/:id`     | Single template body.                                                  |

```bash
curl -X POST http://localhost:8080/api/v1/configs/lint \
  -H 'Content-Type: application/json' \
  -d '{"content": "receivers: {}\nexporters: {}\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]"}'
```

## Rollouts

See [Rollouts → API reference](./rollouts.md#api-reference) for the
full list. Quick summary:

| Method | Path                                                    |
|--------|---------------------------------------------------------|
| GET    | `/api/v1/rollouts`                                      |
| POST   | `/api/v1/rollouts`                                      |
| GET    | `/api/v1/rollouts/:id`                                  |
| POST   | `/api/v1/rollouts/:id/pause`                            |
| POST   | `/api/v1/rollouts/:id/resume`                           |
| POST   | `/api/v1/rollouts/:id/abort`                            |
| GET    | `/api/v1/rollout-preview?group_id=&target_config_id=`   |
| GET    | `/api/v1/rollout-recipes/abort-criteria`                |
| GET    | `/api/v1/rollout-recipes/templates`                     |

## Alerts

See [Alerts → API reference](./alerts.md#api-reference). Endpoints under
`/api/v1/alerts/rules` for CRUD.

## Audit

| Method | Path                       | Purpose                                                         |
|--------|----------------------------|-----------------------------------------------------------------|
| GET    | `/api/v1/audit/events`     | List with `?target_type=`, `?target_id=`, `?since=`, `?limit=`  |

## Events (SSE)

| Method | Path                       | Purpose                                                |
|--------|----------------------------|--------------------------------------------------------|
| GET    | `/api/v1/events`           | Server-Sent Events stream — agent state, drift, rollout transitions, audit. |

Example consumer with curl:

```bash
curl -N http://localhost:8080/api/v1/events
# event: rollout.state_changed
# data: {"rollout_id":"...","state":"in_progress","current_stage":1,"transition":"stage_applied"}
```

Each event has a `type` (matching the audit `event_type` namespace where
applicable) and a JSON `data` body. The UI's `EventSubscriber`
revalidates SWR caches off these events for live updates.
