// Enterprise access-review API client (ADR 0020 6c + 6c query patterns / ADR
// 0022). All endpoints 404 in OSS via the seam — callers feature-detect through
// getAuditReviewCapabilities (a 404 surfaces as Error.status===404 via
// simpleRequest, the SettingsIdentity idiom). Everything returns JSON, so these
// go through apiGet (auto-attaches the Bearer token + stamps error.status).

import { apiGet } from "./base";

// AuditReviewCapabilities is the non-side-effecting probe payload.
export interface AuditReviewCapabilities {
  cross_tenant: boolean;
  rollups: string[];
  patterns: string[];
}

// ReviewBucket is one rollup entry (an actor or event_type + its count).
export interface ReviewBucket {
  key: string;
  count: number;
}

// ReviewEvent mirrors the enterprise auditreview.Event (a subset of AuditEvent).
export interface ReviewEvent {
  id: string;
  timestamp: string; // RFC3339
  actor: string;
  event_type: string;
  target_type: string;
  target_id?: string;
  action: string;
  payload?: Record<string, unknown>;
}

// ReviewResult backs the filtered query / actor-timeline / resource-access views.
export interface ReviewResult {
  events: ReviewEvent[];
  count: number;
  truncated: boolean;
  by_actor: ReviewBucket[];
  by_event_type: ReviewBucket[];
  tenant?: string;
  cross_tenant: boolean;
}

// AdminActionsResult backs the per-tenant admin-action rollup.
export interface AdminActionsResult {
  tenant: string;
  total: number;
  truncated: boolean;
  by_event_type: ReviewBucket[];
  by_actor: ReviewBucket[];
  sample: ReviewEvent[];
  cross_tenant: boolean;
}

// ReviewWindow are the shared query params for the timeline/resource views. tenant
// requests a cross-tenant review of that tenant's trail (requires
// audit:cross_tenant server-side); omit for the caller's own tenant.
export interface ReviewWindow {
  since?: string; // RFC3339
  until?: string; // RFC3339
  limit?: number;
  tenant?: string;
}

const windowParams = (w: ReviewWindow): string => {
  const p = new URLSearchParams();
  if (w.since) p.set("since", w.since);
  if (w.until) p.set("until", w.until);
  if (w.limit) p.set("limit", String(w.limit));
  if (w.tenant) p.set("tenant", w.tenant);
  const qs = p.toString();
  return qs ? `?${qs}` : "";
};

export const getAuditReviewCapabilities =
  (): Promise<AuditReviewCapabilities> =>
    apiGet<AuditReviewCapabilities>("/audit-review/capabilities");

// getActorTimeline — GET /audit-review/actors/{actor}/timeline.
export const getActorTimeline = (
  actor: string,
  w: ReviewWindow = {},
): Promise<ReviewResult> =>
  apiGet<ReviewResult>(
    `/audit-review/actors/${encodeURIComponent(actor)}/timeline${windowParams(w)}`,
  );

// getResourceAccess — GET /audit-review/resources/{type}/{id}/access.
export const getResourceAccess = (
  resourceType: string,
  resourceId: string,
  w: ReviewWindow = {},
): Promise<ReviewResult> =>
  apiGet<ReviewResult>(
    `/audit-review/resources/${encodeURIComponent(resourceType)}/${encodeURIComponent(
      resourceId,
    )}/access${windowParams(w)}`,
  );

// getAdminActions — GET /audit-review/tenants/{tenant}/admin-actions.
export const getAdminActions = (
  tenant: string,
  opts: { since?: string; until?: string; sample?: number } = {},
): Promise<AdminActionsResult> => {
  const p = new URLSearchParams();
  if (opts.since) p.set("since", opts.since);
  if (opts.until) p.set("until", opts.until);
  if (opts.sample) p.set("sample", String(opts.sample));
  const qs = p.toString();
  return apiGet<AdminActionsResult>(
    `/audit-review/tenants/${encodeURIComponent(tenant)}/admin-actions${qs ? `?${qs}` : ""}`,
  );
};
