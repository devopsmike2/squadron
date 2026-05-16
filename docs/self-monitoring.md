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
- [Alert evaluation traces](#alert-evaluation-traces)
- [Config push traces](#config-push-traces)
- [OpAMP connection traces](#opamp-connection-traces)
- [Tracing across the agent boundary](#tracing-across-the-agent-boundary)
- [Tracing API requests](#tracing-api-requests)
- [Metrics](#metrics)
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
| `paused`             | Operator paused the rollout (via API / UI / CLI).      |
| `resumed`            | Operator resumed a paused rollout.                    |
| `aborted`            | Auto-abort fired OR operator aborted; rollback pending. |
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

## Alert evaluation traces

Each alert rule evaluation cycle produces a span named
`alert.evaluate`. The evaluator opens the span when it starts the
Squadron QL query, records the observed value and fired/!fired
state after the threshold comparison, and closes it once dispatch
completes.

A fired alert is still a **successful evaluation** from the tracer's
perspective — the QL query worked, the threshold was applied, the
rule yielded its result. Status only flips to `Error` when the QL
query itself errored. Operators filter:

- `squadron.fired = true` — all firing evaluations.
- `status = Error` — query failures (parse errors, telemetry-store
  problems, etc.).

### Attribute schema

| Attribute                       | Notes                                    |
|---------------------------------|------------------------------------------|
| `squadron.target_type`          | always `rule`                            |
| `squadron.target_id`            | rule ID                                  |
| `squadron.rule_id`              | rule ID (duplicate of target_id for filtering ergonomics) |
| `squadron.rule_name`            | human-readable rule name                 |
| `squadron.rule.query`           | the Squadron QL query that ran           |
| `squadron.operator`             | threshold operator (`>`, `>=`, `<`, `<=`, `==`, `!=`) |
| `squadron.threshold`            | configured threshold value (float)       |
| `squadron.rule.severity`        | rule severity (`info` / `warning` / `critical`) |
| `squadron.observed_value`       | the scalar the QL query returned         |
| `squadron.fired`                | true if the threshold tripped this eval  |

### Span events

| Event name              | When                                              |
|-------------------------|---------------------------------------------------|
| `dispatched_to_webhook` | Firing/resolved payload POSTed to a webhook URL.  |
|                         | Attributes: `squadron.webhook.url`, `squadron.webhook.state` |

## Config push traces

Every per-agent OpAMP config push produces a span named
`config.push`. The span brackets the synchronous SendConfigToAgent
call: the ack (or timeout / agent-not-found / no-capability) lands
as a span event before the span closes.

### Attribute schema

| Attribute                  | Notes                                                  |
|----------------------------|--------------------------------------------------------|
| `squadron.target_type`     | always `agent`                                         |
| `squadron.target_id`       | agent UUID                                             |
| `squadron.agent_id`        | agent UUID (duplicate of target_id for filtering)      |
| `squadron.config_id`       | config row ID being pushed                             |
| `squadron.group_id`        | group ID — omitted entirely for single-agent direct pushes |
| `squadron.push_source`     | what triggered the push: `rollout` / `direct` / `group` / `drift_remediation` (reserved) |

### Span events

| Event name   | When                                              |
|--------------|---------------------------------------------------|
| `opamp_ack`  | Agent confirmed it applied the config.            |
| `opamp_nack` | Agent rejected or timed out. Attributes: `squadron.nack_reason` |

### Status semantics

- `Ok` on `opamp_ack`.
- `Error` on `opamp_nack` with the nack reason as the status message.

### Filtering

```
service.name = squadron AND name = config.push
  AND squadron.push_source = rollout      # all rollout-driven pushes
  AND status = Error                       # only failed ones
```

## OpAMP connection traces

Each connected agent gets a long-lived span named
`opamp.agent_connection` spanning the lifetime of its OpAMP
connection. The span opens on the first inbound message (the
earliest point Squadron knows the agent's instance ID), closes on
disconnect.

### Attribute schema

| Attribute                                | Notes                                       |
|------------------------------------------|---------------------------------------------|
| `squadron.target_type`                   | always `agent`                              |
| `squadron.target_id`                     | agent instance UUID                         |
| `squadron.agent_id`                      | agent instance UUID                         |
| `squadron.agent_version`                 | reported in the first AgentDescription. Omitted if the agent never reports one. |
| `squadron.disconnect_reason`             | set at End: `client_disconnected`, `server_shutdown`, or a protocol-error string |
| `squadron.connection_duration_seconds`   | float — derived at End from the span's wall-clock start time |

### Status semantics

- `Ok` for clean disconnects (`client_disconnected`, `normal`, empty).
- `Error` for everything else (`server_shutdown`, protocol errors).

### Restart caveat

When Squadron restarts, every still-open connection span is
flushed by `Server.Stop` with reason `server_shutdown`. The agents
themselves get the OpAMP close frame and reconnect against the new
process; a fresh span opens for each one. Span identity therefore
doesn't carry across restarts. Document this in your runbook if
your operators rely on connection-span continuity across deploys.

## Tracing across the agent boundary

The trace propagation story doesn't stop at the API edge. When
Squadron pushes a config to an agent — via the rollout engine, the
direct-push API handler, or (eventually) the drift-remediation loop —
the W3C TraceContext for the originating operation rides along on
the OpAMP message. An OTel-instrumented agent can extract it and
parent its own apply-side spans under Squadron's trace, so a single
trace tells the full story: caller → Squadron API → engine → agent.

### What rides on the wire

The OpAMP spec doesn't (yet) define a standard capability for
cross-boundary trace propagation. Until it does, Squadron uses a
Squadron-defined custom capability attached to the same
`ServerToAgent` frame that carries the `RemoteConfig`:

| Field        | Value                                                    |
|--------------|----------------------------------------------------------|
| `Capability` | `io.squadron.traceparent.v1`                             |
| `Type`       | `context`                                                |
| `Data`       | JSON: `{"traceparent": "...", "tracestate": "..."}`      |

The `traceparent` value is exactly what the W3C TraceContext
propagator would put into an HTTP `traceparent` header (`00-<trace-id>-<span-id>-<flags>`).
`tracestate` is included only when the source context carries one.

Per the OpAMP spec, agents that don't recognize a custom capability
ignore it. The injection is therefore fully backward-compatible —
old agents see the same wire frame they always saw.

### When the field is attached

Only when the calling context has a valid active OTel span. Common
sources:

- A `rollouts.stage.apply` span (rollout engine push during a stage
  apply or rollback)
- A `config.push` span (direct-push handler, future drift-remediation
  loop)

Squadron deliberately omits the CustomMessage when no active span is
present rather than emitting a sentinel all-zeros traceparent. The
W3C spec treats `00-00000000000000000000000000000000-0000000000000000-00`
as syntactically valid, and naive consumers might adopt it as a
parent — that would create phantom traces rooted in a non-existent
span. Better to send nothing.

### What we don't send

The selftel publisher installs a composite propagator
(`TraceContext` + `Baggage`). Squadron extracts only the W3C
trace-context headers for the OpAMP payload — baggage entries are
dropped. Baggage typically carries operator-private context (tenant
id, deploy version, request-scoped feature flags), and shipping it
to every agent in the fleet on every push is unnecessary noise.
Operators who do want baggage propagation can patch
`internal/opamp/traceparent.go` to include the full carrier; we'll
revisit if there's demand.

### Consumption sketch for an OTel-aware agent

Agent-side, an OpAMP collector with a custom-message hook can look
for the `io.squadron.traceparent.v1` capability, decode the JSON
payload into an `http.Header`-style map, and feed it to the
propagator's `Extract`:

```go
// Inside the agent's OpAMP custom-message handler:
if msg.Capability == "io.squadron.traceparent.v1" && msg.Type == "context" {
    var headers map[string]string
    if err := json.Unmarshal(msg.Data, &headers); err == nil {
        carrier := propagation.MapCarrier(headers)
        applyCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
        // Use applyCtx as the parent for the agent's apply-side span(s).
        _, span := tracer.Start(applyCtx, "agent.apply_config",
            trace.WithSpanKind(trace.SpanKindServer),
        )
        defer span.End()
        // ... rest of the apply path ...
    }
}
```

When the OpAMP spec defines a standard capability for this, Squadron
will emit both for one release, then switch over and deprecate the
`io.squadron.*` form.

## Tracing API requests

Squadron's API server participates in W3C
[Trace Context](https://www.w3.org/TR/trace-context/) propagation.
When a client (`squadronctl`, a CI pipeline, the in-browser UI) sends
a `traceparent` header on `POST /api/v1/rollouts` or any other API
call, Squadron's server span becomes a child of the caller's span —
the request shows up under the caller's trace in your observability
tool rather than starting a fresh root.

This is automatic — turning on `telemetry.enabled` also installs the
W3C TraceContext + Baggage propagator globally, and the API server
mounts the `otelgin` middleware on every route.

### How rollouts connect to the originating request

`POST /api/v1/rollouts` opens a server span for the request. The
service-layer `Create` captures that span context and stores it for
the rollout engine. When the engine eventually picks up the pending
rollout (typically seconds later) and opens its bracketing parent
span, that span is created with an OTel **span link** pointing at
the API request's span context.

Trace UIs that support links (Tempo, Jaeger, Honeycomb, Datadog) let
operators jump from the API request trace to the rollout trace and
back. The link carries `squadron.link = created_by_request` so it's
distinguishable from any future link types.

We use a link rather than a true parent-child relationship because:

- The rollout span lives across many engine ticks — often minutes,
  sometimes hours. The API span ended seconds after the request
  returned.
- Nesting a long-lived span under a short-lived one breaks trace UIs
  that compute durations from start to end timestamps.
- Span links are the OTel-blessed primitive for "related but not
  parent-child", exactly this case.

### Example: tracing `squadronctl rollout create`

```bash
# squadronctl doesn't yet inject traceparent on its own (planned),
# but you can wrap it with anything that does — e.g. otel-cli:
otel-cli exec --service-name ci-deploy --name "deploy v2.3" -- \
  squadronctl rollout create \
    --group prod-collectors \
    --target-config $CONFIG \
    --template standard-percent-ramp \
    --wait
```

In your trace UI:

- The `ci-deploy` root span shows the full operation.
- A `POST /api/v1/rollouts` child span shows the API request.
- A `rollout.deploy-v2.3` linked span shows the engine's bracketing
  rollout trace — kept as a separate trace tree because it lives
  beyond the API request.

## Metrics

As of v0.17, turning on `telemetry.enabled` also bridges Squadron's
Prometheus `/metrics` surface to OTLP metrics on the same endpoint
that receives the traces. Every collector that backs `/metrics` —
API request counters, OpAMP connection gauges, OTLP receiver
histograms, drift status gauges, alert evaluation counters, worker
pool retries / dead letters — shows up on the OTLP side without any
per-collector rewiring.

The intent is to give operators on OTel-only observability stacks
(no Prometheus scrape configured) the same fleet view that
Prometheus operators get from `/metrics`. Operators who already run
a Prometheus scrape against `/metrics` can keep doing so — both
exports run in parallel, drawing from the same in-memory registry.

### How it works

The bridge is the contrib package
[`go.opentelemetry.io/contrib/bridges/prometheus`][bridge] used as
an OTel metric.Producer. Selftel wraps the registry that backs
`/metrics`, hands it to a PeriodicReader on a configurable scrape
interval, and feeds the reader into an OTLP metric exporter that
shares the trace exporter's endpoint, protocol, headers, and
insecure flag.

[bridge]: https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/prometheus

The pipeline runs entirely in-process — no second scrape against
`/metrics`. The bridge reads the underlying `prometheus.Gatherer`
state directly on each Reader cycle. That means the export cadence
is independent of any external Prometheus scrape; if you want
matching cadences, set `telemetry.metric_interval` to whatever your
Prometheus `scrape_interval` is (most operators run 15s or 30s).

### Naming + label translation

Prometheus metric names and label keys pass through verbatim. A
counter named `api_requests_total` with label `component=api` lands
on the OTLP side as a Sum (monotonic) named `api_requests_total`
with an `api_requests_total{component="api"}` attribute set. There
is no `squadron.` prefix transformation — operators querying the
OTLP side will see the same names as in `/metrics`.

Type mapping:

| Prometheus type    | OTLP type                    |
|--------------------|------------------------------|
| Counter / CounterVec | Sum (monotonic, cumulative)  |
| Gauge / GaugeVec     | Gauge                        |
| Histogram            | Histogram (explicit buckets) |
| Summary              | Summary (quantiles preserved) |

### Configuration

```yaml
telemetry:
  enabled: true
  service_name: squadron
  metric_interval: 30s    # optional; default 30s
  otlp:
    endpoint: otel-collector.example.com:4317
    protocol: grpc
    headers:
      authorization: "Bearer <your-token>"
```

The same endpoint serves both traces and metrics, so a single OTLP
destination receives the full self-telemetry picture.

### What about /metrics?

`/metrics` keeps working unchanged. Operators with established
Prometheus scrapes don't need to touch their config. The bridge is
purely additive: if you run only Prometheus, ignore this section;
if you run only OTel, the bridge gives you parity; if you run
both, both work.

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

As of v0.18 the full four-tier story is in place:

    caller (CI / squadronctl) → Squadron API → engine → agent

Every link carries W3C TraceContext when the relevant runners are
configured, and the Prometheus `/metrics` surface bridges to OTLP
metrics on the same endpoint as the traces. Nothing major remains
on the self-monitoring backlog. Future work is around polish (more
attributes on specific spans, agent-side reference implementations
for the `io.squadron.traceparent.v1` capability, etc.) rather than
new pipelines.
