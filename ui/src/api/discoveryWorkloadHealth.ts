// API client for the v0.89.132 #772 Stream 170 (Workload Health
// dashboard panel slice 1 chunk 1) Discovery workload health
// endpoint. Pairs with internal/api/handlers/discovery_workload_health.go
// — the wire shapes here mirror the Go ProviderWorkloadHealth /
// WorkloadHealthResponse structs exactly. Keep these in sync if the
// backend shape evolves.
//
// Slice-1 honesty (per docs/proposals/workload-health-panel-slice1.md):
//   - The endpoint is read-only; this module exposes one fetcher
//     (fetchWorkloadHealth) the Discovery dashboard polls in parallel
//     with /discovery/summary + /discovery/trace_coverage on mount +
//     refresh.
//   - The server caches the response for 30s
//     (DefaultWorkloadHealthCacheTTL in the Go handler); a quick
//     refresh inside that window returns the cached payload without
//     re-walking the per-provider serverless inventory. Same posture
//     as discoveryTraceCoverage.ts and discoverySpanQuality.ts.
//   - A fresh install with no clouds connected AND no serverless
//     inventory annotated still gets a 200; every provider key is
//     populated as zero-count. The dashboard panel hides itself per
//     the §5.3 hide conditions (serverless_resource_count == 0 OR all
//     three percentages zero).
//   - Same fetch wrapper + Bearer-token discipline as the rest of the
//     /discovery surface via ./base.

import { apiGet } from "./base";

// ProviderWorkloadHealth mirrors the Go ProviderWorkloadHealth struct.
// serverless_resource_count is the honest denominator — total
// serverless rows observed in the latest scan for this provider. The
// three count + three pct fields describe the per-diagnostic
// detection result rollup; any_issue_count uses UNION semantics so a
// resource firing both cold-start AND sampling counts as 1, not 2
// (design doc §4 + §8 acceptance test 5).
//
// All five percentages are server-side zero-safe (returns 0 when
// serverless_resource_count is 0, NOT NaN) and rounded to one
// decimal. The UI just renders the floats via .toFixed(1).
export type ProviderWorkloadHealth = {
  serverless_resource_count: number;
  cold_start_exceeded_count: number;
  cold_start_exceeded_pct: number;
  sampling_too_aggressive_count: number;
  sampling_too_aggressive_pct: number;
  error_rate_spike_count: number;
  error_rate_spike_pct: number;
  any_issue_count: number;
  any_issue_pct: number;
};

// WorkloadHealthResponse mirrors the Go WorkloadHealthResponse struct.
// providers is always keyed by the four provider strings ("aws",
// "gcp", "azure", "oci") so the dashboard renders deterministically
// even when some providers are disabled. totals is the bare
// cross-provider sum; the percentages are computed against the
// totals' serverless_resource_count denominator server-side.
export type WorkloadHealthResponse = {
  providers: {
    aws: ProviderWorkloadHealth;
    gcp: ProviderWorkloadHealth;
    azure: ProviderWorkloadHealth;
    oci: ProviderWorkloadHealth;
  };
  totals: ProviderWorkloadHealth;
};

// fetchWorkloadHealth fetches the Discovery workload health summary
// from the v0.89.132 chunk-1 backend endpoint. Pair with
// useSWR("/discovery/workload_health", fetchWorkloadHealth) for the
// page-level cache.
export async function fetchWorkloadHealth(): Promise<WorkloadHealthResponse> {
  return apiGet<WorkloadHealthResponse>("/discovery/workload_health");
}
