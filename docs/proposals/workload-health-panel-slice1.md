# Workload Health dashboard panel slice 1

**Status:** design doc, locked for slice 1 implementation.
Consolidates the three substrate diagnostics (cold-start
latency + sampling rate + error rate) into a single
dashboard panel below the existing SPAN QUALITY panel.

**See also:**
[Cold-start latency slice 1](./cold-start-latency-slice1.md),
[Cold-start latency slice 2](./cold-start-latency-slice2.md),
[Sampling rate analysis slice 1](./sampling-rate-analysis-slice1.md),
[Error rate correlation slice 1](./error-rate-correlation-slice1.md),
[Unified Discovery dashboard slice 1](./unified-discovery-dashboard-slice1.md).

## 1. Problem

The cold-start latency + sampling rate + error rate arcs all
ship per-Inventory-row columns on each per-provider
Serverless table. That's the right surface for inspecting a
single resource, but it leaves a dashboard-level gap:

- An operator opening Squadron's dashboard at `/discovery`
  sees the existing TRACE COVERAGE + SPAN QUALITY panels.
- The substrate's three diagnostics — cold-start, sampling,
  error rate — are only visible by navigating to each
  per-provider Discovery page and scanning the Serverless
  table column-by-column.
- A multi-cloud operator with serverless across all 4 clouds
  has to do this 4 times to get the picture.

The error rate slice 1 runbook explicitly flagged this gap:

> "Slice 2 may add a top-level 'Workload health' panel
> summarizing cold-start + sampling + error rate together."

Slice 1 of Workload Health ships that panel.

The panel:
- Shows 3 columns, one per substrate diagnostic
- Each column shows the % of serverless resources exceeding
  the diagnostic's threshold across all 4 clouds
- Click any column → deep-links to a per-provider
  Recommendations tab filtered by the relevant kind
- Hides when no serverless inventory exists OR all 3
  percentages are zero

This is a polish arc — exposes existing data at a new
surface. No new substrate, no new metrics, no new
recommendation kinds.

## 2. Non-goals (slice 1)

- **Per-cloud breakdown in the dashboard panel.** Slice 1
  shows aggregate percentages across all 4 clouds. The
  per-cloud breakdown lives on each provider's Discovery
  page already. Slice 2 may add per-provider chips.
- **Time-series trend visualization.** The percentages are
  point-in-time aggregates from the latest scan. Slice 2+
  may add 7-day trend sparklines.
- **Drilldown beyond the deep-link.** Click goes to the
  Recommendations tab filtered by kind. It does NOT open a
  detail modal in slice 1.
- **Compute / database / kubernetes inclusion.** The
  substrate's three diagnostics ship for serverless only
  (slice 1 of cold-start, sampling, error rate all scope
  to the 5 serverless surfaces). The panel reflects this
  scope; slice 2 may expand when compute/db/k8s versions of
  these diagnostics ship.
- **Real-time refresh.** The panel uses the existing 30s
  cache pattern from v0.89.61 (unified Discovery summary
  endpoint). No WebSocket subscription.
- **Per-environment / per-team filtering.** The aggregate
  spans the operator's entire connected fleet. Per-environment
  filtering is slice 2+.

## 3. Detection — already done

Slice 1 of Workload Health reuses the existing
detection rules:

- **Cold-start latency**: ratio ≥ 1.5x AND current_p95 ≥
  500ms AND baseline samples ≥ 50 (cold-start arc).
- **Sampling rate**: ratio < 0.05 AND invocations ≥ 1000
  (sampling rate arc).
- **Error rate**: rate ratio > 2.0x AND invocations ≥ 1000
  AND errors ≥ 50 (error rate arc).

Each diagnostic has its own `ShouldFireRecommendation()`
method. The panel aggregation walks the serverless
inventory and counts resources where each method returns
true.

## 4. API surface

### 4.1 New endpoint: `GET /api/v1/discovery/workload_health`

