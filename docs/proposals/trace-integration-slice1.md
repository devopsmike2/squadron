# Trace integration — slice 1 design

**Status:** design doc, locked for slice 1 implementation. First
arc that consumes Squadron's own OTLP receiver stream as DISCOVERY
SIGNAL, transforming the recommendation surface from "did you turn
on the primitive" to "is telemetry actually flowing."

The strategic frame: after twelve scanner surfaces shipped
(compute + database + kubernetes across four clouds), every
recommendation Squadron currently makes is at the
configuration-primitive layer. "Enable Container Insights."
"Add an otel-collector label." "Bind the OperationsInsights
managed package." Operators who follow those recommendations turn
the primitive on. They do NOT necessarily start getting
telemetry. The primitive being on does not mean an OTel collector
got deployed; the collector being deployed does not mean it is
sending; the sending does not mean Squadron is receiving.

Trace integration closes that gap by reading the receiver stream
Squadron already runs (OTLP HTTP on 4318 and gRPC on 4317) and
indexing which resources have actually emitted spans recently.
The discovery dashboard gains a coverage percentage. The
inventory tabs gain a last-seen-at annotation per row. Slice 1
ships VISIBILITY into the gap. Slice 2 will ship
RECOMMENDATION GENERATION for it.

**See also:**
[Unified Discovery dashboard slice 1](./unified-discovery-dashboard-slice1.md),
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md),
[ai-features.md](../ai-features.md).

## 1. Problem

Squadron's discovery scanner answers "is the observability
primitive enabled on this resource." It does not answer "is this
resource actually emitting telemetry." Those questions are
distinct in practice:

- A GKE cluster can have Managed Prometheus enabled but have
  zero workloads instrumented.
- An EC2 instance can have an `otel-collector` tag but no
  collector process running, or a collector process running but
  failing to reach Squadron's endpoint, or a collector reaching
  Squadron but using a `service.name` that does not match any
  inventory key.
- An Azure SQL database can have a Diagnostic Setting routing
  SQLInsights to Log Analytics, but the operator who turned it
  on never actually queried anything from the workspace.

Each of those failure modes is invisible to the current
discovery surface. Squadron flags the resource as INSTRUMENTED
because the primitive bit is set. The operator merges the PR.
Days later they realize traces are not flowing and have to debug
the deployment.

Trace integration closes this gap by giving the discovery
surface a second axis: **trace emission**. A resource is FULLY
covered when (a) the primitive is enabled AND (b) Squadron has
seen spans from it recently. The gap between (a) and (b) is the
actionable signal slice 2 will turn into recommendations.

Slice 1 ships just the visibility. Operators can SEE which of
their primitive-enabled resources have not emitted in N hours.
They cannot YET get a Terraform PR drafted to fix it; that is
slice 2.

## 2. Non-goals (slice 1)

- **Recommendation generation for trace gaps.** A resource that
  has the primitive enabled but no recent spans is FLAGGED in
  slice 1, not RECOMMENDED on. Slice 2 introduces a
  `trace-emission-fix` recommendation kind family.
- **Span quality analysis.** Squadron sees spans arriving; it
  does not yet assess whether resource attributes are complete,
  whether parent-child propagation is unbroken, or whether the
  service.name is canonical. Slice 3.
- **Trace storage and query.** Squadron's existing DuckDB
  backend already stores spans for the telemetry inventory view.
  Slice 1 does NOT change span storage; it adds a separate
  `trace_resource_seen` index keyed by best-effort resource
  identity, which is much smaller (one row per emitting resource)
  than the full span store.
- **Cost analysis on trace volume.** Squadron's existing
  cost-spike detector watches volume metrics on the collector's
  own self-telemetry. Slice 1 does not extend cost coverage to
  app traces; that is its own concern in slice 3+.
- **Per-service-version detection.** Slice 1 indexes resources
  not service versions. Slice 2 candidate: detect when an
  emitting service stops emitting after a version bump (canary
  regression signal).
- **Cross-cloud span propagation.** A span chain that crosses
  AWS Lambda → SQS → GCP Cloud Function carries context across
  cloud boundaries. Squadron does not yet correlate those chains
  with the inventory. Slice 4+.
- **Metrics and logs integration.** Slice 1 covers traces only.
  Metrics and logs go through the same OTLP receiver but the
  inventory correlation rules are different per signal type;
  metrics integration is slice 2, logs integration is slice 3.

## 3. Architectural decision: resource identity matching

