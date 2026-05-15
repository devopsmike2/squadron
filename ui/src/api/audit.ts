// Audit log API client.

import { simpleRequest } from "./base";

import type { AuditEvent, AuditEventFilter } from "@/types/audit";

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
