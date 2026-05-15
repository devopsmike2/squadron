# Self-monitoring

Squadron can emit its own state changes as OpenTelemetry traces to a
configurable OTLP endpoint — typically the same observability stack
you're already using Squadron to manage the OTel collectors for. The
dogfood version of "telemetry control plane".

- [Why](#why)
- [Turning it on](#turning-it-on)
- [What gets emitted](#what-gets-emitted)
- [Attribute schema](#attribute-schema)
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
- **Traces of in-flight operations.** Today's spans are point events
  derived from audit entries. A future patch will add proper bracketing
  spans for rollout lifecycles (one parent span per rollout, child
  spans per stage) so you can see how long each stage took.
- **OTel logs.** The logs SDK was stabilizing as of late 2024; once
  it's solid we'll add a parallel log emitter so operators who prefer
  log search over trace search can use either.
- **Trace context propagation from incoming API calls.** A future patch
  will pick up the W3C `traceparent` header on `/api/v1/*` calls so a
  rollout created by `squadronctl` appears under the CLI's trace, not
  a new root.