The hard problem is correlating a span the receiver sees to a row
in discovery inventory. Spans arrive with resource attributes per
the OTel semantic conventions; the SDK on each host populates
those attributes from the host detector. Discovery inventory uses
provider-native identifiers. The two do not naturally align.

Per OTel semantic conventions, the relevant resource attributes
are:

- `host.id` — provider-native instance ID (EC2 i-…, GCE numeric
  ID, Azure VM ID, OCI instance OCID). Set by the OTel SDK's
  cloud-aware host detector. Most reliable when set.
- `host.name` — host's network name. Set by the OS detector;
  often the same as the instance hostname.
- `cloud.account.id` — AWS account ID / GCP project ID / Azure
  subscription ID / OCI tenancy OCID. Set by the cloud detector.
- `cloud.resource_id` — full ARN-shaped identifier. Set by the
  cloud detector when available.
- `k8s.cluster.name` — for K8s workloads. Set by the K8s
  detector.
- `db.system` + `db.name` — for database workloads. Set by the
  DB SDK.
- `service.name` — the operator-controlled service identifier.
  Always set but not naturally tied to inventory.

Slice 1's matching strategy: build a `resource_key` per incoming
span's resource attribute set using a fallback chain:

1. If `cloud.resource_id` is set, use it as `resource_key`.
2. Else if `host.id` is set, use `{cloud.provider}:{cloud.account.id}:{host.id}`.
3. Else if `k8s.cluster.name` is set AND `cloud.account.id` is set,
   use `{cloud.provider}:{cloud.account.id}:k8s:{k8s.cluster.name}`.
4. Else if `db.system` + `db.name` are set, use
   `{cloud.provider}:{cloud.account.id}:db:{db.system}:{db.name}`.
5. Else fall back to `host.name` alone, with a flag indicating
   the match is best-effort.
6. Else fall back to `service.name`, with the same flag.

The discovery side computes the SAME `resource_key` from the
inventory snapshot fields (the scanner output already carries
provider, account/project/subscription/tenancy ID, and the
resource ID). The traceindex query does a direct join.

When the fallback chain hits the host.name or service.name tier,
the match is BEST-EFFORT. The discovery dashboard surfaces this
distinction: a resource with a host.id match shows ✓ confidently;
a resource with a host.name match shows ✓ but with a small caveat
indicator the operator can hover for the explanation.

## 4. Storage

A new in-process `traceindex` package owns the index. Backend
storage is a new SQLite table on the application store (NOT the
DuckDB telemetry store, which is sized for full span retention).

```sql
CREATE TABLE trace_resource_seen (
    resource_key             TEXT PRIMARY KEY,
    provider                 TEXT NOT NULL,    -- aws / gcp / azure / oci / unknown
    scope_id                 TEXT,             -- account_id / project_id / subscription_id / tenancy_ocid
    resource_id_hint         TEXT,             -- raw cloud.resource_id when present
    service_name             TEXT,             -- service.name from latest span
    first_seen_at            TIMESTAMP NOT NULL,
    last_seen_at             TIMESTAMP NOT NULL,
    span_count_24h           INTEGER NOT NULL,
    root_span_count_24h      INTEGER NOT NULL,
    attributes_json          TEXT,             -- last full resource attribute map for diagnostic UI
    match_confidence         TEXT NOT NULL,    -- "strong" / "weak" — strong if cloud.resource_id or host.id keyed
    updated_at               TIMESTAMP NOT NULL
);
CREATE INDEX idx_trace_resource_seen_provider_scope ON trace_resource_seen(provider, scope_id);
CREATE INDEX idx_trace_resource_seen_last_seen ON trace_resource_seen(last_seen_at);
```

Schema bump v9 → v10.

`span_count_24h` is a rolling counter the index updates as
batches arrive; the discovery dashboard reads it for the coverage
panel. Slice 1 ships a coarse "24h" window because finer-grained
windowing requires either time-bucketed rows (more storage cost)
or query-time aggregation against the DuckDB span store (more
query complexity). Slice 2 can refine.

The traceindex maintains an in-memory write-through cache for
the last N minutes of activity so the OTLP receiver's hot path
does not touch SQLite per span. Background flush job writes the
batched updates every 30 seconds.

## 5. Receiver integration

The existing OTLP receiver at `internal/otlp/receiver/http_server.go`
routes `POST /v1/traces` to `handleOTLPTraces`. The handler
unmarshals the `coltracepb.ExportTraceServiceRequest` and
dispatches the span batches to the existing worker pool for
processing.

