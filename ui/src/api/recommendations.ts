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

/**
 * v0.85 — typed source for the recommendation. Maps 1:1 with the
 * Go SourceKind constants. Distinguishes recommendations
 * produced by the cost-spike pipeline (JARVIS arc) from
 * discovery scans (universal observation arc) and from manual
 * operator creation. Unknown values fall through to gray styling
 * so future producers don't require a UI deploy.
 */
export type RecommendationSourceKind =
  | "cost_spike"
  | "discovery_scan"
  | "manual";

/**
 * v0.85 — typed action kind. Maps 1:1 with the Go ActionKind
 * constants. The UI matches on this to render the right button
 * label + confirmation flow.
 */
export type RecommendationActionKind =
  | "rollout"
  | "plan"
  | "discovery_action";

/**
 * v0.85 — Infrastructure-as-Code format. Slice 1 emits Terraform
 * only; CDK and Pulumi land via later slices with the same wire
 * shape.
 */
export type IaCFormat = "terraform" | "cdk" | "pulumi";

export interface RecommendationSource {
  kind: RecommendationSourceKind;
  /** Backing reference id (cost_spike_id / discovery_scan_id /
   * actor user id). Descriptive only — the UI may deeplink on it. */
  ref_id?: string;
}

export interface RecommendationAction {
  kind: RecommendationActionKind;
  /** Action-specific JSON. The shape is determined by kind; the
   * UI's panel renders a kind-aware button and defers the
   * unmarshal to whichever flow handles the action. */
  payload: unknown;
}

export interface IaCSnippet {
  format: IaCFormat;
  /** Actual Terraform/CDK/Pulumi code. Squadron does NOT execute
   * this — the operator runs it through their IaC pipeline. */
  source: string;
}

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
  /** v0.85 — typed source. Absent on pre-v0.85 wire shapes. */
  source?: RecommendationSource;
  /** v0.85 — typed action payload. Absent for advisory-only
   * recommendations. */
  action?: RecommendationAction;
  /** v0.85 — Infrastructure-as-Code snippet. Present for
   * cloud-side discovery recommendations; absent for
   * collector-side advice. */
  iac?: IaCSnippet;
  /** v0.89.3 #603 Stream 19 Phase 4 — placement-map key for the
   * Recommendations tab's Open-PR button. Set ONLY on discovery-
   * source recommendations whose snippet classified into one of
   * the slice-1 placement-map rows (ec2-otel-layer,
   * lambda-otel-layer, rds-pi-em, s3-access-logging,
   * alb-access-logs, eks-cluster-logging,
   * eks-observability-addon). Empty when the recommendation is
   * not Open-PR-eligible — the card falls back to Copy-only. */
  resource_kind?: string;
  /** v0.89.4 #611 — list of resource identifiers (ARNs / ids) the
   * discovery proposer named as the step's targets. The Open-PR
   * call forwards this verbatim; the backend uses len() in the PR
   * title's "for <N> resources" count and renders the list in the
   * PR body's "Affected resources" section. Absent on pre-v0.89.4
   * outputs and on cost-spike / collector-side recommendations.
   * NOT rendered to the operator in the recommendation card —
   * metadata for PR-text construction only. */
  affected_resources?: string[];
  /** v0.89.11 #626 Stream 27 — slice 1.5 hybrid PR disposition.
   * Two values:
   *  - "new_file": the snippet defines a NET-NEW top-level
   *    Terraform resource; the Open-PR handler writes a sibling
   *    file (merge-clean).
   *  - "patch_existing": the snippet modifies an EXISTING
   *    top-level resource block; the handler appends to the
   *    placement file and labels the PR "[needs manual merge]".
   * The UI reads this field to render a "Needs manual merge"
   * badge next to the Open PR button on patch_existing kinds.
   * Empty for non-IaC recommendations. */
  disposition?: "new_file" | "patch_existing" | string;
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
