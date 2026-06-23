// API client for the v0.89.48 #670 Stream 68 (slice-1 chunk 4) GCP
// discovery surface. Mirrors the Go handlers in
// internal/api/handlers/discovery_gcp.go and the endpoint surface
// documented in docs/proposals/gcp-discovery-slice1.md §6 — keep
// these in sync.
//
// Slice-1 honesty:
//   - Service-account credentials are pasted client-side, base64-
//     encoded over the wire, and credstore-sealed server-side. The
//     plaintext JSON NEVER lands in localStorage / sessionStorage /
//     any persisted client surface.
//   - The page does NOT carry a recommendations helper yet — the
//     proposer extension ships in chunk 5 of this arc (Stream 68).
//     Recommendations tab renders a "ships in chunk 5" stub until
//     that lands.
//   - Same fetch-wrapper + Bearer-token discipline as
//     ui/src/api/discovery.ts (the AWS counterpart) via the shared
//     ./base helpers.

import { apiDelete, apiGet, apiPost } from "./base";

// --- Storage type --------------------------------------------------

// GCPConnection mirrors gcpconnstore.GCPConnection. The SealedSA
// bytes carry json:"-" on the Go struct so this wire shape NEVER
// includes the sealed credential blob — operators see "this project
// is connected"; they cannot read back the SA key material from the
// UI. Mirrors the AWS CloudConnection posture.
export interface GCPConnection {
  id: string;
  display_name: string;
  project_id: string;
  region: string;
  learn_from_accepted_recommendations: boolean;
  // ISO-8601 timestamp strings. The page formats relative time via
  // an inline helper rather than pulling in date-fns.
  created_at: string;
  updated_at: string;
}

// --- Create / list / get / delete -----------------------------------

// CreateGCPConnectionRequest is the POST /discovery/gcp/connections
// body. sealed_sa is the base64 encoding of the raw SA JSON bytes —
// the wire wrapping avoids JSON-in-JSON escape pain. The server
// base64-decodes then credstore-seals before storage; the plaintext
// JSON does not enter SQLite at any point.
export interface CreateGCPConnectionRequest {
  display_name: string;
  project_id: string;
  // base64(SA JSON bytes). Encoded by the wizard's
  // sealServiceAccountForWire helper below.
  sealed_sa: string;
  region: string;
}

export function createGCPConnection(
  req: CreateGCPConnectionRequest,
): Promise<GCPConnection> {
  return apiPost<GCPConnection>("/discovery/gcp/connections", req);
}

// listGCPConnectionsResponse mirrors the gcpListConnectionsResponse
// wire shape — Connections is always a non-nil array even when the
// store is empty, so the UI's empty-state branch is a single
// .length === 0 check.
interface listGCPConnectionsResponse {
  connections: GCPConnection[];
}

export async function listGCPConnections(): Promise<GCPConnection[]> {
  const resp = await apiGet<listGCPConnectionsResponse>(
    "/discovery/gcp/connections",
  );
  return resp.connections ?? [];
}

export function getGCPConnection(id: string): Promise<GCPConnection> {
  return apiGet<GCPConnection>(
    `/discovery/gcp/connections/${encodeURIComponent(id)}`,
  );
}

export function deleteGCPConnection(id: string): Promise<void> {
  return apiDelete<void>(
    `/discovery/gcp/connections/${encodeURIComponent(id)}`,
  );
}

// --- Validate -------------------------------------------------------

// GCPValidateErrorKind enumerates the discriminated failure modes the
// server's classifier returns. The wizard branches on the kind to
// render a humanized remediation hint specific to the failure:
//
//   - permission_denied : SA lacks roles/compute.viewer.
//   - project_not_found : The configured project_id doesn't exist or
//                         the SA can't see it.
//   - credentials_invalid : SA JSON is malformed or rejected.
//   - network           : Squadron's outbound path to
//                         compute.googleapis.com is blocked.
//   - project_mismatch  : SA JSON's project_id differs from the
//                         connection's configured project_id (the
//                         "wrong SA in the wrong project" footgun
//                         §11.3 of the design doc calls out).
//   - unknown           : Catch-all — surface the raw message and
//                         ask the operator to file an issue.
export type GCPValidateErrorKind =
  | "permission_denied"
  | "project_not_found"
  | "credentials_invalid"
  | "network"
  | "project_mismatch"
  | "unknown";

