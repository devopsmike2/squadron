# Workload Health panel — operator guide

This is the operator-facing runbook for the v0.89.131 through
v0.89.133 Workload Health dashboard panel arc. The Discovery
dashboard at `/discovery` now has a third panel between TRACE
COVERAGE and SPAN QUALITY that consolidates the substrate's
three serverless diagnostics into a single one-glance view.

The strategic frame: the metric correlation substrate
(v0.89.113 + v0.89.118) has shipped three diagnostics —
cold-start latency, sampling rate, error rate. Each diagnostic
already surfaces per-resource on the per-cloud Discovery
pages' Serverless tables. The Workload Health panel adds the
fleet-wide aggregate at the dashboard's primary entrypoint.

This is a polish arc. No new substrate, no new metrics, no
new recommendation kinds. Existing diagnostic work becomes
visible at one new surface.

## What this is good for

- A multi-cloud operator opening Squadron at `/discovery`
  who wants a one-glance health check of the serverless
  fleet before deciding where to drill in.
- An SRE team that wants to spot trends across all 4 clouds
  at once instead of paging through 4 per-provider Discovery
  pages.
- A platform team running a Monday-morning "what shipped
  over the weekend, what broke" review who wants the
  dashboard summary first.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of Workload Health is
intentionally narrow:

- **Per-cloud breakdown chips inside the panel.** Slice 1
  shows aggregate percentages across all 4 clouds. The
  per-cloud breakdown is already on each provider's
  Discovery page (Serverless table columns from the
  cold-start, sampling rate, error rate arcs). Slice 2
  may add `aws 12% | gcp 8% | azure 5% | oci 2%` style
  chips per column.
- **Time-series trend sparklines.** Each column is a
  point-in-time percentage. Slice 2+ may add 7-day trend
  sparklines.
- **Drilldown detail modal.** Click goes to the
  Recommendations tab filtered by kind. There's no inline
  list of contributing resources in slice 1.
- **Compute / database / kubernetes inclusion.** The
  substrate's three diagnostics ship for serverless only
  in their current slices. The panel is honestly scoped
  "WORKLOAD HEALTH (SERVERLESS)" in its title.
- **Real-time refresh.** The panel uses a 30s in-memory
  cache matching the v0.89.61 summary endpoint pattern. A
  scan that just completed may not be reflected until the
  cache expires.
- **Per-environment / per-team filtering.** Aggregate spans
  the whole connected fleet.
- **"Healthy fleet" celebratory empty state.** The panel
  hides when all three percentages are zero. Slice 2 may
  add a green "Healthy fleet — all serverless resources
  passing thresholds" empty state.

## The three diagnostic columns

The panel shows 3 columns, one per substrate diagnostic.
Each column corresponds to one of the substrate's
ShouldFireRecommendation predicates:

### Cold-start P95 exceeded

Counts resources where the latest 24h cold-start observation
exceeds the threshold:
- `ratio >= 1.5x` (vs 7d baseline)
- AND `current_p95 >= 500ms` (absolute floor)
- AND `baseline samples >= 50` (statistical confidence)

The 24h observation is the per-resource result from the
cold-start latency arc (v0.89.112-120). The percentage in
the column is `(resources exceeding) / (total serverless
resources) * 100`.

Click → deep-link to `/discovery/aws#recommendations:cold-start`
where the Recommendations tab is filtered to all cold-start
kinds across surfaces (`lambda-cold-start-baseline`,
`cloudrun-cold-start-baseline`, `cloudfunc-cold-start-baseline`,
`azfunc-cold-start-baseline`, `ocifunc-cold-start-baseline`).

### Sampling too aggressive

Counts resources where the latest 24h sampling observation
exceeds the threshold:
- `ratio < 0.05` (5%)
- AND `invocation_count >= 1000`

The ratio is `observed_span_count / expected_invocation_count`
per the sampling rate arc (v0.89.121-125).

Click → deep-link to `/discovery/aws#recommendations:span-quality-sampling-too-aggressive`.

### Error rate spike

Counts resources where the latest 24h error rate observation
exceeds all three gates:
- `current_error_rate / baseline_error_rate > 2.0`
- AND `current_invocation_count >= 1000`
- AND `current_error_count >= 50`
- Near-zero baseline guard applies (0.01% floor)

The observation comes from the error rate arc (v0.89.126-130).

Click → deep-link to `/discovery/aws#recommendations:span-quality-error-rate-spike`.

