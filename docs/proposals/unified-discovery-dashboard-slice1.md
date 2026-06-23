# Unified Discovery dashboard — slice 1 design

**Status:** design doc, locked for slice 1 implementation. Arc
opens after the four cloud arcs (AWS / GCP / Azure / OCI) all
closed slice 1: Squadron now scans four major clouds, but the
operator UI still requires clicking through four separate pages
(`/discovery/aws`, `/discovery/gcp`, `/discovery/azure`,
`/discovery/oci`) to see the full picture.

Slice 1 of this arc adds a unified entry point at `/discovery`
(no provider suffix) that aggregates connection counts, scan
inventory, recommendation queue, and verdict-learning signal
across all four clouds into a single dashboard. The strategic
frame: this is the **visual story** of universal observability.
Operators see one screen and immediately understand Squadron's
four-cloud value proposition. It's the moment the runbook claim
"Squadron is the universal observability control plane that
scans AWS, GCP, Azure, AND Oracle Cloud fleets" becomes
operator-visible in a single glance.

**See also:**
[discovery-iac-first-time-setup.md](../discovery-iac-first-time-setup.md),
[discovery-gcp-first-time-setup.md](../discovery-gcp-first-time-setup.md),
[discovery-azure-first-time-setup.md](../discovery-azure-first-time-setup.md),
[discovery-oci-first-time-setup.md](../discovery-oci-first-time-setup.md).

## 1. Problem

After the AWS / GCP / Azure / OCI slice 1 closes, an operator
with multi-cloud fleets must click through four separate pages
to see the full discovery picture. Each page renders its own
Wizard / Inventory / Recommendations tabs scoped to one provider.
The aggregate "how much of my fleet is uninstrumented across all
clouds" question has no answer in the UI — only in the audit log
via custom queries.

This works for operators with one cloud, or operators who already
know exactly which provider they need to inspect. It does not work
for:

- **The first-time evaluation.** An operator evaluating Squadron
  against a multi-cloud spec wants to see "Squadron covers my
  fleet" in one screen, not after four clicks.
- **Daily operator triage.** What's the most pressing
  recommendation across all my clouds? Today: four pages, four
  lists. With a unified view: one ranked queue.
- **Reporting.** "How many instances does Squadron see across all
  providers, and what fraction are instrumented?" needs to be
  derivable from the UI for monthly reporting, not just from
  audit query.
- **Demo moments.** The four-cloud claim Squadron makes
  externally needs internal visual support. A LinkedIn post that
  says "Squadron now scans 4 clouds" lands harder when the
  product itself shows that surface in one screen.

The fix is a unified Discovery dashboard at `/discovery` that
aggregates without replacing. The per-provider pages stay (they're
the wizard / deep-dive surfaces) and the new dashboard becomes
the default landing experience for the Discovery section.

## 2. Non-goals (slice 1)

