// API client for the v0.89.53 #677 Stream 75 (slice-1 chunk 4) Azure
// discovery surface. Mirrors the Go handlers in
// internal/api/handlers/discovery_azure.go and the endpoint surface
// documented in docs/proposals/azure-discovery-slice1.md §7 — keep
// these in sync. The shapes here intentionally parallel
// ui/src/api/discoveryGCP.ts so a future arc-spanning refactor that
// factors out a shared "cloud discovery" client has minimal diff
// surface.
//
// Slice-1 honesty:
//   - The Service Principal client_secret is pasted client-side,
//     base64-encoded over the wire, and credstore-sealed server-side.
//     The plaintext secret NEVER lands in localStorage / sessionStorage
//     / any persisted client surface. Same posture as the GCP SA JSON
//     in discoveryGCP.ts.
//   - The page does NOT carry a recommendations helper yet — the
//     proposer extension ships in chunk 5 of this arc. The
//     Recommendations tab renders a "ships in chunk 5" stub until
//     that lands.
//   - Same fetch-wrapper + Bearer-token discipline as the AWS / GCP
//     counterparts via the shared ./base helpers.

import { apiDelete, apiGet, apiPost } from "./base";

// --- Storage type --------------------------------------------------

// AzureConnection mirrors azureconnstore.AzureConnection. The
// SealedSecret bytes carry json:"-" on the Go struct so this wire
// shape NEVER includes the sealed credential blob — operators see
// "this subscription is connected"; they cannot read back the SP
// client_secret material from the UI. Mirrors the AWS / GCP
// CloudConnection posture.
export interface AzureConnection {
  id: string;
  display_name: string;
  tenant_id: string;
  subscription_id: string;
  client_id: string;
  location: string;
  learn_from_accepted_recommendations: boolean;
  // ISO-8601 timestamp strings. The page formats relative time via
  // an inline helper rather than pulling in date-fns — same posture
  // as the GCP page.
  created_at: string;
  updated_at: string;
}

// --- Create / list / get / delete -----------------------------------

// CreateAzureConnectionRequest is the POST /discovery/azure/connections
// body. sealed_secret is the base64 encoding of the raw Service
// Principal client_secret bytes — base64 over the wire keeps the
// payload uniform across providers (GCP uses the same base64 wrap on
// its SA JSON) and avoids any JSON-escape edge case if the operator
// pasted a secret containing quotes / backslashes. The server
// base64-decodes then credstore-seals before storage; the plaintext
// secret does not enter SQLite at any point.
export interface CreateAzureConnectionRequest {
  display_name: string;
  tenant_id: string;
  subscription_id: string;
  client_id: string;
  // base64(SP client_secret bytes). Encoded by the wizard's
  // encodeClientSecretForWire helper below.
  sealed_secret: string;
  location: string;
}

export function createAzureConnection(
  req: CreateAzureConnectionRequest,
): Promise<AzureConnection> {
  return apiPost<AzureConnection>("/discovery/azure/connections", req);
}

// listAzureConnectionsResponse mirrors the azureListConnectionsResponse
// wire shape — Connections is always a non-nil array even when the
// store is empty, so the UI's empty-state branch is a single
// .length === 0 check. Matches the GCP precedent.
interface listAzureConnectionsResponse {
  connections: AzureConnection[];
}

export async function listAzureConnections(): Promise<AzureConnection[]> {
  const resp = await apiGet<listAzureConnectionsResponse>(
    "/discovery/azure/connections",
  );
  return resp.connections ?? [];
}

export function getAzureConnection(id: string): Promise<AzureConnection> {
  return apiGet<AzureConnection>(
    `/discovery/azure/connections/${encodeURIComponent(id)}`,
  );
}

export function deleteAzureConnection(id: string): Promise<void> {
  return apiDelete<void>(
    `/discovery/azure/connections/${encodeURIComponent(id)}`,
  );
}

// --- Validate -------------------------------------------------------

// AzureValidateErrorKind enumerates the discriminated failure modes
// the server's classifier returns (see classifyAzureScanError in
// internal/api/handlers/discovery_azure.go). The wizard branches on
// the kind to render a humanized remediation hint specific to the
// failure:
//
//   - permission_denied      : SP lacks Reader at the subscription scope.
//   - subscription_not_found : The configured subscription_id doesn't
//                              exist or the SP can't see it.
//   - tenant_invalid         : The tenant_id doesn't match the Azure AD
//                              tenant where the SP was created.
//   - credentials_invalid    : Client ID / Client Secret mismatch or
//                              the SP secret expired (Azure SP
//                              secrets default to 1 year).
//   - network                : Squadron's outbound path to
//                              management.azure.com is blocked.
//   - subscription_mismatch  : The SP's subscription scope doesn't
//                              match the configured subscription_id.
//   - unknown                : Catch-all — surface the raw message and
//                              ask the operator to file an issue.
export type AzureValidateErrorKind =
  | "permission_denied"
  | "subscription_not_found"
  | "tenant_invalid"
  | "credentials_invalid"
  | "network"
  | "subscription_mismatch"
  | "unknown";