Slice 1 adds a NEW dispatch path INSIDE the handler: after the
unmarshal, the handler also notifies the traceindex of every
ResourceSpan in the request. The traceindex extracts resource
attributes, computes `resource_key`, updates its in-memory cache,
and returns immediately. The hot path adds ~10 microseconds per
ResourceSpan; the SQLite flush happens in background.

The gRPC receiver (port 4317) gets the same integration in its
parallel handler.

### 5.1 Why duplicate dispatch instead of mid-pipeline tap?

The existing worker pool processes spans for storage; the
traceindex's needs are smaller (resource-level seen-time tracking,
not per-span storage). Decoupling means the traceindex can be
disabled in deployments that do not want it without affecting
span storage. It also makes the index resilient to schema
evolution in the worker pool processor.

## 6. Discovery integration

The discovery dashboard at `/discovery` (v0.89.62) gains a TRACE
COVERAGE panel under the existing per-provider cards. The new
panel:

- Shows the percentage of inventoried resources with at least one
  span seen in the last 24h, per provider.
- Surfaces a per-cloud breakdown identical to the existing
  instrumentation percentage breakdown.
- Includes a "Trace coverage" sub-section in each provider card.

Per-provider Inventory tabs (DiscoveryAWS, DiscoveryGCP,
DiscoveryAzure, DiscoveryOCI) gain a `last_seen_at` column on
Compute / Database / Kubernetes tables. Values are relative time
strings: "2m ago", "1h ago", "3d ago", "never". A "never" value
gets a yellow indicator inviting the operator to investigate.

The Recommendations tab in slice 1 does NOT gain a new
recommendation kind. Slice 2 introduces
`trace-emission-{aws,gcp,azure,oci}-{compute,db,k8s}` kinds.

## 7. API endpoints

New endpoint:
- `GET /api/v1/discovery/trace_coverage` returns the per-provider
  trace coverage summary. Cache 30s in memory (the existing
  Discovery dashboard cache pattern from v0.89.61).

Response shape:
```json
{
  "providers": {
    "aws": {
      "inventory_count": 142,
      "emitting_count": 89,
      "coverage_pct": 62.7,
      "strong_match_pct": 88.0,
      "weak_match_pct": 12.0,
      "last_index_update_at": "2026-06-23T14:32:00Z"
    },
    "gcp": { ... },
    "azure": { ... },
    "oci": { ... }
  },
  "totals": {
    "inventory_count": 198,
    "emitting_count": 122,
    "coverage_pct": 61.6
  }
}
```

The existing per-provider list endpoints (e.g.
`/api/v1/discovery/aws/connections/:id/scan`) gain a
`last_seen_at` annotation per inventory row, looked up against
the traceindex at query time.

## 8. Audit events

Two new event types:

```go
const AuditEventTraceIndexBackgroundFlushed = "trace_index.background_flushed"
const AuditEventTraceCoverageRequested = "discovery.trace_coverage.requested"
```

The flush event fires once per background flush cycle (every 30s
under normal load); the payload carries the count of rows updated
and the index size. Useful for operators debugging "is the
traceindex healthy."

The coverage requested event fires on every cache-miss request to
the new summary endpoint, mirroring the Discovery summary
endpoint pattern from v0.89.61.

No span content is in either audit payload. Trace data stays in
the traceindex / DuckDB; audit captures only the meta-shape of
the index.

## 9. Slice 1 contract

**In:**

1. New package `internal/traceindex` with:
   - `Index` struct holding the in-memory write-through cache
   - `Observe(ctx, resourceAttrs map[string]any, spanCount, rootSpanCount int)` method
   - `Coverage(ctx, provider, scopeID string) (Summary, error)` method
   - `LastSeenAt(ctx, resourceKey string) (time.Time, bool, error)` method
2. Schema migration v9 → v10: `trace_resource_seen` table.
3. ApplicationStore methods on the new table:
   `UpsertTraceResource(...)`, `GetTraceResource(...)`,
   `ListTraceResourcesByScope(provider, scopeID, since)`.
4. Receiver integration: `handleOTLPTraces` (HTTP) AND the gRPC
   trace handler dispatch to `Index.Observe` for each ResourceSpan.
5. Background flush job: in-memory cache → SQLite every 30s.
   Audit event `trace_index.background_flushed` on each cycle.
6. New endpoint `GET /api/v1/discovery/trace_coverage`. Audit
   event `discovery.trace_coverage.requested` on cache miss.
