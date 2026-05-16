# Self-monitoring

Squadron can emit its own state changes as OpenTelemetry traces to a
configurable OTLP endpoint — typically the same observability stack
you're already using Squadron to manage the OTel collectors for. The
dogfood version of "telemetry control plane".

- [Why](#why)
- [Turning it on](#turning-it-on)
- [What gets emitted](#what-gets-emitted)
- [Attribute schema](#attribute-schema)
- [Rollout traces](#rollout-traces)
- [Examples](#examples)
- [What's NOT exported (yet)](#whats-not-exported-yet)

## Why

The audit log in `/api/v1/audit/events` is the durable record of every
state change Squadron makes. Self-monitoring fans the same events out
as OTel spans so they land in your existing trace search tool (Tempo,
Jaeger, SigNoz, Honeycomb, Datadog OTLP — anything that speaks OTLP).
Operators get to query Squadron's activity alongside everything else
they observe.

- "Show me every rollout that aborted in the last week."
- "What did `operator:alice@example.com` change yesterday?"
- "Trace `target_id=<rollout-id>` — every stage, every state
  transition, every audit entry, in one place."

The audit log stays source-of-truth in SQLite. OTel export is
best-effort — if the destination is unreachable, the durable record
is unaffected.

## Turning it on

In `squadron.yaml`:

```yaml
telemetry:
  enabled: true
  service_name: squadron  # optional; default 'squadron'
  otlp:
    endpoint: otel-collector.example.com:4317
    protocol: grpc        # grpc | http
    insecure: false       # set true for local dev over plain TCP
    headers:
      # Most managed OTLP backends require a bearer or API key.
      authorization: "Bearer <your-token>"
```

Restart Squadron. The startup log line confirms the configuration:

```
INFO  selftel: OTLP trace export enabled  endpoint=otel-collector:4317 protocol=grpc service_name=squadron
```

If `enabled: true` but `endpoint` is empty, Squadron refuses to start —
the misconfiguration would silently drop every export.

## What gets emitted

Every audit event becomes one span:

- **agent.registered, agent.drift.drifted, agent.drift.synced**
- **config.stored, config.applied**
- **rule.created, rule.updated, rule.deleted**
- **alert.fired, alert.resolved**
- **rollout.created, rollout.stage_applied, rollout.empty_canary,
  rollout.paused, rollout.resumed, rollout.aborted, rollout.rolled_back,
  rollout.succeeded**

The full list lives in [audit-log.md](./audit-log.md#whats-recorded).
Anything that lands in the audit log lands as a span.

Spans are **point-event-shaped** — start time equals end time (plus the
SDK's monotonic-clock noise). Trace UIs render them as flat events
rather than nested durations. That matches what audit entries actually
are: instantaneous state changes, not bracketing operations.

## Attribute schema

Every span carries the same canonical attributes:

| Attribute              | Source                       | Example                        |
|------------------------|------------------------------|--------------------------------|
| `squadron.actor`       | audit event `actor`          | `operator:ci-bot`, `system`    |
| `squadron.event_type`  | audit event `event_type`     | `rollout.created`              |
| `squadron.target_type` | audit event `target_type`    | `rollout`, `agent`, `config`   |
| `squadron.target_id`   | audit event `target_id`      | `<uuid>`                       |
| `squadron.action`      | audit event `action`         | `created`, `aborted`           |
| `squadron.payload.<k>` | primitive payload keys       | `squadron.payload.stage_count = 3` |

Non-primitive payload fields (maps, slices) are deliberately dropped —
they blow up trace UI cardinality and don't filter cleanly. The full
payload is always retained in the SQLite audit log.

Span name is the `event_type` (`rollout.created`,
`alert.fired`, etc.) so trace-search by name maps directly onto the
audit event vocabulary.

## Rollout traces

Beyond the per-event spans, Squadron emits a **bracketing span tree
for each rollout** so operators can see how long each stage took and
what failed. The tree shape:

```
rollout.<rollout-name>                      [parent, spans the whole rollout]
├── rollout.stage_applied  (stage_index=0)  [child, brackets stage 0]
├── rollout.stage_applied  (stage_index=1)
├── rollout.stage_applied  (stage_index=2)
└── (events on the parent: aborted, rollback_started, empty_canary)
```

The parent span ends with status:

- `Ok` when the rollout succeeded.
- `Error` (with the abort reason as the status message) when the
  rollout was rolled back or aborted. Trace UIs render these red.

### Rollout span attributes

| Attribute                                  | Where                  | Example                |
|--------------------------------------------|------------------------|------------------------|
| `squadron.target_type`                     | parent + stage         | `rollout`              |
| `squadron.target_id`                       | parent + stage         | `<rollout-uuid>`       |
| `squadron.rollout.name`                    | parent + stage         | `ship-v2`              |
| `squadron.rollout.group_id`                | parent + stage         | `<group-uuid>`         |
| `squadron.rollout.target_config_id`        | parent + stage         | `<config-uuid>`        |
| `squadron.rollout.total_stages`            | parent + stage         | `3`                    |
| `squadron.rollout.stage_index`             | stage                  | `0` `1` `2`            |
| `squadron.rollout.canary_size`             | stage                  | resolved agent count   |
| `squadron.rollout.stage.dwell_seconds`     | stage                  | `120`                  |
| `squadron.rollout.stage.mode`              | stage                  | `percent` or `label`   |
| `squadron.rollout.stage.percentage`        | stage (percent mode)   | `10`                   |
| `squadron.rollout.stage.label_selector`    | stage (label mode)     | `region=us-east,role=canary` (sorted) |
| `squadron.rollout.terminal_state`          | parent (final)         | `succeeded`, `rolled_back`, `aborted` |
| `squadron.rollout.reason` (on event)       | parent events          | abort reason text      |

Span events on the parent span carry the transition narrative:

| Event name           | When                                                  |
|----------------------|-------------------------------------------------------|
| `empty_canary`       | A stage resolved to zero agents (likely selector typo). |
| `aborted`            | Engine flipped state to aborted; rollback push pending. |
| `rollback_started`   | Engine began pushing the previous config back.        |

### Restart recovery

When Squadron restarts mid-rollout, the OTel span doesn't carry
across processes — the new Squadron opens a fresh span when it picks
up the in-progress rollout on its next tick. The recovered span will
be missing the early stages' history but the rest of the lifecycle
gets traced. Document this in your runbook if your operators rely on
trace continuity across deploys.

### Engine shutdown

`engine.Stop` flushes any in-flight rollout spans before exiting.
Truncated spans end with status `Error` and message `engine.shutdown`,
so they're visible in the trace UI rather than silently dropped. The
rollout itself isn't aborted — on next start the engine resumes from
the persisted state and opens a new span.

## Examples

### Self-host: feed into an OTel Collector

```yaml
# squadron.yaml
telemetry:
  enabled: true
  otlp:
    endpoint: otel-collector:4317
    protocol: grpc
    insecure: true   # internal network only

# otel-collector.yaml
receivers:
  otlp:
    protocols:
      grpc: { endpoint: 0.0.0.0:4317 }
exporters:
  # Whatever your stack uses — Tempo, Jaeger, Datadog, etc.
  otlphttp:
    endpoint: http://tempo:4318
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlphttp]
```

### Honeycomb / Datadog / SigNoz Cloud

Hosted OTLP endpoints typically use HTTP + an API key header:

```yaml
telemetry:
  enabled: true
  otlp:
    endpoint: https://api.honeycomb.io
    protocol: http
    headers:
      x-honeycomb-team: "<your-api-key>"
```

Squadron's spans will appear under the service name `squadron`
(override via `telemetry.service_name`).

### Searching

In your trace tool, filter by `service.name = squadron` to scope to
the control-plane events. Then narrow further by any attribute:

- `squadron.event_type = rollout.aborted` — every aborted rollout.
- `squadron.actor != system` — operator-attributed events only.
- `squadron.target_id = <rollout-id>` — full event stream for one
  rollout.

## What's NOT exported (yet)

- **Metrics.** Squadron's `/metrics` endpoint is the primary Prometheus
  scrape path; an OTel-metrics bridge would duplicate the
  surface area without adding much value. Planned for a future patch.
- **Bracketing spans for non-rollout operations.** v0.13 added the
  rollout span tree. Similar treatment for alert evaluations,
  long-running config pushes, and OpAMP connection lifecycles is
  planned but not yet shipped.
- **OTel logs.** The logs SDK was stabilizing as of late 2024; once
  it's solid we'll add a parallel log emitter so operators who prefer
  log search over trace search can use either.
- **Trace context propagation from incoming API calls.** A future patch
  will pick up the W3C `traceparent` header on `/api/v1/*` calls so a
  rollout created by `squadronctl` appears under the CLI's trace, not
  a new root. Today every rollout span starts a fresh trace.
- **Pause / resume events on the rollout span.** They appear as
  point-event spans via the v0.12 audit-event publisher (filterable
  by `squadron.event_type = rollout.paused`), but they're not yet
  woven into the rollout's bracketing span as events. Bounded
  follow-up.