- **Cross-provider recommendation deduplication.** A recommendation
  for an EC2 instance and a recommendation for a GCE instance
  are separate items; slice 1 surfaces both. Smart deduplication
  (e.g., "you have the same observability gap across providers,
  here's one batched action") is slice 2.
- **Cross-provider topology view.** A graph showing how
  resources across clouds connect (e.g., an AWS Lambda calling
  a GCP Cloud Function). Slice 2+ — needs trace data integration.
- **Cross-provider rollout / drift correlation.** Surface the
  proposer's verdict-learning state aggregated across providers.
  Slice 2.
- **Editing connections from the dashboard.** Slice 1 surfaces
  read-only counts; operators click through to per-provider
  pages for create / edit / delete operations.
- **Real-time SSE updates.** Slice 1 polls the summary endpoint
  on tab focus (and on manual refresh). Real-time push is slice
  2 — not blocking for the visual story.
- **Per-account / per-project drill-down within a provider.**
  The per-provider page already shows that. Slice 1 keeps the
  drill-down on the per-provider page.

## 3. Architectural decision

Two real options for the aggregation layer:

### Option A — Backend aggregation endpoint

A new `/api/v1/discovery/summary` endpoint that queries all four
provider stores + recent scan audit events, composes a single
JSON response, and returns. Front-end fetches once on dashboard
load.

**Picked for slice 1.** Single round-trip from the UI. Easy to
test (one endpoint, one shape). Backend has more visibility into
each store's auth model. Cache-friendly if needed in slice 2.

### Option B — Frontend aggregation

Front-end calls four list endpoints in parallel and composes
client-side.

**Rejected for slice 1.** Four parallel requests with four
different auth responses means more error states to handle in
the UI, and the aggregation logic duplicates per-provider call
shapes. Slice 1 picks the backend approach for simplicity.

## 4. Response shape

`GET /api/v1/discovery/summary` returns:

```json
{
  "providers": {
    "aws": {
      "connection_count": 3,
      "last_scan_at": "2026-06-23T10:00:00Z",
      "instance_count": 142,
      "instrumented_count": 89,
      "uninstrumented_count": 53,
      "recommendation_count": 53,
      "enabled": true
    },
    "gcp": {
      "connection_count": 1,
      "last_scan_at": "2026-06-23T09:30:00Z",
      "instance_count": 24,
      "instrumented_count": 18,
      "uninstrumented_count": 6,
      "recommendation_count": 6,
      "enabled": true
    },
    "azure": { ... },
    "oci": { ... }
  },
  "totals": {
    "connection_count": 5,
    "instance_count": 198,
    "instrumented_count": 132,
    "uninstrumented_count": 66,
    "recommendation_count": 66,
    "coverage_pct": 66.7
  },
  "recent_recommendations": [
    {
      "provider": "aws",
      "kind": "ec2-otel-tag",
      "resource_id": "i-0abc...",
      "scope_id": "123456789012",
      "region": "us-east-1",
      "generated_at": "2026-06-23T10:00:00Z"
    }
    // up to 10 most recent across providers
  ]
}
```

The `enabled` flag per provider is `true` when the corresponding
store is wired in the deployment. Operators without OCI configured
see `oci.enabled=false` so the dashboard renders the OCI card
in a "Connect OCI to add to your fleet view" empty state instead
of pretending zero instances exist.

`coverage_pct` is computed server-side as
`instrumented_count / instance_count * 100` (zero-safe).

## 5. Aggregation logic

The handler walks each provider's store:

- **AWS**: existing `awsconnstore.Store.List()` for connections;
  most recent `discovery.aws.scan_completed` audit event per
  connection for the inventory counts.
- **GCP**: `gcpconnstore.Store.List()` + most recent
  `discovery.gcp.scan_completed` audit per connection.
- **Azure**: parallel.
- **OCI**: parallel.

For each provider:
1. Count connections.
2. Find the most recent `scan_completed` audit per connection.
3. Sum `instance_count`, `instrumented_count`,
   `uninstrumented_count` from those audit payloads.
4. `last_scan_at` = max timestamp across the per-connection
   scan_completed events.

For `recommendation_count`, slice 1 uses `uninstrumented_count`
as the proxy. The actual recommendation surface is computed on
demand by the proposer; without storing recommendation rows
(which is a separate slice 2 candidate per #531 slice 2 §10 Q3),
the count of uninstrumented resources is the best proxy.

For `recent_recommendations`, slice 1 queries the
`discovery_proposal.created` audit events across all providers,
limit 10, ordered by timestamp DESC. The payload already carries
the verdict_examples_used_by_state field from #531 slice 2 chunk
6, so we can show the proposer's reasoning context inline.

### 5.1 Performance considerations

The aggregation is four parallel queries against four stores + an
audit table scan. Total cost: ~5 queries with the existing
indexes. On a deployment with hundreds of connections + millions
of audit events, this could be slow.

Slice 1 ships unoptimized but with `cache_ttl=30s` in-memory
caching on the summary response. Operators clicking around the
dashboard see the cached response; the next 30-second window
refreshes.

Slice 2 candidate: pre-computed materialized rollup table updated
on each scan.

## 6. UI structure

`/discovery` becomes the new dashboard. Existing per-provider
pages stay at `/discovery/aws`, `/discovery/gcp`, etc.

The dashboard structure:

**Header row:**
- Page title: "Discovery"
- Subtitle: "Squadron sees N resources across X providers"
- Last refreshed timestamp + manual refresh button

**Coverage panel (top):**
- Large number: `coverage_pct` (e.g., "67%")
- Subtext: "X of Y instances instrumented across all providers"
- Color-coded ring (green > 80%, yellow 50-80%, red < 50%)

**Provider cards (4-card grid):**
Each card shows:
- Provider name + logo (AWS / GCP / Azure / OCI)
- Connection count
- Instance count
- Coverage_pct for that provider
- "View details →" link to per-provider page
- For providers with `enabled=false`: "Connect <provider> to add
  to your fleet view" + button linking to wizard

**Recent recommendations table (bottom):**
- 10 most recent recommendations across all providers
- Columns: Provider | Kind | Resource | Scope | Region | Generated
- Click row → opens the recommendation in the per-provider page

**Empty state:**
When all 4 providers show `enabled=false` (fresh Squadron
install), the dashboard renders a welcome state:
"Welcome to Squadron Discovery. Connect your first cloud to start
seeing observability gaps." with four "Connect AWS" / "Connect
GCP" / "Connect Azure" / "Connect OCI" buttons.

## 7. Slice 1 contract

**In:**

1. New backend handler `internal/api/handlers/discovery_summary.go`
   exposing `GET /api/v1/discovery/summary`.
2. The handler queries all four provider stores + audit events
   and composes the response per §4.
3. In-memory cache with 30s TTL.
4. New audit event constant
   `AuditEventDiscoverySummaryRequested = "discovery.summary.requested"`
   emitted on each cache-miss request (cache-hit requests don't
   re-emit).
5. Server.go route registration: `GET /api/v1/discovery/summary`
   under the existing auth middleware.
6. New UI page `ui/src/pages/Discovery.tsx` at route `/discovery`.
7. New API helper
   `ui/src/api/discovery.ts::getDiscoverySummary()`.
8. Sidebar update: the existing "Discovery" group header in the
   sidebar gets a click handler that navigates to `/discovery`
   (the unified dashboard) instead of being just a label.
9. Acceptance tests:
   - Backend: TestDiscoverySummary_AggregatesAllFourProviders
   - Backend: TestDiscoverySummary_DisabledProvidersShowZero
   - Backend: TestDiscoverySummary_CacheTTLBehavior
   - Backend: TestDiscoverySummary_RecentRecommendationsLimitedTo10
   - Backend: TestDiscoverySummary_EmitsAuditOnCacheMiss
   - Backend: TestDiscoverySummary_CoveragePctZeroSafe
   - Frontend: TestDiscoveryDashboard_RendersFourProviderCards
   - Frontend: TestDiscoveryDashboard_EmptyStateWhenNoConnections
   - Frontend: TestDiscoveryDashboard_CoverageRingColorByThreshold
   - Frontend: TestDiscoveryDashboard_RecentRecommendationsTable

**Out:**
- Smart deduplication across providers
- Topology graph
- Cross-provider rollout correlation
- Real-time SSE updates
- Edit connections from dashboard
- Per-account drill-down

## 8. Open questions

1. **Cache TTL value.** 30s feels right for slice 1 (operators
   rarely refresh within 30s, and the cost of one full
   aggregation per 30s is bounded). Slice 2 may tune based on
   usage data.
2. **What about cost-spike data on the dashboard?** The
   four-cloud claim is observability-focused, but Squadron also
   does cost-spike alerting. Should the dashboard surface a "cost
   anomalies last 7 days" card? Slice 1 says no — keep the
   discovery focus pure; cost goes in the existing Cost page.
   Slice 2 candidate.
3. **What about IaC connection summary?** The IaC GitHub
   connection is the substrate for Open PR. Slice 1 doesn't
   surface IaC connection counts on the dashboard — that's a
   separate Settings concern.
4. **Authorization scope.** The summary endpoint requires the
   same bearer auth as other discovery endpoints. Should there
   be a fine-grained scope like `discovery:summary:read`? Slice
   1 reuses the general `discovery:read` scope; slice 2 may add
   granular.

## 9. Acceptance tests in detail

(See §7 contract item 9 for the list.) Notable details:

- **TestDiscoverySummary_AggregatesAllFourProviders**: seed each
  provider's store with 1 connection + 1 scan_completed audit.
  Assert response totals.instance_count is the sum across all
  four providers.
- **TestDiscoverySummary_DisabledProvidersShowZero**: stub the
  OCI store to nil. Assert response.providers.oci.enabled=false
  and counts are all zero.
- **TestDiscoverySummary_CacheTTLBehavior**: call twice within
  30s with the same state. Assert only one audit event emitted
  (the cache-miss one).
- **TestDiscoveryDashboard_EmptyStateWhenNoConnections**: mock
  summary endpoint to return all zeros. Assert: welcome state
  visible, four Connect buttons present, coverage ring NOT
  rendered.

## 10. Implementation chunks

Tighter than the cloud-arc chunks because patterns are settled:

- **Chunk 1: Backend aggregation endpoint + tests.** ~500-700
  lines. v0.89.61. Single subagent, no parallelism needed.
- **Chunk 2: UI dashboard page + tests + sidebar wiring.**
  ~700-900 lines. v0.89.62. Single subagent.
- **Chunk 3: Operator-facing runbook update.** Update
  `docs/README.md` and existing per-provider runbooks to mention
  the dashboard. Small. Bundled into chunk 2 commit or its own
  small release.

Total: 2-3 release tags. No design-doc heavy lift beyond this
locked spec.

## 11. Slice 2+ candidates

- Smart cross-provider recommendation deduplication
- Topology graph showing cross-provider resource relationships
  (needs trace integration)
- Cost-spike + drift surface integration (cross-cutting concerns
  on the dashboard)
- Real-time SSE updates instead of cache poll
- Edit connections directly from dashboard cards
- Per-account / per-project / per-subscription drill-down within
  cards
- Pre-computed materialized rollup table for very large
  deployments
- Fine-grained `discovery:summary:read` scope

---

**Strategic frame:**

This dashboard is the **demo moment** for Squadron's four-cloud
claim. The runbook says "scans AWS, GCP, Azure, AND Oracle Cloud
fleets" — the dashboard MAKES that visible in one screen.

Until this lands, an operator clicking around the Squadron UI
sees four separate /discovery/* pages with no aggregate view.
The four-cloud claim feels theoretical. After this lands, the
operator opens `/discovery`, sees "Squadron sees 198 resources
across 4 providers, 67% instrumented", and the claim becomes
concrete.

For the LinkedIn / GTM story, the dashboard is the screenshot.
A screenshot of `/discovery/aws` is "Squadron scans AWS." A
screenshot of `/discovery` showing the four-card grid + coverage
ring is "Squadron is the universal observability control plane."
The difference matters.

The implementation cost is modest (2-3 chunks) relative to the
strategic value. Slice 1 ships the visual; slice 2 makes it
smarter with deduplication + topology.
