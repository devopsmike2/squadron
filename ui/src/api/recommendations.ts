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
  | "empty_signal";

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