export interface ValidateGCPResponse {
  ok: boolean;
  instance_count?: number;
  error_kind?: GCPValidateErrorKind;
  message?: string;
}

export function validateGCPConnection(
  id: string,
): Promise<ValidateGCPResponse> {
  return apiPost<ValidateGCPResponse>(
    `/discovery/gcp/connections/${encodeURIComponent(id)}/validate`,
  );
}

// --- Scan ----------------------------------------------------------

// ComputeInstanceSnapshot mirrors scanner.ComputeInstanceSnapshot
// (the same shared shape AWS uses — chunk 2 of this arc settled on
// the shared struct). The Inventory tab renders one row per entry.
//
// Field naming follows the Go json tags. `labels` is the GCP analog
// of the AWS `tags` field — both are the single-axis observability
// signal carrier (presence of an `otel*` key marks the row covered).
export interface ComputeInstanceSnapshot {
  resource_id: string;
  instance_type: string;
  tags: Record<string, string>;
  has_otel: boolean;
  os_family: string;
  region: string;
}

// DatabaseInstanceSnapshot mirrors scanner.DatabaseInstanceSnapshot —
// the shared cross-cloud database row shape. The four per-cloud
// observability axis flags are optional because each cloud only
// populates the one that matches its scanner: AWS RDS sets
// performance_insights_enabled + enhanced_monitoring_enabled; GCP
// Cloud SQL sets query_insights_enabled; Azure SQL sets
// sql_insights_diag_enabled; OCI DB sets database_management_enabled.
// The `provider` discriminator routes the inventory table's
// instrumentation-column rendering to the right axis.
//
// Database tier slice 2 (v0.89.66, #695 Stream 93).
export interface DatabaseInstanceSnapshot {
  resource_id: string;
  engine: string;
  engine_version: string;
  instance_class: string;
  region: string;
  tags?: Record<string, string>;
  provider?: string;

  performance_insights_enabled?: boolean; // AWS RDS (slice 1)
  enhanced_monitoring_enabled?: boolean;  // AWS RDS (slice 1)

  query_insights_enabled?: boolean;       // GCP Cloud SQL (slice 2)
  sql_insights_diag_enabled?: boolean;    // Azure SQL (slice 2)
  database_management_enabled?: boolean;  // OCI DB (slice 2)
}

// ScanGCPResponse mirrors gcpScanResponse on the wire. Note that
// unlike the AWS ScanResult there is no top-level instance_count —
// the page computes it client-side as compute.length. Same posture
// as the AWS handler which omitted a redundant tally.
//
// Database tier slice 2 (v0.89.66, #695 Stream 93) — `databases`
// carries the Cloud SQL instance inventory. Optional on the wire
// (omitempty Go JSON tag) so older scan rows from before the
// scanner extension don't render an undefined array; the Inventory
// tab's Databases sub-tab treats undefined as empty.
export interface ScanGCPResponse {
  connection_id: string;
  project_id: string;
  region: string;
  compute: ComputeInstanceSnapshot[];
  databases?: DatabaseInstanceSnapshot[];
  instrumented_count: number;
  uninstrumented_count: number;
  partial: boolean;
  partial_reason?: string;
  failed_services?: string[];
  scan_id: string;
}

export function scanGCPConnection(id: string): Promise<ScanGCPResponse> {
  return apiPost<ScanGCPResponse>(
    `/discovery/gcp/connections/${encodeURIComponent(id)}/scan`,
  );
}

// --- Wire-encoding helper ------------------------------------------

// encodeServiceAccountForWire base64-encodes the raw SA JSON the
// operator pasted into the wizard textarea. The server expects the
// payload to be a base64 string (gcpCreateConnectionRequest.SealedSA
// in the Go handler) rather than a JSON-in-JSON nested object — see
// design doc §6 for the rationale.
//
// btoa() only accepts Latin-1; SA JSON is ASCII so the encoding is
// safe, but we use the TextEncoder + base64 path for forward-compat
// with any future non-ASCII content (e.g. unicode-named projects).
export function encodeServiceAccountForWire(saJSON: string): string {
  const bytes = new TextEncoder().encode(saJSON);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}