```json
{
  "providers": {
    "aws": {
      "serverless_resource_count": 47,
      "cold_start_exceeded_count": 5,
      "cold_start_exceeded_pct": 10.6,
      "sampling_too_aggressive_count": 3,
      "sampling_too_aggressive_pct": 6.4,
      "error_rate_spike_count": 2,
      "error_rate_spike_pct": 4.3,
      "any_issue_count": 8,
      "any_issue_pct": 17.0
    },
    "gcp": { ... },
    "azure": { ... },
    "oci": { ... }
  },
  "totals": {
    "serverless_resource_count": 142,
    "cold_start_exceeded_count": 12,
    "cold_start_exceeded_pct": 8.5,
    "sampling_too_aggressive_count": 8,
    "sampling_too_aggressive_pct": 5.6,
    "error_rate_spike_count": 5,
    "error_rate_spike_pct": 3.5,
    "any_issue_count": 22,
    "any_issue_pct": 15.5
  }
}
```

The `any_issue_count` is the count of resources where AT
LEAST ONE of the three diagnostics fires. Useful for the
"workload health summary" headline number.

**Aggregation source:** the endpoint walks the existing
`cold_start_observation`, `error_rate_observation`, and
the in-memory traceindex sampling counters per resource.
Same 30s in-memory cache pattern as the v0.89.61 summary
endpoint.

### 4.2 No inventory changes

The panel uses the new endpoint only. Per-Inventory-row
Cold-start P95 + Sampling rate + Error rate columns from
the existing arcs stay unchanged.

## 5. UI

### 5.1 Panel placement

The Discovery dashboard at `/discovery` currently shows:

1. TRACE COVERAGE panel (v0.89.76)
2. SPAN QUALITY panel (v0.89.87, extended to 6 columns
   in v0.89.124)

Slice 1 of Workload Health adds a third panel:

3. **WORKLOAD HEALTH (SERVERLESS)** panel — between TRACE
   COVERAGE and SPAN QUALITY for vertical narrative flow:
   coverage → workload health → span quality.

### 5.2 Panel structure

3-column health grid:

```
WORKLOAD HEALTH (SERVERLESS)

  Cold-start             Sampling too           Error rate
  P95 exceeded           aggressive             spike
       10.6%                 6.4%                  4.3%
   12 resources          8 resources          5 resources

  Total resources with at least one issue: 22 / 142 (15.5%)
```

Each column:
- Title (kind-friendly name)
- Headline % (totals.cold_start_exceeded_pct, etc.)
- Resource count
- Clickable — deep-links to per-provider Recommendations tab
  filtered by the corresponding kind prefix
  (lambda-cold-start-baseline / cloudrun-cold-start-baseline
  / etc. for cold-start; span-quality-sampling-too-aggressive
  for sampling; span-quality-error-rate-spike for error rate)

Footer line: aggregate "any issue" count + percentage.

### 5.3 Hide conditions

Panel hides when:
- `totals.serverless_resource_count == 0` (no serverless
  inventory)
- OR all 3 percentages are zero (the inventory exists but
  is healthy)

Same hide pattern as SPAN QUALITY and TRACE COVERAGE
sub-indicator from earlier arcs.

### 5.4 Color palette

Headline numbers use the same amber-on-non-zero pattern as
SPAN QUALITY (text-amber-300 when value > 0). Resource
counts use slate. Footer line uses slate.

No new color tokens.

### 5.5 Loading / error states

The panel shares the same loading state as the dashboard
endpoint pattern from v0.89.61. Errors surface as a small
muted message; the rest of the dashboard renders normally.

## 6. Slice 1 contract

**In:**

1. New backend endpoint `GET /api/v1/discovery/workload_health`
   with 30s cache.
2. Per-provider + totals aggregation walking
   `cold_start_observation`, `error_rate_observation`, and
   traceindex sampling counters.
3. UI panel "WORKLOAD HEALTH (SERVERLESS)" between TRACE
   COVERAGE and SPAN QUALITY on the Discovery dashboard.
4. Click-through deep-links to filtered Recommendations
   per kind.
5. Hide-when-zero behavior.
6. Operator runbook section in a sibling location OR a
   new doc.
7. Acceptance tests covering the aggregation math, the
   hide condition, the deep-link wiring, and cold-start
   parity.

**Out:**

- Per-cloud breakdown chips in the panel.
- Time-series trend sparklines.
- Drilldown detail modal.
- Compute / database / kubernetes inclusion.
- Real-time refresh.
- Per-environment filtering.

## 7. Implementation chunks

- **Chunk 1: Backend endpoint + aggregation + UI panel.**
  ~900-1100 lines. NEW handler with the existing 30s cache
  pattern, the panel component on Discovery.tsx, the
  deep-link wiring. **v0.89.132.**
- **Chunk 2: Operator runbook + README index.**
  ~300-400 lines. **v0.89.133.**

