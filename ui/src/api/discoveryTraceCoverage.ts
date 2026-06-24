// API client for the v0.89.76 #707 Stream 105 (Trace integration
// slice 1 chunk 3) Discovery trace coverage endpoint. Pairs with
// internal/api/handlers/discovery_trace_coverage.go — the wire shapes
// here mirror the Go ProviderTraceCoverage / TraceCoverageTotals /
// TraceCoverageResponse structs exactly. Keep these in sync if the
// backend shape evolves.
//
// Slice-1 honesty (per docs/proposals/trace-integration-slice1.md):
//   - The endpoint is read-only; this module exposes one fetcher
//     (getTraceCoverage) the Discovery dashboard polls in parallel
//     with /discovery/summary on mount + refresh.
//   - The server caches the response for 30s (DefaultTraceCoverageCacheTTL
//     in the Go handler); a quick refresh inside that window returns
//     the cached payload without re-walking the four provider stores
//     or the traceindex. The UI displays the local fetch timestamp,
//     not the server's cachedAt — same posture as discoverySummary.ts.
//   - A fresh install with no clouds connected AND no spans observed
//     still gets a 200; every provider key is populated as zero-count.
//     The dashboard renders an empty state inside the panel rather
//     than hiding it.
//   - Same fetch wrapper + Bearer-token discipline as the rest of the
//     /discovery surface via ./base.

import { apiGet } from "./base";

// --- Per-provider trace coverage -----------------------------------

// ProviderTraceCoverage mirrors the Go ProviderTraceCoverage struct.
// coverage_pct is server-side zero-safe (returns 0 when inventory_count
// is 0, NOT NaN — design doc §11 acceptance test 11). strong_match_pct
// + weak_match_pct describe the slice-1 binary confidence split: a
// resource matched via cloud.resource_id or host.id keys strong, a
// resource matched via host.name or service.name alone keys weak. The
// dashboard renders a caveat icon next to providers whose
// weak_match_pct exceeds 20%. last_index_update_at is omitted
// (undefined) on cold start; the dashboard renders "—" in that case.
export type ProviderTraceCoverage = {
  inventory_count: number;
  emitting_count: number;
  coverage_pct: number;
  strong_match_pct: number;
  weak_match_pct: number;
  last_index_update_at?: string;
  /**
   * v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3) —
   * count of inventory rows for this provider where primitive_enabled
   * is true AND last_seen_at is null or older than 24h. Surfaces the
   * slice-2 "instrumented but not emitting" gap; the dashboard sub-
   * indicator renders when the fleet-wide sum is non-zero and hides
   * otherwise (design doc §10 acceptance test 10). Required field —
   * the backend always populates it (0 on cold start).
   */
  pending_trace_emission_count: number;
  /**
   * serverless_pct — serverless tier slice 1 chunk 5 (v0.89.92,
   * #725 Stream 123). Per-provider serverless coverage rendered as
   * emitting / inventory * 100 over the serverless-only sub-counts.
   * A function counts as emitting when last_seen_at is within 24h
   * (design doc §6.4). Zero on cold start; the dashboard hides the
   * SERVERLESS chip line when the fleet-wide sum is 0 (design doc
   * §7 + §11 acceptance test 13). Required field — the backend
   * always populates it.
   */
  serverless_pct: number;
};

// --- Cross-provider totals -----------------------------------------

// TraceCoverageTotals mirrors the Go TraceCoverageTotals struct.
// coverage_pct is server-side as emitting / inventory * 100
// (zero-safe, rounded to one decimal). The UI uses it directly for
// the panel's headline percentage + the ring threshold band.
export type TraceCoverageTotals = {
  inventory_count: number;
  emitting_count: number;
  coverage_pct: number;
};

// --- Full response -------------------------------------------------

// TraceCoverage mirrors the Go TraceCoverageResponse struct. The
// providers map is always keyed by the four provider strings so the
// dashboard can render a deterministic per-provider chip row even
// when some providers are disabled. The four chips render as
// "aws 67% | gcp 40% | azure 55% | oci 30%" in the panel footer.
export type TraceCoverage = {
  providers: {
    aws: ProviderTraceCoverage;
    gcp: ProviderTraceCoverage;
    azure: ProviderTraceCoverage;
    oci: ProviderTraceCoverage;
  };
  totals: TraceCoverageTotals;
};

// getTraceCoverage fetches the Discovery trace coverage payload from
// the v0.89.76 chunk-3 backend endpoint. Returns the parsed JSON;
// surfaces network / 401 / 5xx errors via the shared simpleRequest
// helper. Pair with useSWR("/discovery/trace_coverage",
// getTraceCoverage) for the page-level cache.
export async function getTraceCoverage(): Promise<TraceCoverage> {
  return apiGet<TraceCoverage>("/discovery/trace_coverage");
}