## The any-issue footer

Below the 3-column grid, a footer line summarizes:

> **Total resources with at least one issue: 22 / 142 (15.5%)**

This uses **UNION semantics** — a resource that fires both
cold-start AND sampling AND error rate counts as ONE
any-issue, not three. A clean fleet would have any_issue at
zero; a 100% problem fleet would have any_issue at 100%.

This is the single most operator-useful number: how many
serverless resources need attention across all three
diagnostics combined?

## When the panel hides

The panel hides when:
1. `serverless_resource_count == 0` (no serverless inventory
   exists)
2. OR all three percentages are zero (the inventory exists
   but is healthy)

This matches the hide-when-zero pattern from TRACE COVERAGE
sub-indicator (v0.89.82) and SPAN QUALITY panel (v0.89.87).
The dashboard layout collapses cleanly without an empty
amber-when-nothing-to-show placeholder.

## The panel placement

The Discovery dashboard renders three panels in this order:

1. **TRACE COVERAGE** — "is telemetry flowing?"
2. **WORKLOAD HEALTH (SERVERLESS)** — "is the workload
   healthy?" (this arc)
3. **SPAN QUALITY** — "are the spans we receive
   diagnostically usable?"

The vertical narrative flow reads top-to-bottom:
coverage → workload health → span quality. Pinned by test
`TestDiscoveryDashboard_WorkloadHealthPanel_PlacedBetweenTraceCoverageAndSpanQuality`
using `compareDocumentPosition`.

## The 30s cache

Mirrors the v0.89.61 unified Discovery summary endpoint
pattern:

- First request after server start computes the aggregation
  + caches the response with a 30s TTL
- Within 30s, subsequent requests return the cached response
- After 30s, the next request triggers a recomputation +
  refreshes the cache

The cache miss emits a single audit event
`discovery.workload_health.requested` so operators can see
how often the dashboard is being pulled.

The 30s window is a deliberate tradeoff: longer windows save
compute but mask freshly-completed scans; shorter windows
keep the panel responsive but increase aggregation cost.
Slice 2 may add cache invalidation on scan completion to
get the best of both.

## The API endpoint

```
GET /api/v1/discovery/workload_health
```

Returns:

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

Operators can curl this directly for scripting or to wire
into their own dashboards. The shape is stable for slice 1;
slice 2 additions stay backward-compatible.

## Reading the audit

No new audit event types beyond the cache-miss surface:

- `discovery.workload_health.requested` — emitted on cache
  miss when the dashboard or a curl pulls fresh data.
  Audit-only; no side effects.

The recommendation lifecycle (`recommendation.created` /
`pr_opened` / `pr_merged` / `pr_closed`) carries kinds from
the underlying diagnostics — the panel doesn't introduce
new kinds.

## Workflow — first dashboard view

1. Open `/discovery`. The dashboard renders three panels:
   TRACE COVERAGE → WORKLOAD HEALTH → SPAN QUALITY.
2. If the WORKLOAD HEALTH panel renders with non-zero
   percentages, you have at least one serverless resource
   exceeding a substrate threshold.
3. Check the footer's any-issue count for the headline
   number. If it's 0 of N or below a tolerance you've set,
   move on. If it's above tolerance, drill into the
   highest-percentage column first.
4. Click the column header → land on AWS Recommendations
   tab filtered by the corresponding kind prefix.
5. Per-cloud picture lives on the per-provider Discovery
   pages' Serverless tables. The slice 1 panel intentionally
   aggregates across all 4 clouds; per-cloud chips are
   slice 2.

## Troubleshooting

- **Panel doesn't render but I know I have serverless
  inventory.** Three causes:
  1. All three percentages are zero (fleet is healthy →
     panel hides). Check the per-resource columns on
     each provider's Serverless table to confirm.
  2. The serverless inventory reader returned an error.
     Check the audit log for
     `discovery.workload_health.requested` — if it's
     emitting but no panel renders, the aggregation may
     be returning a zero-count result.
  3. The new endpoint isn't wired in production yet
     (chunk 1 used a setter pattern; production wiring
     may be at nil). Check
     `internal/api/server.go::SetWorkloadHealthInventoryReader`
     wiring.
