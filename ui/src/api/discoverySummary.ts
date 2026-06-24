// API client for the v0.89.62 #689 Stream 87 (slice-1 chunk 2) Unified
// Discovery dashboard. Pairs with internal/api/handlers/discovery_summary.go
// (v0.89.61 chunk 1) — the wire shapes here mirror the Go
// SummaryResponse / ProviderSummary / SummaryTotals / RecentRecommendation
// structs exactly. Keep these in sync if the backend shape evolves.
//
// Slice-1 honesty:
//   - The endpoint is read-only; this module exposes one fetcher
//     (getDiscoverySummary) that the dashboard page polls on mount
//     and on the manual refresh button.
//   - The server caches the response for 30s (DefaultSummaryCacheTTL
//     in the Go handler); a quick refresh inside that window will
//     return the cached payload without re-walking the four provider
//     stores. The UI displays "X ago" relative to the local fetch
//     timestamp, not the server's cachedAt — slice 1 honesty: an
//     operator who clicks Refresh inside the cache window sees the
//     fetch time advance but the underlying counts may not.
//   - Same fetch wrapper + Bearer-token discipline as the per-provider
//     discovery helpers via ./base.

import { apiGet } from "./base";

// --- Per-provider summary ------------------------------------------

// ProviderSummary mirrors the Go ProviderSummary struct. The Enabled
// flag distinguishes "this provider has zero connections" from "this
// deployment never wired the provider's store" — the dashboard
// renders a connect-state card when Enabled=false instead of
// pretending zero instances exist. last_scan_at is omitted (undefined)
// when no scan_completed audit row exists for any of the provider's
// connections; the UI shows "Never scanned" in that case.
export type ProviderSummary = {
  connection_count: number;
  last_scan_at?: string;
  instance_count: number;
  instrumented_count: number;
  uninstrumented_count: number;
  recommendation_count: number;
  // serverless_count — serverless tier slice 1 chunk 5 (v0.89.92,
  // #725 Stream 123). Per-provider count of serverless functions /
  // services the most recent scan_completed event surfaced. Zero on
  // cold start and on deployments that haven't yet observed a
  // scan_completed audit row carrying the serverless_count payload
  // field; the dashboard's per-provider card shows the count
  // unconditionally so an operator sees "0" instead of an absent
  // chip when the tier is wired but empty.
  serverless_count: number;
  enabled: boolean;
};

// --- Cross-provider totals -----------------------------------------

// SummaryTotals mirrors the Go SummaryTotals struct. coverage_pct is
// computed server-side as instrumented_count / instance_count * 100
// (zero-safe, rounded to one decimal). The UI uses it directly for
// the coverage ring + the cross-provider "67% instrumented" subtitle.
export type SummaryTotals = {
  connection_count: number;
  instance_count: number;
  instrumented_count: number;
  uninstrumented_count: number;
  recommendation_count: number;
  // serverless_count — serverless tier slice 1 chunk 5 (v0.89.92,
  // #725 Stream 123). Cross-provider sum of ProviderSummary
  // serverless_count.
  serverless_count: number;
  coverage_pct: number;
};

// --- Recent recommendation row -------------------------------------

// RecentRecommendation mirrors the Go RecentRecommendation struct.
// The dashboard renders up to 10 most-recent recommendations across
// all providers in a single table; clicking a row deep-links to the
// per-provider page (slice 1 lands on the provider page root, slice
// 2 deep-links to the specific recommendation). The provider literal
// drives both the badge color and the click-through path.
export type RecentRecommendation = {
  provider: "aws" | "gcp" | "azure" | "oci";
  kind: string;
  resource_id?: string;
  scope_id: string;
  region: string;
  generated_at: string;
};

// --- Full response -------------------------------------------------

// DiscoverySummary mirrors the Go SummaryResponse struct. The
// providers map is always keyed by the four provider strings so the
// dashboard can render a deterministic 4-card grid even when some
// providers are disabled. recent_recommendations is always an array
// (the Go handler explicitly initializes it to an empty slice rather
// than nil) so the UI never sees `null` and can short-circuit the
// table empty-state on `.length === 0`.
export type DiscoverySummary = {
  providers: {
    aws: ProviderSummary;
    gcp: ProviderSummary;
    azure: ProviderSummary;
    oci: ProviderSummary;
  };
  totals: SummaryTotals;
  recent_recommendations: RecentRecommendation[];
};

// getDiscoverySummary fetches the unified Discovery dashboard payload
// from the v0.89.61 chunk-1 backend endpoint. Returns the parsed
// JSON; surfaces network / 401 / 5xx errors via the shared
// simpleRequest helper. Pair with useSWR("/discovery/summary",
// getDiscoverySummary) for the page-level cache.
export async function getDiscoverySummary(): Promise<DiscoverySummary> {
  return apiGet<DiscoverySummary>("/discovery/summary");
}