7. Per-provider scan response: `last_seen_at` annotation on
   ComputeInstanceSnapshot / DatabaseInstanceSnapshot /
   ClusterSnapshot.
8. UI: Discovery dashboard gains TRACE COVERAGE panel showing
   the per-provider percentages.
9. UI: Per-provider Inventory tabs gain `last_seen_at` column.
10. Tests covering the matching logic + the endpoint + the
    UI rendering.

**Out:**

- `trace-emission-*` recommendation kinds (slice 2).
- Span quality analysis (slice 3).
- Trace storage changes (slice 1 keeps the existing DuckDB span
  store untouched).
- Cost analysis on trace volume.
- Per-service-version detection.
- Cross-cloud span propagation correlation.
- Metrics and logs integration.

## 10. Implementation chunks

- **Chunk 1: traceindex package foundation + storage migration.**
  ~600-800 lines. New package, schema bump, ApplicationStore
  methods, in-memory cache, Observe + Coverage + LastSeenAt
  methods. v0.89.74.
- **Chunk 2: Receiver integration.** ~300-500 lines. Wire
  Index.Observe into the HTTP and gRPC trace handlers. Background
  flush goroutine. Audit constants. v0.89.75.
- **Chunk 3: Discovery API extension + UI dashboard panel.**
  ~700-900 lines. New endpoint, response composition, frontend
  panel + per-provider card extension. v0.89.76.
- **Chunk 4: Per-provider Inventory last_seen_at column.**
  ~500-700 lines. Backend annotation in scan responses,
  frontend column rendering across all 4 DiscoveryAWS / GCP /
  Azure / OCI pages. v0.89.77.
- **Chunk 5: Runbook + operator guide.** ~400-600 lines. New
  doc `docs/trace-coverage-operator-guide.md` explaining the
  matching strategy, the strong vs weak confidence indicator,
  the slice 1 → slice 2 progression. v0.89.78.

Total: 5 release tags. Chunks 3 + 4 could parallelize (both
extend the UI but on disjoint code paths) but slice 1 prefers
sequential to keep the change surface coherent for review.

## 11. Acceptance tests

1. **Observe with cloud.resource_id matches discovery row.**
   Send a span batch with resource attribute
   `cloud.resource_id="arn:aws:ec2:us-east-1:12345:instance/i-0abc"`.
   Assert: traceindex has a row keyed by that ARN, match_confidence="strong".
2. **Observe with host.id falls back correctly.** Send a span
   with `host.id="i-0abc"` and `cloud.account.id="12345"`.
   Assert: row keyed by `aws:12345:i-0abc`, match_confidence="strong".
3. **Observe with host.name only.** Send with `host.name="db-prod"`,
   no other identifiers. Assert: row keyed by host.name,
   match_confidence="weak".
4. **Coverage computed against discovery inventory.** Seed 10
   AWS inventory rows. Seed 6 traceindex rows with resource keys
   matching 6 of the 10. Call `Coverage(aws, "12345")`. Assert:
   `inventory_count=10, emitting_count=6, coverage_pct=60.0`.
5. **Cache TTL behavior.** Two summary requests within 30s.
   Assert: audit event `discovery.trace_coverage.requested`
   fires only once.
6. **Background flush.** Observe 100 spans in memory. Wait 31s
   (or trigger flush manually). Assert: SQLite has 100 rows
   updated, flush audit event fired with `rows_updated=100`.
7. **last_seen_at annotation on AWS scan response.** Inventory
   row for `i-0abc`. Traceindex shows last seen 5 minutes ago.
   Call AWS scan endpoint. Assert: the response row for `i-0abc`
   has `last_seen_at` set to the right ISO timestamp.
8. **last_seen_at on inventory row with no traces.** Assert:
   `last_seen_at` is null (or "never" in UI).
9. **Discovery dashboard panel renders.** Mock the trace_coverage
   endpoint to return per-provider 60/40 split. Assert: dashboard
   shows the percentages and ring indicator.
10. **Mismatch on weak match confidence shows caveat.** Inventory
    row matched via host.name only. Assert: UI shows a
    confidence indicator on hover.
11. **Cold-start parity preserved.** No spans observed → traceindex
    has no rows → coverage_pct=0 (zero-safe, not NaN) →
    last_seen_at is null on every inventory row → discovery
    dashboard renders without the trace coverage panel data but
    without errors.
