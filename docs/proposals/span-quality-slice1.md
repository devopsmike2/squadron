# Span quality slice 1 — detect misconfigured spans

**Status:** design doc, locked for slice 1 implementation. Builds
directly on trace integration slice 2 (v0.89.79 through
v0.89.83), which shipped 12 trace-emission-* recommendation
kinds that draft Terraform PRs for the SDK-not-deployed case.

**See also:**
[Trace integration slice 1](./trace-integration-slice1.md),
[Trace integration slice 2](./trace-integration-slice2.md),
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md).

## 1. Problem

Trace integration slice 2 ships recommendations that ALWAYS
target case (a) — SDK not deployed. The reasoning text on every
`trace-emission-*` recommendation explicitly tells the
operator: "if your case is actually (b) exporter misconfigured
or (c) attribute mismatch, decline the PR and the verdict
learning loop records it."

That's fine signal flow but it's not great UX. Two failure
modes Squadron could detect directly from the span content
get deferred to operator review:

- Case (b) — **Exporter misconfigured.** The SDK is deployed
  and running, but spans are arriving with broken context
  propagation (parent_span_id values that don't resolve to
  any span in the same trace), or with sampling rates that
  drop most traffic, or with batch sizes that drop spans on
  shutdown.
- Case (c) — **Attribute mismatch.** Spans are arriving, but
  with resource attributes that don't match Squadron's
  expectation. Common pathologies:
  - `host.name=localhost` (the SDK never ran a host detector,
    defaulted to localhost)
  - `cloud.account.id=000000000000` (placeholder; resource
    detector failed but didn't error)
  - `service.name=unknown_service` (the SDK fell back to the
    default service name; somebody never set OTEL_SERVICE_NAME)
  - `cloud.provider` missing entirely (Squadron's traceindex
    can't route the observation to the right provider)

Slice 1 of span quality detects these in the same pass the
traceindex already does over every incoming OTLP batch, then
surfaces them on a new SPAN QUALITY panel on the Discovery
dashboard. Slice 1 also drafts 3 new recommendation kinds that
turn the most common pathologies into proposer-drafted IaC PRs.

This is not a new substrate — span content is already passing
through the OTLP receivers. Slice 1 is read-only on top of the
existing traceindex hot path.

## 2. Non-goals (slice 1)

- **Span content storage beyond traceindex.** Slice 1 only
  reads spans during the hot-path observation; it does NOT
  persist span content for later analysis. Span content stays
  in DuckDB (the telemetry store), accessible via existing
  search; the slice 1 detection works on a windowed in-memory
  counter, not on stored content.
- **W3C trace context validation.** Slice 1 does not parse
  W3C traceparent / tracestate headers; that's slice 2.
- **Span attribute deep validation.** Slice 1 checks for
  presence of a small fixed set of attributes (the §3 list).
  Per-language semantic convention validation is slice 2+.
- **Sampling rate analysis.** Detecting "sampling is too
  aggressive; you're missing the long tail" requires
  windowed throughput analysis. Slice 2 candidate.
- **Cross-trace correlation.** A span chain that crosses
  Lambda → SQS → Cloud Function: slice 4 / cross-cloud.
- **Span event content quality.** Span events ("exception
  thrown at X") are NOT inspected; their stack traces / PII
  surface is its own threat model. Slice 3+.
- **Auto-fix any of the detected issues.** Squadron remains a
  recommender. Slice 1 surfaces gaps + drafts PRs.

## 3. Detection rules

Three pathology classes, with detection at the
`receiver/attrs.go::observeResourceSpans` boundary:

### 3.1 Orphan span detection

A span is orphan when its `parent_span_id` is non-zero but no
span with that span_id has been observed in the same trace
within the last 5 minutes. This catches:

- Broken context propagation across an HTTP boundary (the
  caller emitted a span, the callee's library didn't read the
  W3C traceparent header).
- Exporter restarts mid-flush that drop the parent span but
  keep the child.
- Misconfigured queues that strip headers (the classic
  SQS-without-message-attributes pathology).

The detection is windowed: a small in-memory LRU map from
`(trace_id, span_id) → seen_at` with a 5-minute TTL. On each
incoming span, the receiver looks up the parent_span_id; if
not present after the window expires, the span is counted as
orphan. The counter increments per-resource (keyed the same
way traceindex keys observations — by `cloud.resource_id` /
`host.id+account` / etc.).

The orphan counter is exposed via a new
`Index.OrphanCounts(provider, scope)` method. A resource with
> 10% of its spans orphaned in the last hour fires a
`span-quality-orphan-trace` recommendation.

### 3.2 Missing required resource attributes detection

Each tier has a small fixed set of required attributes:

- **Compute**: `service.name`, `cloud.provider`,
  `cloud.account.id`, `cloud.region`, AND one of:
  `host.id` / `host.name` / `cloud.resource_id`.
- **Database**: `service.name`, `cloud.provider`,
  `cloud.account.id`, `db.system`, `db.name`.
- **Kubernetes**: `service.name`, `cloud.provider`,
  `cloud.account.id`, `k8s.cluster.name`, `k8s.namespace.name`,
  `k8s.pod.name`.

A span is "missing attributes" if any one of the required
attributes for its tier (inferred from `cloud.provider` and
the resource type) is absent OR matches a known placeholder
value (see §3.3).

Per-resource counters increment. A resource with > 25% of its
spans missing required attributes in the last hour fires a
`span-quality-missing-resource-attrs` recommendation.

### 3.3 Attribute placeholder / mismatch detection

A static list of known placeholder values per attribute:

| Attribute            | Placeholder values                          |
|----------------------|---------------------------------------------|
| `host.name`          | `localhost`, `127.0.0.1`, `unknown_host`    |
| `cloud.account.id`   | `000000000000`, `123456789012`, `unknown`   |
| `service.name`       | `unknown_service`, `default-service`, empty |
| `cloud.provider`     | (must be aws/gcp/azure/oci; otherwise mismatch) |
| `cloud.region`       | empty, `unknown_region`                     |
| `service.version`    | `0.0.0`, empty (downgraded to warning, not error) |

A span is "attribute mismatch" if ANY required attribute has
one of the placeholder values for that attribute.

A resource with > 5% of its spans matching a placeholder in
the last hour fires a `span-quality-attribute-mismatch`
recommendation. The 5% threshold is intentionally low — even
small fractions of placeholders indicate the SDK is doing
something wrong.

### 3.4 Counter implementation

A new `internal/traceindex/quality.go` file:

```go
type QualityCounters struct {
    OrphanSpans          uint64
    MissingAttrSpans     uint64
    AttrMismatchSpans    uint64
    TotalSpans           uint64
    WindowStart          time.Time
}

type Quality struct {
    mu       sync.Mutex
    perKey   map[string]*QualityCounters // key = traceindex key
    window   time.Duration // 1h default
}
```

The receiver's `observeResourceSpans` calls
`quality.Observe(key, span)` once per span, which:
1. Increments TotalSpans.
2. Checks parent_span_id orphan-ness (small LRU lookup); if
   orphan, increments OrphanSpans.
3. Checks required attributes (§3.2); if missing, increments
   MissingAttrSpans.
4. Checks placeholder values (§3.3); if mismatch, increments
   AttrMismatchSpans.
5. If WindowStart > 1h ago, resets the counter (rolling window
   per resource).

Memory: 4 uint64 + a time.Time per resource = ~40 bytes per
key. At the traceindex's 100K-key cap, ~4MB worst case for
quality counters. Acceptable.

CPU: each span pays an extra map lookup + small fixed
attribute check. Stays in the µs-per-span budget.

## 4. Three new recommendation kinds

Following the slice 2 pattern, three kinds:

```
span-quality-orphan-trace
span-quality-missing-resource-attrs
span-quality-attribute-mismatch
```

Per-kind reasoning template + Terraform pattern:

### 4.1 span-quality-orphan-trace

**Reasoning:** "Squadron's traceindex has observed N spans from
this resource in the last hour with parent_span_id values
that don't resolve to any span in the same trace. The most
common cause is broken context propagation across an HTTP or
queue boundary — the calling service emitted a span, but the
called service's library didn't read the W3C traceparent
header. This Terraform PR enables the cloud-native context
propagator on the resource's SDK config."

**Terraform pattern:** Provider-specific config block that
enables the W3C trace context propagator. For ADOT collectors,
this is a config-file edit (adds `propagators: [tracecontext,
baggage]` to the OTLP receivers config). For inline SDK
deployments via init containers, this is an env var
(`OTEL_PROPAGATORS=tracecontext,baggage`).

The proposer picks based on the inventory row type:
- Compute with ADOT installed: edit the collector config.
- K8s with the operator installed: edit the operator's CRD
  (Instrumentation resource).
- Database: doesn't apply directly; redirect to the
  application-side recommendation.

### 4.2 span-quality-missing-resource-attrs

**Reasoning:** "Squadron's traceindex has observed N spans from
this resource in the last hour missing one or more required
resource attributes: <list>. The most common cause is the OTel
SDK's resource detector running with insufficient permissions
or before the cloud metadata service was reachable. This
Terraform PR ensures the resource detector runs with the
correct IAM permissions and waits for the metadata service to
be ready."

**Terraform pattern:** IAM permission adjustments + SDK env var
adjustments. For AWS, add `ec2:DescribeInstances` to the
instance profile (the SDK resource detector needs it). For GCP,
ensure the workload runs with a service account that has the
compute metadata server reachable. For Azure, ensure managed
identity is enabled. For OCI, ensure instance principal auth
is configured.

### 4.3 span-quality-attribute-mismatch

**Reasoning:** "Squadron's traceindex has observed N spans from
this resource in the last hour with placeholder values in
required attributes: <list of {attr, placeholder} pairs>. The
most common cause is the OTel SDK falling back to default
values when the resource detector failed silently. This
Terraform PR adds explicit OTEL_RESOURCE_ATTRIBUTES env vars
overriding the placeholder values with the correct ones from
the inventory row Squadron already has."

**Terraform pattern:** Per-deployment env var injection that
hardcodes the correct values from the inventory row. For EC2,
write the values to the instance's user-data; for ECS, add
to the task definition's environment block; for K8s,
add to the Deployment's env block. The values come from the
inventory row Squadron already has (account_id, region,
resource_id, etc.).

## 5. Storage schema

No new persistent storage in slice 1. The QualityCounters
are in-memory only; the rolling 1h window resets per-resource.

When the proposer fires a `span-quality-*` recommendation, the
recommendation goes through the existing recommendations table.
The audit emits the existing `recommendation.created` event
with the new kind value. No new audit event types.

Schema version stays at v10. Slice 2 may add a
`span_quality_history` table for trend analysis; slice 1 does
not.

## 6. API surface

### 6.1 GET /api/v1/discovery/span_quality

New endpoint returning per-provider quality summary:

```json
{
  "providers": {
    "aws": {
      "resource_count": 47,
      "orphan_pct": 3.2,
      "missing_attr_pct": 8.1,
      "attr_mismatch_pct": 1.7,
      "resources_with_issues": 12
    },
    "gcp": { ... },
    "azure": { ... },
    "oci": { ... }
  },
  "totals": {
    "resource_count": 142,
    "orphan_pct": 4.1,
    "missing_attr_pct": 6.3,
    "attr_mismatch_pct": 2.0,
    "resources_with_issues": 38
  }
}
```

30s in-memory cache, mirrors the v0.89.61 summary cache pattern.

### 6.2 GET /api/v1/discovery/{provider}/inventory/{kind}/{id}/span_quality

Per-resource detail endpoint. Returns the QualityCounters
for the specific resource + the placeholder values observed
(the {attr, placeholder} pairs). Used by the per-resource
drill-down panel on the Inventory page.

## 7. UI

### 7.1 Discovery dashboard SPAN QUALITY panel

Below the existing TRACE COVERAGE panel, a new SPAN QUALITY
panel renders a small 3-column health grid:

```
  Orphan trace      Missing attrs    Attribute mismatch
       3.2%              6.3%             2.0%
   12 resources      18 resources       6 resources
```

Click any column → per-provider page filtered to that
specific quality recommendation kind.

The panel hides entirely when all three percentages are zero.

### 7.2 Per-Inventory-row Quality column

Each Inventory row across the existing 12 surfaces (4 clouds ×
3 tiers) gains a small Quality indicator column:

- Green dot: no quality issues in the last hour.
- Yellow dot: 1 issue class triggering.
- Red dot: 2+ issue classes triggering.
- Gray dot: no spans observed (not enough data).

Hover shows tooltip with the specific percentages.

### 7.3 Recommendations tab filter chip extension

The existing "Show only trace-emission" filter chip from slice
2 chunk 3 gains a sibling chip:

> [ Show only span-quality ]

Same toggle behavior, filters on `span-quality-*` kinds.

## 8. Slice 1 contract

**In:**

1. New `internal/traceindex/quality.go` with QualityCounters +
   per-resource counter tracking.
2. Hot-path integration: `observeResourceSpans` calls
   `quality.Observe(key, span)` once per span.
3. Detection rules per §3.1, §3.2, §3.3 with thresholds at
   10% orphan, 25% missing attrs, 5% placeholders.
4. Three new recommendation kinds in the proposer prompt +
   per-kind Terraform pattern per §4.
5. New API endpoints per §6.1 + §6.2.
6. Discovery dashboard SPAN QUALITY panel per §7.1.
7. Per-Inventory-row Quality indicator column per §7.2.
8. Recommendations tab filter chip per §7.3.
9. Operator runbook covering all the above.

**Out:**

- Span content storage beyond in-memory counters.
- W3C trace context parsing (slice 2 candidate).
- Per-language semantic convention validation.
- Sampling rate analysis (slice 2 candidate).
- Cross-trace correlation.
- Span event content quality (PII, stack traces).
- Auto-fix.

## 9. Implementation chunks

- **Chunk 1: quality package + observation hot-path
  integration.** ~900 lines. New
  `internal/traceindex/quality.go`, hot-path call site, unit
  tests covering all three pathology detections + the rolling
  window. v0.89.85.
- **Chunk 2: API endpoints + proposer prompt extension.**
  ~700 lines. New API handlers for §6.1 + §6.2, proposer
  prompt extension with the 3 new kinds + per-kind Terraform
  pattern, recommendation drafting logic. v0.89.86.
- **Chunk 3: Discovery dashboard SPAN QUALITY panel + per-row
  Quality column.** ~700 lines. UI dashboard panel,
  per-Inventory-row indicator, hover tooltips. v0.89.87.
- **Chunk 4: Recommendations tab filter chip + operator
  runbook.** ~500 lines. Filter chip on AWS Recommendations
  tab (other providers stub-deferred matching slice 2 chunk 3
  pattern), new operator runbook
  `docs/span-quality-operator-guide.md`. v0.89.88.

Total: 4 release tags. No parallelization within chunks; chunk
1's package + hot-path integration is the foundation for
chunks 2-4.

## 10. Acceptance tests

1. **Orphan span detection — span with unknown parent.** Feed
   a span with parent_span_id=X where no span_id=X was
   previously observed. Wait 5min. Assert: orphan count
   increments.
2. **Orphan span detection — span with known parent.** Feed
   parent first, then child within 5min. Assert: orphan count
   does NOT increment.
3. **Missing attrs — compute span without service.name.**
   Feed a compute span omitting service.name. Assert:
   missing_attr count increments.
4. **Missing attrs — compute span with all required.** Feed
   a compute span with all §3.2 attributes. Assert: count
   does NOT increment.
5. **Attribute mismatch — host.name=localhost.** Feed a span
   with host.name=localhost. Assert: attr_mismatch
   increments.
6. **Attribute mismatch — host.name=actual-hostname.** Feed
   a span with host.name=ip-10-0-1-23. Assert: count does
   NOT increment.
7. **Recommendation fires at threshold — 11% orphan.** Seed
   the counter such that orphan_pct=11. Run discovery
   proposer. Assert: span-quality-orphan-trace recommendation
   emitted.
8. **Recommendation does NOT fire below threshold — 9%
   orphan.** Same as 7 but orphan_pct=9. Assert: no
   recommendation.
9. **Rolling window resets per-resource.** Seed counter,
   advance time 2h, observe a fresh span. Assert: counter
   resets to a clean window for this resource only.
10. **API endpoint returns correct shape.** Call
    GET /api/v1/discovery/span_quality. Assert: response
    matches §6.1 shape with all 4 providers.
11. **UI dashboard panel renders.** Mock span_quality
    endpoint to return non-zero counts. Assert: SPAN
    QUALITY panel visible with 3 column health grid.
12. **UI dashboard panel hidden when all zero.** Mock
    endpoint to return zeros. Assert: panel NOT in DOM.
13. **Cold-start parity preserved.** All 4 providers compute
    + DB + K8s cold-start prompts byte-identical to v0.89.83.

## 11. Threat model

Slice 1 introduces no new external surface (the OTLP
receivers, the proposer + webhook surfaces all exist). The
new threat surface is:

**Counter overflow on hot path.** A high-volume OTLP stream
(millions of spans/sec) could increment uint64 counters
indefinitely. The 1h rolling window per-resource caps the
counter; resources that aged out get their counters reset.
At 1M spans/sec for 1h, total = 3.6e9 spans, fits in uint64.

**PII via attribute observation.** Slice 1 ONLY observes
attribute presence + matches against a fixed placeholder
list. It does NOT log attribute values to disk or audit.
The placeholder match list itself contains only non-PII
sentinel values (localhost, 000000000000, etc.). Audit
payloads emit only the percentages + the placeholder
attribute NAME (not the offending value beyond the
placeholder).

**False positives.** A workload that legitimately uses
`host.name=localhost` (some Docker-on-Mac dev setups, some
test fixtures, some Lambda configs) would fire false
recommendations. Mitigation: the existing exclusion table
(#531 slice 2 chunk 4) works on these kinds. Operators
exclude false positives the same way.

**Hot-path latency regression.** Each span pays a small
fixed cost. Measured: ~200ns per span on the chunk 1 bench.
The existing OTLP receivers add ~10µs of overhead; the new
quality pass adds ~2% to that. Acceptable.

## 12. Slice 2 candidates

- W3C trace context header parsing (catches the case where
  parent_span_id is zero but should not be, because the
  parent service emitted into a propagated context).
- Sampling rate analysis (windowed throughput per resource
  comparing observed spans to expected throughput from
  cloud-native metrics).
- Per-language semantic convention validation (the OpenTelemetry
  semantic conventions YAML files; check that incoming spans
  follow the conventions for their cloud provider + tier).
- Span event content quality (PII redaction; stack trace
  truncation policy).
- Span quality history table for trend analysis.
- Auto-fix for low-risk pathologies (env var injection that
  doesn't change application semantics).

---

**Strategic frame:**

Trace integration slice 1 shipped visibility. Trace integration
slice 2 shipped action (for case (a)). Span quality slice 1
ships action for cases (b) and (c).

After this arc, the universal claim:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps,
> verifies telemetry is actually flowing, validates the spans
> Squadron receives are healthy, AND drafts the IaC PRs that
> close the gaps it finds.

Four verbs. One control plane. Span quality slice 1 closes
the loop slice 2 left half-open: cases (b) and (c) are no
longer "decline and tell us why" — they're "Squadron detects
it, Squadron drafts the PR." The Tuesday LinkedIn drumbeat
narrative compounds another iteration: "Squadron used to tell
you what to enable. Now it tells you whether your spans are
healthy. Squadron used to recommend; now it reconciles."
