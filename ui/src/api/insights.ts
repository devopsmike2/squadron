// API client for v0.24 Telemetry Volume Insights.
// Mirrors the Go service's response shapes in
// internal/insights/insights.go — keep these in sync.

import { apiGet } from "./base";

export type InsightsWindow = "5m" | "1h" | "24h";
export type InsightsSignal = "traces" | "metrics" | "logs";

export interface SignalVolume {
  signal: InsightsSignal | "";
  bytes: number;
  item_count: number;
  dropped_count: number;
}

export interface FleetSummary {
  window: InsightsWindow;
  start_time: string;
  end_time: string;
  totals: SignalVolume;
  by_signal: SignalVolume[];
  agent_count: number;
}

export interface AgentVolume {
  agent_id: string;
  agent_name?: string;
  total_bytes: number;
  by_signal: SignalVolume[];
}

export interface AttributeVolume {
  key: string;
  bytes: number;
  estimated: boolean;
  pct_of_signal: number;
}

export interface TopAgentsResponse {
  items: AgentVolume[];
  limit: number;
}

export interface TopAttributesResponse {
  items: AttributeVolume[];
  signal: InsightsSignal;
  limit: number;
  // True on the envelope so callers can show the "estimated" caveat
  // even before drilling into the rows.
  estimated: boolean;
}

export interface DropsResponse {
  items: SignalVolume[];
}

// Build a query string from the params. Empty / undefined values
// are skipped so the URL stays clean.
function qs(params: Record<string, unknown>): string {
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === "" || v === null) continue;
    if (Array.isArray(v)) {
      for (const x of v) usp.append(k, String(x));
    } else {
      usp.set(k, String(v));
    }
  }
  const s = usp.toString();
  return s ? `?${s}` : "";
}

export const getFleetVolume = (params: {
  window: InsightsWindow;
  signal?: InsightsSignal | InsightsSignal[];
}): Promise<FleetSummary> => apiGet<FleetSummary>(`/insights/volume${qs(params)}`);

export const getTopAgents = (params: {
  window: InsightsWindow;
  limit?: number;
}): Promise<TopAgentsResponse> =>
  apiGet<TopAgentsResponse>(`/insights/volume/agents${qs(params)}`);

export const getAgentVolume = (
  agentID: string,
  params: { window: InsightsWindow },
): Promise<AgentVolume> =>
  apiGet<AgentVolume>(`/insights/volume/agents/${agentID}${qs(params)}`);

export const getTopAttributes = (params: {
  window: InsightsWindow;
  signal: InsightsSignal;
  limit?: number;
}): Promise<TopAttributesResponse> =>
  apiGet<TopAttributesResponse>(`/insights/volume/attributes${qs(params)}`);

export const getDrops = (params: {
  window: InsightsWindow;
}): Promise<DropsResponse> => apiGet<DropsResponse>(`/insights/volume/drops${qs(params)}`);
