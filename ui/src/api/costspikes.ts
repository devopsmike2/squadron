// API client for v0.29 Cost-Spike alerting.
// Mirrors internal/costspikes/detector.go + handlers/costspikes.go.

import { apiGet, apiPost } from "./base";

export type CostSpikeStatus = "open" | "closed" | "all";
export type CostSpikeSeverity = "warn" | "critical";

export interface CostSpikeEvent {
  id: string;
  started_at: string;
  ended_at?: string | null;
  severity: CostSpikeSeverity;
  signal?: string;
  baseline_monthly_usd: number;
  peak_monthly_usd: number;
  peak_pct_above_baseline: number;
  /** JSON string — see CostSpikeAttribution. Empty when attribution
   * collection failed at fire time. UI parses on demand and
   * gracefully degrades when missing. */
  attribution_json?: string;
  acknowledged_at?: string | null;
  acknowledged_by?: string;
}

/** Parsed shape of attribution_json. Kept loose so future detector
 * changes don't break old UIs. */
export interface CostSpikeAttribution {
  signal?: string;
  top_agents?: { agent_id: string; agent_name?: string; bytes_pct?: string }[];
  top_attributes?: { key: string; bytes_pct?: string }[];
}

export interface CostSpikesResponse {
  items: CostSpikeEvent[];
  count: number;
  status: CostSpikeStatus;
  /** Present + false when the backend has no store wired (test
   * harness). UI treats this as "feature off". */
  enabled?: boolean;
}

export function listCostSpikes(
  status: CostSpikeStatus = "open",
  limit = 50,
): Promise<CostSpikesResponse> {
  const q = new URLSearchParams({ status, limit: String(limit) });
  return apiGet<CostSpikesResponse>(`/alerts/cost-spikes?${q}`);
}

export function acknowledgeCostSpike(id: string): Promise<CostSpikeEvent> {
  return apiPost<CostSpikeEvent>(
    `/alerts/cost-spikes/${encodeURIComponent(id)}/acknowledge`,
    {},
  );
}

/** Force-runs one detector tick. For demo + test paths; in
 * production the background loop handles this every minute. */
export function tickCostSpikes(): Promise<{ ok: boolean; reason?: string }> {
  return apiPost<{ ok: boolean; reason?: string }>(
    "/alerts/cost-spikes/tick",
    {},
  );
}

export function parseAttribution(
  raw?: string,
): CostSpikeAttribution | null {
  if (!raw) return null;
  try {
    return JSON.parse(raw) as CostSpikeAttribution;
  } catch {
    return null;
  }
}
