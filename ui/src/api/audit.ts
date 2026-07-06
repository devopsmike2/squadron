// Audit log API client.

import { getAuthToken } from "./auth-store";
import { apiConfig, simpleRequest } from "./base";

import type {
  AuditEvent,
  AuditEventFilter,
  AuditExplainResponse,
} from "@/types/audit";

interface AuditListResponse {
  events: AuditEvent[];
}

// AuditExportFormat is the download format for the audit-log evidence export
// (ADR 0020). CSV is the compliance default; JSON preserves the full payload
// shape. The GET /audit/events route serves these as an attachment when
// ?format= is set; it is tenant-scoped to the caller's tenant and self-audits
// the export (audit.exported). NDJSON + cross-tenant are the enterprise wedge
// (6b-ENT) and are not offered here.
export type AuditExportFormat = "csv" | "json";

// downloadAuditExport fetches the audit log as an attachment and triggers a
// browser download. It goes through fetch (not a bare <a href>) so the Bearer
// token from auth-store rides along — an anchor navigation would drop it. The
// blob is materialized fully client-side; for the single-tenant OSS export this
// is bounded by the store's page cap, so memory is fine.
export const downloadAuditExport = async (
  filter: AuditEventFilter = {},
  format: AuditExportFormat = "csv",
): Promise<void> => {
  const params = new URLSearchParams();
  if (filter.target_type) params.set("target_type", filter.target_type);
  if (filter.target_id) params.set("target_id", filter.target_id);
  if (filter.since) params.set("since", filter.since);
  if (filter.limit) params.set("limit", String(filter.limit));
  params.set("format", format);

  const url = `${apiConfig.baseUrl}/audit/events?${params.toString()}`;
  const headers: Record<string, string> = {};
  const token = getAuthToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;

  const resp = await fetch(url, { headers });
  if (!resp.ok) {
    throw new Error(`export failed (${resp.status})`);
  }
  const blob = await resp.blob();
  const objectUrl = URL.createObjectURL(blob);
  try {
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");
    const a = document.createElement("a");
    a.href = objectUrl;
    a.download = `audit-export-${stamp}.${format}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    URL.revokeObjectURL(objectUrl);
  }
};

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