Total: 2 release tags. Smallest arc shipped to date because
the substrate's three diagnostics are entirely in place;
slice 1 is purely surfacing existing data at a new endpoint
+ panel.

## 8. Acceptance tests

1. **Aggregation includes only serverless resources** —
   compute/db/k8s inventory rows don't contribute.
2. **Cold-start exceeded count counts resources whose
   latest 24h cold-start observation exceeds threshold**.
3. **Sampling too aggressive count counts resources whose
   span / invocation ratio is below 5% with >= 1000
   invocations** (matches the existing sampling rate
   detection).
4. **Error rate spike count counts resources whose latest
   24h error rate observation passes all 3 error rate
   gates**.
5. **Any-issue count uses UNION semantics** (a resource
   that fires both cold-start AND sampling counts as 1
   any-issue, not 2).
6. **30s cache returns the same response within window**.
7. **Cache miss emits audit event
   `discovery.workload_health.requested`**.
8. **Panel hides when serverless_resource_count is zero**.
9. **Panel hides when all 3 percentages are zero**.
10. **Panel renders when at least one percentage is non-zero**.
11. **Click on Cold-start column deep-links to
    Recommendations with filter prefix
    `*-cold-start-baseline`**.
12. **Click on Sampling column deep-links to
    Recommendations with filter
    `span-quality-sampling-too-aggressive`**.
13. **Click on Error rate column deep-links to
    Recommendations with filter
    `span-quality-error-rate-spike`**.
14. **Footer count = sum of unique resources with any
    issue** (matches any_issue_count from the endpoint).
15. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.130 when no workload health
    rows trigger recommendations.

## 9. Threat model

**No new external surface.** The new endpoint reads from
existing storage tables and the in-memory traceindex. No
cloud API calls beyond what cold-start / sampling / error
rate arcs already make.

**Cache behavior.** The 30s in-memory cache mirrors the
existing v0.89.61 summary endpoint. Cache invalidation on
scan completion is slice 2 candidate.

**Aggregation cost.** Walking the serverless inventory
once per cache miss is O(N) where N = serverless resource
count. For typical fleets (≤10K functions across all
clouds), the aggregation runs in <100ms. Slice 2 may add
incremental aggregation when fleet sizes warrant.

**No span content logging.** Aggregates only; individual
resource identities flow through the existing audit chain
that the substrate's per-resource endpoints already use.
PII surface stays at zero.

**False positives propagate from underlying detectors.**
If cold-start's 1.5x ratio triggers a false positive on a
permanently-warm Cloud Run service, it counts in the
workload health panel. Same for sampling and error rate
false positives. The exclusion table from #531 slice 2
chunk 4 + verdict learning loop work the same way they
do today; slice 1 of Workload Health is downstream of
that filtering.

## 10. Slice 2 candidates

- Per-cloud breakdown chips in the panel
  (e.g. `aws 12% | gcp 8% | azure 5% | oci 2%` per column).
- Time-series trend sparklines on each column showing
  the last 7 days of percentages.
- Drilldown detail modal listing the specific resources
  contributing to each percentage.
- Compute / database / kubernetes inclusion when those
  diagnostics ship.
- Real-time refresh via SSE when a scan completes.
- Per-environment filtering when Squadron ingests
  environment tags consistently.
- "Healthy fleet" celebratory empty state instead of
  panel-hidden when a fleet is genuinely healthy.
- Recommendation count badge per column (currently the
  percentage + resource count; could also show the open
  recommendation count for at-a-glance triage).

---

**Strategic frame:**

This is a polish arc — the substrate has paid for itself
three times over already. The Workload Health panel
exposes the substrate's three diagnostics at the dashboard
level where multi-cloud operators get a one-glance
serverless health picture without paging through 4
per-provider pages.

The universal claim doesn't grow a new verb or new tier.
What changes is the SURFACE — the existing diagnostic
work becomes visible at the dashboard's primary entrypoint.

The Tuesday LinkedIn drumbeat narrative gains the most
operator-friendly framing yet: "Open Squadron's dashboard.
TRACE COVERAGE tells you if telemetry is flowing.
WORKLOAD HEALTH tells you if your serverless fleet is
healthy across three dimensions — latency, throughput,
errors. SPAN QUALITY tells you if the spans you receive
are diagnostically usable. Three panels. One screen. Then
drill into whichever's flashing amber."