- **Cold-start P95 exceeded count is 5 but Recommendations
  tab shows 0 cold-start recommendations.** The
  count walks the cold_start_observation table; the
  Recommendations tab walks the proposer's draft list. If
  a resource has a 24h observation exceeding threshold but
  the proposer hasn't drafted a recommendation yet
  (proposer runs separately from the scan), the panel
  count leads the recommendation count. Re-run the
  proposer (or wait for the next proposer cycle).
- **Any-issue count is lower than the sum of the three
  column counts.** This is UNION semantics working
  correctly — a resource firing 2 of 3 diagnostics counts
  as 1 in any-issue, but 2 in the column sums. Pinned by
  `TestWorkloadHealth_AnyIssueUsesUnionSemantics`.
- **Panel shows stale data.** Cache TTL is 30s. Wait at
  least 30s after a scan completes for the cache to
  expire, then reload. Slice 2 may add cache invalidation
  on scan completion.
- **Per-provider breakdowns I see on the per-provider
  Discovery pages don't match the panel aggregate.** Two
  causes:
  1. Cache timing — the panel may be 30s stale relative
     to a fresh provider-page read.
  2. Aggregation rounding — the panel uses 1-decimal
     precision on the column percentages but full
     precision internally for the union count.
- **Panel shows non-zero but I clicked into a column and
  see no fresh recommendations.** The substrate detects
  threshold exceedance per scan; the proposer drafts
  recommendations on its separate cycle. A short delay
  between the two is expected. Refresh after the next
  proposer cycle.

## Per-cloud rate limits + cost surface

The panel reads from existing storage tables + the
in-memory traceindex. No new cloud API calls. The
aggregation runs in <100ms for typical fleets (≤10K
functions across all 4 clouds). Slice 2 may add
incremental aggregation when fleet sizes warrant.

No new cost surface beyond what cold-start, sampling rate,
and error rate arcs already incur. Per the no-money brief:
no new operator-facing decisions needed.

## What slice 2 will add

Per §10 of the design doc:

- Per-cloud breakdown chips (`aws 12% | gcp 8% | azure 5% |
  oci 2%`) on each column.
- Time-series trend sparklines showing the last 7 days of
  percentages.
- Drilldown detail modal listing the specific resources
  contributing to each percentage.
- Compute / database / kubernetes inclusion when those
  diagnostics ship.
- Real-time refresh via SSE when a scan completes.
- Per-environment / per-team filtering.
- "Healthy fleet" green celebratory empty state instead of
  hide-when-zero.
- Recommendation count badge per column.

## Strategic frame — surface polish

This is a polish arc. The substrate has paid for itself
three times over already (cold-start latency + sampling
rate + error rate). The Workload Health panel exposes the
substrate's three diagnostics at the dashboard's primary
entrypoint where multi-cloud operators get a one-glance
serverless health picture without paging through 4
per-provider pages.

The universal claim doesn't grow a new verb or new tier.
What changes is the SURFACE — the existing diagnostic
work becomes visible at the dashboard's primary
entrypoint:

> "Open Squadron's dashboard. TRACE COVERAGE tells you if
> telemetry is flowing. WORKLOAD HEALTH tells you if your
> serverless fleet is healthy across three dimensions —
> latency, throughput, errors. SPAN QUALITY tells you if
> the spans you receive are diagnostically usable. Three
> panels. One screen. Then drill into whichever's
> flashing amber."

Three panels. One screen. The Tuesday LinkedIn drumbeat
narrative gains the most operator-friendly framing yet
because the operator doesn't have to know which
recommendation kinds map to which problem — the panel
labels them in operator language: "Cold-start P95
exceeded" / "Sampling too aggressive" / "Error rate
spike."

## Cross-references

- [Workload Health panel slice 1 design doc](./proposals/workload-health-panel-slice1.md) —
  the locked spec this runbook operationalizes.
- [Cold-start latency operator guide](./cold-start-latency-operator-guide.md) —
  first substrate diagnostic; column 1 in the panel.
- [Sampling rate analysis operator guide](./sampling-rate-operator-guide.md) —
  second substrate diagnostic; column 2 in the panel.
- [Error rate correlation operator guide](./error-rate-correlation-operator-guide.md) —
  third substrate diagnostic; column 3 in the panel.
- [Unified Discovery dashboard slice 1](./proposals/unified-discovery-dashboard-slice1.md) —
  the dashboard surface this panel sits on, plus the 30s
  cache pattern this reuses.
- [Audit log](./audit-log.md) — full catalog of event types
  including the new `discovery.workload_health.requested`.
