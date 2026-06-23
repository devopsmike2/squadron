# Trace coverage — operator guide

This is the operator-facing runbook for the v0.89.73 through
v0.89.78 trace integration arc that just closed slice 1.
Squadron now consumes its own OTLP receiver stream as a discovery
signal: the Discovery dashboard at `/discovery` gains a TRACE
COVERAGE panel showing what percentage of your inventoried
resources have actually emitted spans in the last 24 hours, and
every Inventory row (Compute, Databases, Kubernetes, across all
four clouds) gains a "Last seen" column.

The strategic frame: every recommendation Squadron made before
this arc was at the configuration-primitive layer ("enable
Container Insights"). Slice 1 of trace integration answers the
adjacent question that operators care about more: "is telemetry
actually flowing." Slice 2 will turn the visibility into
recommendation kinds; slice 1 ships the visibility.

For a first test, the walkthrough takes about 20 minutes — most
of it spent confirming your OTel collectors point at Squadron's
endpoint. For a production deployment with mature OTel coverage
already in place, the trace coverage panel populates in seconds.

## What this is good for

- A team that already has OTel collectors deployed and wants to
  know which discovered resources are actually emitting (closing
  the gap between "primitive enabled" and "spans flowing").
- A team running multi-cloud OTel and wanting one dashboard that
  reads coverage across AWS, GCP, Azure, and Oracle Cloud
  uniformly.
- An auditor who needs to correlate "Squadron flagged this
  cluster as instrumented in scan X" with "Squadron received N
  spans from that cluster in audit Y" — both signals live in the
  same audit timeline now.

## What this is NOT (slice 1)

Read this carefully. Slice 1 is intentionally narrow:

- **No recommendation generation for trace gaps.** Slice 1 SHOWS
  you that EC2 instance `i-0abc` has Container Insights enabled
  but has emitted zero spans in 24h. It does NOT yet draft a
  Terraform PR to fix the gap. Recommendation kinds in the
  `trace-emission-*` family are slice 2.
- **No span quality analysis.** Slice 1 sees spans arriving and
  counts them. It does not yet assess whether resource
  attributes are complete or whether parent-child context
  propagation is unbroken. Slice 3.
- **No metrics or logs integration.** Same OTLP receiver
  ingests metrics and logs, but the inventory correlation rules
  differ per signal type. Metrics integration is slice 2, logs
  is slice 3.
- **No cross-cloud span propagation correlation.** A span chain
  that crosses AWS Lambda then SQS then a GCP Cloud Function
  carries context across cloud boundaries; Squadron sees the
  spans but does not yet correlate the chain. Slice 4+.
- **No per-service-version detection.** When a deployment bumps
  to a new version and emission stops, Squadron does not yet
  flag the regression. Slice 2 candidate.

The [trace integration slice 1 design doc](./proposals/trace-integration-slice1.md)
§2 and §13 track slice 2+ candidates.

## How the matching works

Squadron receives spans on the existing OTLP HTTP endpoint
(port 4318) and gRPC endpoint (port 4317). Both receivers are
part of the standard `all-in-one` build — no separate setup.

For each incoming span batch, Squadron extracts the resource
attributes (`host.id`, `cloud.resource_id`,
`k8s.cluster.name`, etc.) and projects them into a
`resource_key` using a six-tier fallback chain. The discovery
side projects the SAME key from inventory snapshot fields. The
two sides join on equality.

The six tiers in priority order:

1. **`cloud.resource_id` matches verbatim.** If your OTel SDK
   sets the full ARN-shaped identifier (some SDKs do, most do
   not), Squadron keys on that. Match confidence: **strong**.
2. **`host.id` + `cloud.account.id`.** Most common AWS / GCP /
   Azure / OCI hosts emit this pair via the OTel host detector.
   Key shape: `<provider>:<account>:<host_id>`. Strong.
3. **`k8s.cluster.name` + `cloud.account.id`.** For workloads
   running on managed Kubernetes. Key shape:
   `<provider>:<account>:k8s:<cluster_name>`. Strong.
4. **`db.system` + `db.name` + `cloud.account.id`.** For
   database workloads using the OTel DB instrumentation. Key
   shape: `<provider>:<account>:db:<db_system>:<db_name>`. Strong.
5. **`host.name` alone.** Useful when no cloud-aware host
   detector is enabled. Match confidence: **weak** — the
   discovery dashboard surfaces a caveat indicator on this row.
6. **`service.name` alone.** Last-resort fallback. Operators
   should not see weak-match-via-service-name in production
   except on misconfigured exporters. Match confidence: **weak**.

When the discovery dashboard says "67% coverage with 12% weak
matches" for a provider, it means 67% of inventoried resources
have at least one span in the last 24h, and 12% of those joins
fell back to `host.name` or `service.name`. The remaining
matches were strong.

## Prerequisites

- A Squadron deployment on v0.89.78 or later.
- At least one discovered resource via one of the four cloud
  scanners (AWS / GCP / Azure / OCI). Without inventory there's
  nothing to compute coverage against.
- (Optional but valuable) An OTel-instrumented workload pointing
  at Squadron's OTLP endpoint. Slice 1 shows coverage as a gap
  view; the gap is only meaningful when there's a denominator.

The trace integration is ON by default. To disable (for example
during a load test):

```sh
export SQUADRON_TRACEINDEX_DISABLED=true
```

The receiver still accepts traces; the index just doesn't
update. Discovery dashboard's trace coverage panel reports
zero in this state.

## Step 1 — Point your collectors at Squadron's OTLP endpoint

If you're already running OTel collectors emitting elsewhere
(Tempo, Honeycomb, Datadog), add Squadron as a SECOND export
destination. Squadron does not require exclusive ownership of
your trace stream.

Sample collector config addition:

```yaml
exporters:
  otlphttp/squadron:
    endpoint: https://your-squadron-host:4318
    headers:
      Authorization: Bearer ${SQUADRON_OTLP_TOKEN}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo, otlphttp/squadron]
```

Squadron currently accepts unauthenticated OTLP requests on the
receiver port; auth tokens are roadmap work. Operators with
strict posture should put Squadron's receiver behind a TLS-
terminating reverse proxy.

## Step 2 — Run a discovery scan to populate the denominator

Open the Squadron UI, navigate to Discovery, pick one of your
connected clouds, and run a scan. The scan populates the
inventory tables that the trace coverage view joins against.

Without scans, the trace coverage panel shows "Run a discovery
scan to populate the trace coverage view."

## Step 3 — Open the Discovery dashboard

Navigate to `/discovery`. Look for the **Trace coverage** panel
below the existing instrumentation coverage ring.

The panel shows:

- **Large number**: overall percentage of inventoried resources
  with at least one span in 24h.
- **Subtext**: `N of M inventoried resources have emitted spans
  in the last 24h.`
- **Coverage ring**: green ≥ 80%, yellow 50-80%, red < 50%.
- **Per-provider chips**: small badges showing per-cloud
  coverage. Each chip is color-coded the same way.
- **Caveat indicator** (yellow icon): appears next to any chip
  where weak matches exceed 20%. Hover for explanation.

## Step 4 — Drill into a per-cloud Inventory tab

Click the "View details" link on one of the provider cards (or
navigate to `/discovery/aws`, `/discovery/gcp`, etc.). The
Inventory tab now shows a new **Last seen** column on every
sub-tab (Compute, Databases, Kubernetes).

Values:

- "2m ago" / "1h ago" / "3d ago" — relative time in compact form
- "1w ago" / "2w ago" — for older but recent emissions
- A literal date (YYYY-MM-DD) for emissions older than 30 days
- "never" — never emitted; a yellow indicator invites investigation

The "never" rows are the actionable signal. They are resources
Squadron has discovered (so the scanner can see them) but has
received zero spans from. Three common causes, in rough
prevalence order:

1. **OTel SDK not deployed.** The most common reason. The
   primitive bit (Container Insights, Cloud Logging, etc.) was
   turned on, but no SDK or collector ever got deployed to emit
   spans. Slice 2 will draft Terraform PRs to fix this; slice
   1 surfaces it for manual investigation.
2. **SDK deployed but exporter misconfigured.** Spans are
   generated but the OTLP exporter points at the wrong endpoint
   or fails auth. Check the collector logs and confirm the
   endpoint matches your Squadron deployment.
3. **Resource attribute mismatch.** Spans are flowing but the
   `host.id` / `cloud.account.id` / `host.name` they carry
   don't align with what Squadron expects. Toggle the host
   detector and cloud detector on your OTel SDK; on EC2 the
   correct detector set is `host`, `os`, `ec2`. The dashboard's
   weak-match caveat usually correlates with this failure mode.

## Step 5 — Reading the audit signal

Open the Timeline page. Filter by event type for the new trace
integration events:

- **`trace_index.background_flushed`** — fires every 30 seconds
  (default flush interval) when there are rows to write. Payload
  carries `rows_written`, `rows_evicted`, `duration_ms`,
  `interval_s`. Useful for confirming the index is healthy.
  Empty cycles do NOT emit; this keeps the timeline clean during
  low-traffic windows.
- **`discovery.trace_coverage.requested`** — fires once per
  cache miss on the `/api/v1/discovery/trace_coverage` endpoint.
  Cache TTL is 30 seconds, so a rapid burst of dashboard refreshes
  yields one audit event, not N.

No span content in either payload. Squadron's existing DuckDB
store keeps full span data; audit captures only the meta-shape.

## Step 6 — (Optional) Tune the row cap

The traceindex has a hard cap (default 100,000 rows) with LRU
eviction. This prevents a high-cardinality attribute set (a span
emitting a unique service name per request) from blowing up the
index.

To change the cap:

```sh
export SQUADRON_TRACEINDEX_MAX_ROWS=500000
```

The flush audit event's `rows_evicted` field is non-zero when
the cap is being hit; that's the signal to raise it. If
operators routinely see eviction in steady state, investigate
the cardinality of your service.name attribute first — high
service.name churn is the most common cause and usually
indicates a misconfigured exporter or auto-instrumentation
labeling pods rather than services.

## Troubleshooting matrix

| Symptom | Likely cause | Remedy |
|---|---|---|
| Trace coverage panel shows 0% for all providers | No spans received yet | Confirm collectors point at Squadron's OTLP endpoint; check `trace_index.background_flushed` audit events fire |
| Trace coverage panel shows 0% but `/v1/traces` requests are arriving | Resource attributes too sparse — slice 1 can't extract a key | Enable cloud-aware host detector in your OTel SDK (`host`, `os`, cloud detector for your provider) |
| Most rows show "never" but you know your service emits | host.id / cloud.account.id mismatch with what scanner sees | Verify host.id is the instance ID (i-... for EC2); for GCE use numeric ID |
| Weak match percentage is high (> 20%) | Many spans key on host.name or service.name | Enable the cloud-aware host detector; spans without `cloud.account.id` always fall to weak match |
| `rows_evicted` consistently non-zero | Index cap is being hit | Either raise `SQUADRON_TRACEINDEX_MAX_ROWS` or audit `service.name` cardinality |
| Trace coverage shows N% strong + M% weak but doesn't add to 100 | Some inventory rows have NO match at all — strong+weak ≤ 100 | This is correct — the gap is the uncovered portion |
| Last seen column shows "1w ago" for actively-running services | Bug or stale traceindex | Restart Squadron; the in-memory cache reseeds on receiver activity |
| Audit shows `trace_index.background_flushed` but discovery summary doesn't update | Discovery dashboard cache is 30s; refresh after the next cycle | Click the dashboard's refresh button or wait |

## What this means for the universal observability claim

Slice 1 of trace integration shifts Squadron from discovery +
recommendation to discovery + RECONCILIATION. The model up to
v0.89.77 was "Squadron tells you what's wrong, you fix it."
Now: "Squadron tells you what's wrong, you fix it, Squadron
verifies the fix actually closed the gap."

Universal observability claim grows another dimension:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps AND
> verifies telemetry is actually flowing.

Slice 2 will turn the visibility into recommendation kinds:
`trace-emission-aws-compute`, `trace-emission-gcp-k8s`, etc. —
each one a proposer-drafted recommendation that says
"resource X has the primitive enabled but no recent emission.
Investigate via the inventory tab on each provider."

## Cross-references

- [Trace integration slice 1 design doc](./proposals/trace-integration-slice1.md) —
  the locked spec this runbook operationalizes.
- [Unified Discovery dashboard slice 1](./proposals/unified-discovery-dashboard-slice1.md) —
  the dashboard the new TRACE COVERAGE panel sits on.
- [Database tier slice 2](./proposals/database-tier-slice2.md) —
  one of the tiers whose Inventory tab gains a Last seen column.
- [Kubernetes tier slice 2](./proposals/kubernetes-tier-slice2.md) —
  same.
- [Audit log](./audit-log.md) — full catalog of event types
  including the two new trace integration events.
