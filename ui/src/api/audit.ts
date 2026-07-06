// Audit log API client.

import { getAuthToken } from "./auth-store";
import { apiConfig, apiGet, simpleRequest } from "./base";

import type {
  AuditEvent,
  AuditEventFilter,
  AuditExplainResponse,
} from "@/types/audit";

interface AuditListResponse {
  events: AuditEvent[];
}

// auditFilterParams renders an AuditEventFilter to query params. Shared by the
// list fetch and the export download so a filtered view and its export always
// match.
const auditFilterParams = (filter: AuditEventFilter): URLSearchParams => {
  const params = new URLSearchParams();
  if (filter.event_type) params.set("event_type", filter.event_type);
  if (filter.target_type) params.set("target_type", filter.target_type);
  if (filter.target_id) params.set("target_id", filter.target_id);
  if (filter.actor) params.set("actor", filter.actor);
  if (filter.since) params.set("since", filter.since);
  if (filter.until) params.set("until", filter.until);
  if (filter.limit) params.set("limit", String(filter.limit));
  return params;
};

// AuditExportFormat is the download format for the audit-log evidence export
// (ADR 0020). CSV is the compliance default; JSON preserves the full payload
// shape; NDJSON is the streamable enterprise form (one event per line). The OSS
// GET /audit/events?format= route serves csv|json for the caller's own tenant;
// NDJSON + cross-tenant are the enterprise wedge served by
// GET /audit-export (6b-ENT).
export type AuditExportFormat = "csv" | "json" | "ndjson";

// OSSAuditExportFormat is the subset the single-tenant OSS route accepts.
export type OSSAuditExportFormat = "csv" | "json";

// downloadAuditExport fetches the OSS single-tenant audit log as an attachment
// and triggers a browser download. It goes through fetch (not a bare <a href>)
// so the Bearer token from auth-store rides along — an anchor navigation would
// drop it. The blob is materialized fully client-side; for the single-tenant
// OSS export this is bounded by the store's page cap, so memory is fine.
export const downloadAuditExport = async (
  filter: AuditEventFilter = {},
  format: OSSAuditExportFormat = "csv",
): Promise<void> => {
  const params = auditFilterParams(filter);
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
  triggerDownload(blob, `audit-export-${downloadStamp()}.${format}`);
};

// AuditExportCapabilities is the enterprise audit-export capabilities probe
// (GET /api/v1/audit-export/capabilities). 404 in OSS via the seam (a 404 tells
// the UI to fall back to the single-tenant export). cross_tenant reports whether
// the operator may target another tenant with ?tenant=.
export interface AuditExportCapabilities {
  formats: string[];
  cross_tenant: boolean;
}

// getAuditExportCapabilities is the non-side-effecting feature-detect the UI
// probes to decide whether to offer the enterprise export controls (NDJSON,
// cross-tenant picker, streaming progress). It must NOT trigger a real export —
// a real export GET self-audits audit.exported. Goes through apiGet, so a 404
// surfaces as an Error with .status === 404 (the SWR feature-detect idiom).
export const getAuditExportCapabilities =
  (): Promise<AuditExportCapabilities> =>
    apiGet<AuditExportCapabilities>("/audit-export/capabilities");

// StreamProgress is reported incrementally while an enterprise export streams.
// rows counts newline-delimited records seen so far (CSV rows incl. header, or
// NDJSON objects); bytes is the raw payload size received.
export interface StreamProgress {
  rows: number;
  bytes: number;
}

export interface StreamExportOptions {
  // tenant, when set, requests a cross-tenant export of that tenant's trail
  // (requires audit:cross_tenant server-side). Omit for the caller's own tenant.
  tenant?: string;
  // onProgress is invoked as chunks arrive so the UI can show live progress.
  onProgress?: (p: StreamProgress) => void;
}

// streamAuditExport downloads the ENTERPRISE audit export (GET /audit-export),
// reading the response body as a stream so a large export reports progress
// instead of blocking opaquely (ADR 0020 6d-part-2 — no streaming-read
// precedent existed in the UI before this). It counts newline-delimited records
// via a TextDecoder as chunks arrive, accumulates the bytes, then materializes a
// Blob and triggers the download. Falls back to a buffered blob() read when the
// response exposes no readable stream (older runtimes / mocked responses).
export const streamAuditExport = async (
  filter: AuditEventFilter = {},
  format: AuditExportFormat = "ndjson",
  opts: StreamExportOptions = {},
): Promise<StreamProgress> => {
  const params = auditFilterParams(filter);
  params.set("format", format);
  if (opts.tenant) params.set("tenant", opts.tenant);

  const url = `${apiConfig.baseUrl}/audit-export?${params.toString()}`;
  const headers: Record<string, string> = {};
  const token = getAuthToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;

  const resp = await fetch(url, { headers });
  if (!resp.ok) {
    throw new Error(`export failed (${resp.status})`);
  }

  const ext = format === "json" ? "json" : format === "csv" ? "csv" : "ndjson";
  const filename = `audit-export-${downloadStamp()}.${ext}`;

  // Fallback: no streamable body (e.g. a mocked/blob-only response).
  const reader = resp.body?.getReader?.();
  if (!reader) {
    const blob = await resp.blob();
    const rows = countNewlines(await blob.text());
    opts.onProgress?.({ rows, bytes: blob.size });
    triggerDownload(blob, filename);
    return { rows, bytes: blob.size };
  }

  const decoder = new TextDecoder();
  const chunks: Uint8Array[] = [];
  let rows = 0;
  let bytes = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (value) {
      chunks.push(value);
      bytes += value.byteLength;
      rows += countNewlines(decoder.decode(value, { stream: true }));
      opts.onProgress?.({ rows, bytes });
    }
  }
  // Flush any trailing multibyte remainder (no bytes, but completes decode).
  decoder.decode();

  const blob = new Blob(chunks as BlobPart[], {
    type: contentTypeFor(format),
  });
  triggerDownload(blob, filename);
  return { rows, bytes };
};

const countNewlines = (s: string): number => {
  let n = 0;
  for (let i = 0; i < s.length; i++) if (s.charCodeAt(i) === 10) n++;
  return n;
};

const contentTypeFor = (format: AuditExportFormat): string => {
  if (format === "csv") return "text/csv;charset=utf-8";
  if (format === "json") return "application/json";
  return "application/x-ndjson";
};

const downloadStamp = (): string =>
  new Date().toISOString().replace(/[:.]/g, "-");

// triggerDownload materializes a Blob as a browser download via an object-URL
// anchor click (revoked in finally). Shared by the OSS + enterprise export paths.
const triggerDownload = (blob: Blob, filename: string): void => {
  const objectUrl = URL.createObjectURL(blob);
  try {
    const a = document.createElement("a");
    a.href = objectUrl;
    a.download = filename;
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
  const params = auditFilterParams(filter);
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
