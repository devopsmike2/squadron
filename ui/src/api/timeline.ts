// API client for v0.40 Timeline (postmortem view).
// Mirrors internal/api/handlers/timeline.go. Keep in sync.

import { apiGet } from "./base";

export type TimelineSource = "audit" | "deploy" | "cost_spike";

export type TimelineSeverity = "info" | "ok" | "warn" | "critical";

export interface TimelineEvent {
  id: string;
  source: TimelineSource;
  /** ISO 8601 timestamp. */
  time: string;
  title: string;
  subtitle?: string;
  severity: TimelineSeverity;
  /** Deep link to the source page for full details. */
  href?: string;
}

export interface TimelineResponse {
  items: TimelineEvent[];
  count: number;
  since: string;
  until: string;
}

export interface TimelineQuery {
  /** ISO 8601. Defaults backend-side to now - 24h. */
  since?: string;
  /** ISO 8601. Defaults backend-side to now. */
  until?: string;
  /** Empty = all sources. */
  sources?: TimelineSource[];
  /** Default 500, cap 2000. */
  limit?: number;
}

export function fetchTimeline(q: TimelineQuery = {}): Promise<TimelineResponse> {
  const params = new URLSearchParams();
  if (q.since) params.set("since", q.since);
  if (q.until) params.set("until", q.until);
  if (q.limit) params.set("limit", String(q.limit));
  for (const s of q.sources ?? []) {
    params.append("source", s);
  }
  const qs = params.toString();
  return apiGet<TimelineResponse>(`/timeline${qs ? `?${qs}` : ""}`);
}

/** Color for a severity, expressed as a CSS var fallback. */
export function severityColor(s: TimelineSeverity): string {
  switch (s) {
    case "ok":
      return "var(--success)";
    case "warn":
      return "var(--warning)";
    case "critical":
      return "var(--destructive)";
    default:
      return "var(--primary)";
  }
}

/** Human-friendly source label. Kept here so the UI doesn't sprinkle
 * cap-conversion ternaries throughout. */
export function sourceLabel(s: TimelineSource): string {
  switch (s) {
    case "audit":
      return "Audit";
    case "deploy":
      return "Deploys";
    case "cost_spike":
      return "Cost spikes";
  }
}
