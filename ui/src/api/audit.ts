// Audit log API client.

import { simpleRequest } from "./base";

import type {
  AuditEvent,
  AuditEventFilter,
  AuditExplainResponse,
} from "@/types/audit";

interface AuditListResponse {
  events: AuditEvent[];
}

export const listAuditEvents = async (
  filter: AuditEventFilter = {},
): Promise<AuditEvent[]> => {
  const params = new URLSearchParams();
  if (filter.target_type) params.set("target_type", filter.target_type);
  if (filter.target_id) params.set("target_id", filter.target_id);
  if (filter.since) params.set("since", filter.since);
  if (filter.limit) params.set("limit", String(filter.limit));

  const query = params.toString();
  const path = query ? `/audit/events?${query}` : "/audit/events";

  const resp = await simpleRequest<AuditListResponse>(path);
  return resp.events ?? [];
};

// explainAuditEvent calls POST /audit/:id/explain. Pass regenerate=true
// to bypass the cache and force a fresh LLM call (the response cached
// on the row is replaced server side).
export const explainAuditEvent = async (
  id: string,
  regenerate = false,
): Promise<AuditExplainResponse> => {
  const path = regenerate
    ? `/audit/${encodeURIComponent(id)}/explain?regenerate=1`
    : `/audit/${encodeURIComponent(id)}/explain`;
  return simpleRequest<AuditExplainResponse>(path, {
    method: "POST",
    body: JSON.stringify({}),
  });
};