12. **Span content not in audit.** Verify across both new audit
    events that no span attributes, no span names, no trace IDs
    are in the payload — only the meta-shape (counts, sizes).

## 12. Threat model

Trace integration consumes data already inside Squadron's
process (the OTLP receiver runs on the same Squadron binary).
No new external surface area, no new credentials, no new
endpoints exposed beyond Squadron's existing API auth.

The two threats worth naming:

**Resource attribute spoofing.** A malicious tenant in a
multi-tenant Squadron deployment could send spans with
`cloud.resource_id` claiming to be from a different tenant's
resource. Slice 1 documents that Squadron is SINGLE-TENANT
today (the existing posture from v0.89.0 onward) and that the
trace correlation rule trusts the sender. Multi-tenant
isolation is the same gating concern that blocks several other
slice 2+ features and is tracked separately.

**Volume amplification on the traceindex.** A high-cardinality
attribute set (e.g. a span with a unique `service.name` per
request) would produce a new traceindex row per request, blowing
up the row count and the SQLite size. Slice 1's mitigation is a
hard cap on the index (default 100K rows) with LRU eviction when
the cap is hit. Operators configure via env var. The flush
event payload includes the eviction count when non-zero so
operators can detect the situation.

**Span content as PII concern.** Span attributes commonly carry
URLs, query parameters, user identifiers. Squadron's existing
DuckDB span store retains all of this. The traceindex stores
the RESOURCE-LEVEL attribute snapshot (which generally does
NOT carry PII; it carries deployment identifiers), explicitly
NOT span content. Audit events carry zero span content.

## 13. Open questions

1. **Refresh cadence for the inventory join.** A discovery scan
   produces inventory at a point in time. The traceindex updates
   continuously. Which timestamp should the dashboard report
   for the coverage percentage? Slice 1 picks: dashboard reports
   the most recent scan inventory joined against the current
   traceindex state, computed at endpoint-call time. Slice 2 may
   add a "trailing 24h coverage" view that better tolerates
   workloads that emit intermittently.

2. **How to handle resources that disappear from inventory.**
   When an operator deletes an EC2 instance, the next scan no
   longer surfaces it. But the traceindex may still have spans
   from before the deletion. Slice 1 ignores entries that don't
   join with current inventory; slice 2 may surface them as a
   "stale resource" indicator for operators to clean up
   downstream services that still reference the dead host.

3. **Confidence indicator UX.** "Strong" vs "weak" match is a
   binary today. Operators might want a numeric confidence
   score. Slice 2 candidate; slice 1 keeps it binary for
   simplicity.

4. **Whether to surface traces that fail to correlate AT ALL.**
   Spans with no host.id, no host.name, no service.name (or
   `service.name="unknown_service"`) get dropped from the index
   silently in slice 1. Slice 2 might add a "orphan trace
   volume" indicator so operators can spot misconfigured
   exporters.

5. **Privacy and compliance posture for attributes_json.** The
   slice 1 schema stores the full resource attribute map
   verbatim for diagnostic purposes. Operators with strict PII
   posture may want to redact specific keys before persistence.
   Slice 2 adds a per-deployment redaction list.

---

**Strategic frame:**

Trace integration is the first arc that shifts Squadron from
discovery + recommendation to discovery + RECONCILIATION.
Previously, the model was "Squadron tells you what's wrong, you
fix it." Now: "Squadron tells you what's wrong, you fix it,
Squadron VERIFIES the fix actually closed the gap."

This is what closes the loop the Tuesday LinkedIn post was
implicitly pointing at. The post's narrative landed on "make the
postmortem about the proposal the operator turned down, not
about Tuesday's surprise." Trace integration is what makes the
"is the proposal actually working" question answerable.

Slice 2 of trace integration turns the visibility into
recommendation kinds: `trace-emission-aws-compute`,
`trace-emission-gcp-k8s`, etc. — each one a proposer-emitted
recommendation that says "Resource X has the primitive enabled
but no recent emission. Suggested investigation: SDK deployment
check via the inventory tab on each provider."

Slice 3 candidates: metrics and logs integration (same pattern,
different signal type); span quality analysis (broken context
propagation, missing required resource attributes); cross-cloud
correlation; cost analysis on app trace volume.

After slice 1 ships, Squadron's universal claim grows another
dimension: "Squadron scans AWS, GCP, Azure, AND Oracle Cloud
across COMPUTE, DATABASE, AND KUBERNETES for observability gaps
AND verifies telemetry is actually flowing." Two-step process
in one product surface.
