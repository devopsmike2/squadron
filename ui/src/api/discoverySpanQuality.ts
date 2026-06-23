// API client for the v0.89.86 chunk 2 Discovery Span Quality
// endpoints (#717 Stream 115, span quality slice 1). Pairs with the
// Go handler that exposes:
//
//   GET /api/v1/discovery/span_quality
//     → per-provider + totals summary (design doc §6.1).
//   GET /api/v1/discovery/{provider}/inventory/{kind}/{id}/span_quality
//     → per-resource detail incl. placeholder observations (§6.2).
//
// Mirrors the wire shape from
// internal/api/handlers/discovery_span_quality.go exactly. Keep these
// in sync if the backend shape evolves.
//
// The per-resource endpoint may legitimately return 404 — a resource
// Squadron has inventoried but for which NO spans have been observed
// in the current quality window has no counter row to surface.
// fetchResourceSpanQuality maps 404 to null so the caller renders a
// gray "no observations" dot instead of an error state.

import { apiGet, simpleRequest } from "./base";

// ProviderSpanQuality mirrors the Go ProviderSpanQuality struct.
// resource_count is the number of resources with at least one span
// observed in the current quality window (default 1h). The three
// percentages are fleet-rolled-up across that resource set, one
// decimal place server-side. resources_with_issues counts rows where
// at least one of the three percentages is non-zero — the dashboard
// renders that count below each column header. All five fields are
// required; the backend always populates them (0 on cold start).
export type ProviderSpanQuality = {
  resource_count: number;
  resources_with_issues: number;
  orphan_pct: number;
  missing_attr_pct: number;
  attr_mismatch_pct: number;
};

// SpanQualityTotals mirrors the Go SpanQualityTotals struct. Computed
// across the four-provider sum (NOT the average of per-provider
// percentages — the server rolls up raw span counts then computes
// the totals percentage). Drives the headline 3-column health grid +
// the panel-hidden-when-all-zero check (design doc §10 test 12).
export type SpanQualityTotals = ProviderSpanQuality;

// SpanQualityResponse mirrors the Go SpanQualityResponse struct. The
// providers map is always keyed by the four provider strings so the
// dashboard renders deterministically even when some providers are
// disabled.
export type SpanQualityResponse = {
  providers: {
    aws: ProviderSpanQuality;
    gcp: ProviderSpanQuality;
    azure: ProviderSpanQuality;
    oci: ProviderSpanQuality;
  };
  totals: SpanQualityTotals;
};

// PlaceholderObservation carries the {attribute, placeholder} pair
// observed plus the wall-clock timestamp of first observation in the
// current window. Used by the per-resource drill-down panel only.
export type PlaceholderObservation = {
  attribute: string;
  placeholder: string;
  seen_at: string;
};

// ResourceSpanQuality mirrors the Go ResourceSpanQuality struct.
// has_issues is a server-side convenience flag — TRUE iff at least
// one of the three percentages is non-zero.
export type ResourceSpanQuality = {
  resource_id: string;
  total_spans: number;
  window_start: string;
  orphan_pct: number;
  missing_attr_pct: number;
  attr_mismatch_pct: number;
  placeholders: PlaceholderObservation[];
  has_issues: boolean;
};

// fetchSpanQuality fetches the Discovery span quality summary from
// the v0.89.86 chunk-2 backend endpoint. Pair with
// useSWR("/discovery/span_quality", fetchSpanQuality) for the
// page-level cache.
export async function fetchSpanQuality(): Promise<SpanQualityResponse> {
  return apiGet<SpanQualityResponse>("/discovery/span_quality");
}

// fetchResourceSpanQuality fetches the per-resource detail. Returns
// null on 404 (resource has no observations yet). simpleRequest
// throws an Error with .status set to the response status; we catch
// and inspect .status rather than parsing the message, mirroring the
// pattern established by rollouts.getPlan in v0.74.
export async function fetchResourceSpanQuality(
  provider: string,
  kind: string,
  id: string,
): Promise<ResourceSpanQuality | null> {
  const path =
    `/discovery/${encodeURIComponent(provider)}` +
    `/inventory/${encodeURIComponent(kind)}` +
    `/${encodeURIComponent(id)}/span_quality`;
  try {
    return await simpleRequest<ResourceSpanQuality>(path, { method: "GET" });
  } catch (err) {
    if (
      err instanceof Error &&
      (err as Error & { status?: number }).status === 404
    ) {
      return null;
    }
    throw err;
  }
}
