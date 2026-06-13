// API client for v0.31 Pipeline Health.
// Mirrors internal/pipelinehealth + handlers/pipelinehealth.go.

import { apiGet } from "./base";

export type PipelineHealthVerdict =
  | "healthy"
  | "degraded"
  | "broken"
  | "unknown";

export type PipelineHealthSeverity = "warn" | "critical";

/** One finding that contributed to the verdict — surfaced as a
 * bullet under the badge on the agent drawer. */
export interface PipelineHealthSignal {
  kind: string; // e.g. "queue_saturation", "send_failed", "processor_drops"
  severity: PipelineHealthSeverity;
  message: string;
  value: number;
}

/** One (label set, value) row inside Latest. The labels are sorted
 * server-side so the UI doesn't need to stabilize them. */
export interface PipelineHealthMetricRow {
  labels: { key: string; value: string }[];
  value: number;
  unit?: string;
}

/** Per-agent snapshot — the JSON shape behind the agent drawer
 * Pipeline Health panel. */
export interface PipelineHealthAgentSnapshot {
  agent_id: string;
  verdict: PipelineHealthVerdict;
  signals: PipelineHealthSignal[];
  latest: Record<string, PipelineHealthMetricRow[]>;
  last_sample: string;
}

/** Fleet-wide bucketed counts + per-agent verdict map. Dashboard
 * stacked-bar + Fleet Map color coding both read this. */
export interface PipelineHealthFleetSummary {
  total: number;
  healthy: number;
  degraded: number;
  broken: number;
  unknown: number;
  per_agent: Record<string, PipelineHealthVerdict>;
  updated_at: string;
  /** Top-5 worst-offender agent IDs, broken first. */
  concerns?: string[];
}

/** One bucket of a per-metric sparkline. */
export interface PipelineHealthTimePoint {
  t: string;
  v: number;
}

export interface PipelineHealthTimeseriesResponse {
  points: PipelineHealthTimePoint[];
  window: string;
}

export function fetchFleetPipelineHealth(): Promise<PipelineHealthFleetSummary> {
  return apiGet<PipelineHealthFleetSummary>("/pipeline-health/fleet");
}

export function fetchAgentPipelineHealth(
  agentID: string,
): Promise<PipelineHealthAgentSnapshot> {
  return apiGet<PipelineHealthAgentSnapshot>(
    `/pipeline-health/agents/${encodeURIComponent(agentID)}`,
  );
}

/** Per-metric sparkline. `labels` is a "key=value;key=value" filter
 * selecting one (exporter / receiver / processor) time series within
 * the agent. Pass an empty string to aggregate across all label
 * sets. */
export function fetchAgentPipelineHealthTimeseries(
  agentID: string,
  metric: string,
  labels = "",
  window = "1h",
): Promise<PipelineHealthTimeseriesResponse> {
  const params = new URLSearchParams({ metric, window });
  if (labels) params.set("labels", labels);
  return apiGet<PipelineHealthTimeseriesResponse>(
    `/pipeline-health/agents/${encodeURIComponent(agentID)}/timeseries?${params}`,
  );
}

/** Returns the CSS color token for a verdict — UI components use
 * this so the palette stays consistent across the dashboard,
 * agent drawer, and Fleet Map nodes. */
export function verdictColor(v: PipelineHealthVerdict): string {
  switch (v) {
    case "healthy":
      return "var(--status-healthy, #22c55e)";
    case "degraded":
      return "var(--status-warn, #eab308)";
    case "broken":
      return "var(--status-critical, #ef4444)";
    default:
      return "var(--status-unknown, #94a3b8)";
  }
}

/** Human label for the badge text. */
export function verdictLabel(v: PipelineHealthVerdict): string {
  switch (v) {
    case "healthy":
      return "Healthy";
    case "degraded":
      return "Degraded";
    case "broken":
      return "Broken";
    default:
      return "Unknown";
  }
}
