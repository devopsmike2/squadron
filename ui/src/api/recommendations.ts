// API client for v0.25 Cost Recommendations.
// Mirrors the Go shapes in internal/recommendations/recommendations.go.
// Keep these in sync — the engine's JSON shape is wire-stable.

import { apiGet, apiPost } from "./base";
import type { InsightsWindow, InsightsSignal } from "./insights";

/**
 * Recipe buckets. Maps 1:1 with the Go Category constants. The UI
 * uses these for icon/color routing; unknown values fall through to
 * the default treatment so future recipes don't require a UI deploy.
 */
export type RecommendationCategory =
  | "noisy_attribute"
  | "outlier_agent"
  | "drop_hotspot"
  | "empty_signal"
  | "high_cardinality";

export type RecommendationSeverity = "critical" | "warn" | "info";

export interface Recommendation {
  id: string;
  category: RecommendationCategory;
  severity: RecommendationSeverity;
  title: string;
  detail: string;
  agent_id?: string;
  agent_name?: string;
  signal?: InsightsSignal | "";
  /** Bytes-per-window the fix would plausibly avoid. -1 when the
   * recommendation isn't a byte-savings (e.g. drop hotspots). */
  est_savings_bytes: number;
  /** Projected $/month the fix would save under current pricing
   * assumptions. 0 when pricing is disabled or the recipe doesn't
   * map to a byte-savings (high-cardinality is per-series cost,
   * not per-byte). */
  est_savings_per_month_usd?: number;
  /** Share of the signal's byte budget the recommendation targets.
   * Useful for the per-card progress bar. */
  pct_of_signal?: number;
  /** Small YAML fragment the operator can copy. Empty when no
   * single snippet applies (the outlier-agent category, for
   * example, needs a human to investigate). */
  snippet?: string;
  generated_at: string;
}

export interface RecommendationsResponse {
  items: Recommendation[];
  window: InsightsWindow;
  count: number;
  /** Always true on the envelope — every recommendation includes
   * sampled / approximated inputs. UI surfaces this once. */
  estimated: boolean;
}

export interface AgentRecommendationsResponse extends RecommendationsResponse {
  agent_id: string;
}

export interface RecommendationDismissal {
  recommendation_id: string;
  dismissed_at: string;
  dismissed_by: string;
  reason?: string;
}

export interface DismissalsResponse {
  items: RecommendationDismissal[];
}

// ----------------------------------------------------------------
// Public client functions
// ----------------------------------------------------------------

export function getRecommendations(
  window: InsightsWindow = "1h",
  limit = 50,
): Promise<RecommendationsResponse> {
  const q = new URLSearchParams({ window, limit: String(limit) });
  return apiGet<RecommendationsResponse>(`/recommendations?${q}`);
}

export function getRecommendationsForAgent(
  agentId: string,
  window: InsightsWindow = "24h",
): Promise<AgentRecommendationsResponse> {
  const q = new URLSearchParams({ window });
  return apiGet<AgentRecommendationsResponse>(
    `/recommendations/agents/${encodeURIComponent(agentId)}?${q}`,
  );
}

/**
 * Record that the operator clicked Apply on a recommendation.
 * Snapshots the recommendation + baseline byte rate server-side so
 * Squadron can later compute realized savings against the live
 * post-apply byte rate. Optional body lets the UI freeze a view in
 * case the engine has stopped producing this recommendation by the
 * time the click lands.
 */
export interface ApplyRecommendationBody {
  title?: string;
  category?: RecommendationCategory;
  signal?: InsightsSignal | "";
  est_savings_per_month_usd?: number;
  est_savings_bytes?: number;
  attribute_key?: string;
}

export interface RecommendationOutcome {
  id: string;
  recommendation_id: string;
  applied_at: string;
  applied_by: string;
  title: string;
  category: string;
  signal?: string;
  attribute_key?: string;
  baseline_bytes_per_hour: number;
  est_savings_per_month_usd_at_apply: number;
  last_observed_bytes_per_hour: number;
  last_observed_at?: string;
  realized_savings_per_month_usd: number;
  /** pending | realized | not_observed | reverted */
  status: "pending" | "realized" | "not_observed" | "reverted";
}

export function applyRecommendation(
  id: string,
  body?: ApplyRecommendationBody,
): Promise<RecommendationOutcome> {
  return apiPost<RecommendationOutcome>(
    `/recommendations/${encodeURIComponent(id)}/applied`,
    body ?? {},
  );
}

export function dismissRecommendation(
  id: string,
  reason?: string,
): Promise<RecommendationDismissal> {
  return apiPost<RecommendationDismissal>(
    `/recommendations/${encodeURIComponent(id)}/dismiss`,
    reason ? { reason } : {},
  );
}

export function restoreRecommendation(
  id: string,
): Promise<{ ok: boolean; recommendation_id: string }> {
  return apiPost<{ ok: boolean; recommendation_id: string }>(
    `/recommendations/${encodeURIComponent(id)}/restore`,
    {},
  );
}

export function listDismissals(): Promise<DismissalsResponse> {
  return apiGet<DismissalsResponse>("/recommendations/dismissals");
}