export interface ValidateAzureResponse {
  ok: boolean;
  instance_count?: number;
  error_kind?: AzureValidateErrorKind;
  message?: string;
}

export function validateAzureConnection(
  id: string,
): Promise<ValidateAzureResponse> {
  return apiPost<ValidateAzureResponse>(
    `/discovery/azure/connections/${encodeURIComponent(id)}/validate`,
  );
}

// --- Scan ----------------------------------------------------------

// ComputeInstanceSnapshot mirrors scanner.ComputeInstanceSnapshot.
// The same shared shape AWS and GCP use — chunk 2 of the GCP arc
// settled on the shared struct. The Inventory tab renders one row
// per entry.
//
// Field naming follows the Go json tags. `tags` is the single-axis
// observability signal carrier (presence of an `otel*` key marks the
// row covered). For Azure the OS family is reliably populated from
// vm.Properties.StorageProfile.OsDisk.OSType — unlike AWS and GCP
// where it remains "unknown" in slice 1.
export interface ComputeInstanceSnapshot {
  resource_id: string;
  instance_type: string;
  tags: Record<string, string>;
  has_otel: boolean;
  os_family: string;
  region: string;
}

// DatabaseInstanceSnapshot mirrors scanner.DatabaseInstanceSnapshot —
// the shared cross-cloud database row shape. Each cloud only
// populates the axis flag that matches its scanner; the optional
// fields keep the type stable across providers. See discoveryGCP.ts
// for the full per-axis documentation.
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

// ClusterSnapshot mirrors scanner.ClusterSnapshot — the shared
// cross-cloud Kubernetes cluster row shape. See discoveryGCP.ts for
// the full per-axis documentation.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100).
export interface ClusterSnapshot {
  resource_id: string;
  name: string;
  kubernetes_version: string;
  status: string;
  region: string;
  tags?: Record<string, string>;
  provider?: string;

  // AWS EKS slice 1.
  control_plane_logging?: string[];
  addons?: Array<{ name: string; status: string; version?: string }>;
  nodegroup_count?: number;
  fargate_profile_count?: number;

  // Slice 2 per-cloud managed-observability axes.
  managed_prometheus_enabled?: boolean;     // GCP GKE
  azure_monitor_enabled?: boolean;          // Azure AKS
  operations_insights_enabled?: boolean;    // OCI OKE
}

// ScanAzureResponse mirrors azureScanResponse on the wire. The
// handler emits instance_count, instrumented_count, uninstrumented_count
// directly so the UI doesn't have to derive them — symmetric with the
// GCP handler's tally pass.
//
// Database tier slice 2 (v0.89.66, #695 Stream 93) — `databases`
// carries the Azure SQL Database inventory. Optional on the wire
// (omitempty Go JSON tag) so older scan rows from before the chunk
// 3 scanner extension don't render an undefined array; the
// Inventory tab's Databases sub-tab treats undefined as empty.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) — `clusters`
// carries the AKS cluster inventory the v0.89.70 chunk 3 scanner
// populates. Optional on the wire (omitempty Go JSON tag) so older
// scan rows from before the scanner extension don't render an
// undefined array; the Inventory tab's Kubernetes sub-tab treats
// undefined as empty.
export interface ScanAzureResponse {
  connection_id: string;
  subscription_id: string;
  location: string;
  compute: ComputeInstanceSnapshot[];
  databases?: DatabaseInstanceSnapshot[];
  clusters?: ClusterSnapshot[];
  instrumented_count: number;
  uninstrumented_count: number;
  partial: boolean;
  partial_reason?: string;
  failed_services?: string[];
  scan_id: string;
}

export function scanAzureConnection(id: string): Promise<ScanAzureResponse> {
  return apiPost<ScanAzureResponse>(
    `/discovery/azure/connections/${encodeURIComponent(id)}/scan`,
  );
}

// --- Wire-encoding helper ------------------------------------------

// encodeClientSecretForWire base64-encodes the raw Service Principal
// client_secret the operator pasted into the wizard textarea. The
// server expects the payload to be a base64 string
// (azureCreateConnectionRequest.SealedSecret in the Go handler)
// rather than a raw secret literal — design doc §7 calls out the
// rationale: the base64 wrapping insulates the JSON wire shape from
// any special characters Azure may emit in a generated secret
// (slashes, plus, equals are common in the `password` field of an
// `az ad sp create-for-rbac` output) and keeps the GCP / Azure
// create-request shapes uniform.
//
// btoa() only accepts Latin-1; Azure SP secrets are ASCII so the
// encoding is safe, but we use the TextEncoder + base64 path for
// forward-compat with any future non-ASCII content.
export function encodeClientSecretForWire(secret: string): string {
  const bytes = new TextEncoder().encode(secret);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}
