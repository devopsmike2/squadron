# Pipeline Health (v0.31)

Squadron's Pipeline Health surface answers the operational question
SRE teams ask first: **"are my collectors actually delivering data?"**

It runs entirely off the OpenTelemetry Collector's built-in
self-metrics (the `otelcol_*` family) — no extra agents, no
sidecars, no scraping infrastructure. If you can already point your
collectors at Squadron's OpAMP server, you can already see pipeline
health.

## What it shows

For every collector reporting to Squadron, you get:

- A **verdict** — `healthy`, `degraded`, `broken`, or `unknown` —
  derived from threshold rules over the latest self-metric samples.
- A **signal list** explaining what's wrong (queue 92% full,
  send_failed > 0, processor dropping points, etc.).
- The **latest values** of every captured metric, grouped by
  exporter / receiver / processor / process.

At the fleet level, you get a stacked-bar summary on the Dashboard
showing the count in each bucket plus the top five worst-offender
agent IDs.

## How to set it up

Add the `prometheus/internal` receiver to your collector config so
the collector scrapes its own metrics, then point it at Squadron's
OTLP endpoint. The standard pattern:

```yaml
receivers:
  prometheus/internal:
    config:
      scrape_configs:
        - job_name: otelcol
          scrape_interval: 30s
          static_configs:
            - targets: ["127.0.0.1:8888"]

  # ... your other receivers (otlp, hostmetrics, etc.) ...

processors:
  batch: {}

exporters:
  otlp/squadron:
    endpoint: squadron.example.com:4317
    tls:
      insecure: false

service:
  telemetry:
    metrics:
      address: 127.0.0.1:8888       # the prometheus/internal target
  pipelines:
    metrics/self:
      receivers: [prometheus/internal]
      processors: [batch]
      exporters: [otlp/squadron]
    # ... your user pipelines ...
```

That's it. Squadron's ingest pipeline detects the `otelcol_*` metric
prefix in the incoming OTLP batch, extracts those samples into the
dedicated `pipeline_health_samples` table, and serves them through
the new endpoints below. Your user telemetry continues to flow into
`metrics_sum` / `metrics_gauge` unchanged — pipeline health is a
sibling surface, not a replacement.

## The verdict rules

`internal/pipelinehealth/verdict.go` implements three rules. They're
deliberately coarse — the goal is to flag obvious problems, not
nitpick steady-state noise.

**Queue saturation** (`otelcol_exporter_queue_size` /
`otelcol_exporter_queue_capacity`). A ratio ≥ 50% emits a warn
signal and bumps the verdict to `degraded`; ≥ 90% emits critical
and bumps to `broken`. This is the cleanest "destination is too
slow" signal.

**Send failures** (`otelcol_exporter_send_failed_*` > 0). Any
non-zero value emits a warn signal and bumps to `degraded`. The
collector reports cumulative counters, so a non-zero value means
failures have occurred at some point in the agent's lifetime — not
necessarily right now. v0.32+ adds a rate-over-time evaluator to
the alert layer for that distinction.

**Processor drops** (`otelcol_processor_dropped_*` > 0). Same shape
as send failures — non-zero counter → `degraded`. Drops usually
mean a filter processor or a memory_limiter is throwing data away.

The worst-severity signal drives the overall verdict. Verdicts
display in this order: critical signals first, then warn, then
healthy.

## API

All three endpoints require `ScopeAgentsRead`.

`GET /api/v1/pipeline-health/fleet` — fleet-wide bucketed counts +
per-agent verdict map. The Dashboard's `<FleetHealthSummary/>`
component reads this.

`GET /api/v1/pipeline-health/agents/:agentID` — per-agent snapshot
with verdict, signals, and the latest values of every captured
metric. The Agent Details drawer's pipeline-health panel reads
this.

`GET /api/v1/pipeline-health/agents/:agentID/timeseries?metric=...`
— 1-minute bucketed sparkline for a single metric on one agent.
`labels=key=value;key=value` filters to a specific exporter /
receiver / processor. Optional `window` parameter (default 1h).

## Storage

The `pipeline_health_samples` table lives in DuckDB alongside the
other telemetry tables. Schema:

```sql
CREATE TABLE pipeline_health_samples (
    timestamp    TIMESTAMP NOT NULL,
    agent_id     VARCHAR NOT NULL,
    metric_name  VARCHAR NOT NULL,
    labels_json  JSON,
    labels_hash  VARCHAR NOT NULL,
    value        DOUBLE NOT NULL,
    unit         VARCHAR
);
```

The natural key is `(agent_id, metric_name, labels_hash)`. The
labels hash is a sha256/16 prefix of the sorted (key=value) pairs
in the labels map — stable across requests, collision-free at
realistic scales.

Retention follows the same configuration as the rest of the
telemetry store. The table is small (a few rows per agent per
self-metric reporting interval, default 10s) so even six months of
data at fleet scales of low-thousands stays well under a GiB.

## What's captured vs. what's not

The extractor's allow-list (in `internal/pipelinehealth/extractor.go`)
is intentionally small. It covers the metrics the verdict rules
actually consume:

- Receiver throughput + refusal counts
- Exporter throughput + send-failed + enqueue-failed counts
- Exporter queue size + capacity
- Processor dropped + refused counts
- Process uptime + memory + cpu

Other `otelcol_*` metrics (compression ratio, per-receiver
per-format counters, scope-instrumentation internals) **still land
in the regular `metrics_*` tables** — you can still query them via
SquadronQL. We just don't store a second copy of them in the
pipeline-health table, and they don't influence the verdict.

## Roadmap

v0.32+ extends pipeline health with:

- Webhook + silent-agent alerts driven by verdict transitions.
- A timeseries-based send-failed rate that fires `broken` instead
  of `degraded` when failures are continuous.
- Inventory reconciliation (GHA pipeline pushes its target host
  list; the dashboard surfaces missing collectors).

See `docs/operating.md` for the broader operability story.
