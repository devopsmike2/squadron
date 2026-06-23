// API client for the v0.89.58 #684 Stream 82 (slice-1 chunk 4) OCI
// discovery surface. Mirrors the Go handlers in
// internal/api/handlers/discovery_oci.go and the endpoint surface
// documented in docs/proposals/oci-discovery-slice1.md §7 — keep
// these in sync. The shapes here intentionally parallel
// ui/src/api/discoveryAzure.ts so a future arc-spanning refactor that
// factors out a shared "cloud discovery" client has minimal diff
// surface.
//
// Slice-1 honesty:
//   - The pasted API Signing Key private key (a PEM-encoded RSA
//     keypair's private half) lives client-side in component state
//     only. It is base64-encoded over the wire and credstore-sealed
//     server-side under the squadron.oci_signing_key.v1 AAD. The
//     plaintext bytes NEVER land in localStorage / sessionStorage /
//     any persisted client surface. Same posture as the Azure SP
//     client_secret in discoveryAzure.ts and the GCP SA JSON in
//     discoveryGCP.ts.
//   - The page does NOT carry a recommendations helper yet — the
//     proposer extension ships in chunk 5 of this arc. The
//     Recommendations tab renders a "ships in chunk 5" stub until
//     that lands.
//   - Same fetch-wrapper + Bearer-token discipline as the AWS / GCP /
//     Azure counterparts via the shared ./base helpers.

import { apiDelete, apiGet, apiPost } from "./base";

// --- Storage type --------------------------------------------------

// OCIConnection mirrors ociconnstore.OCIConnection. The
// SealedPrivateKey bytes carry json:"-" on the Go struct so this wire
// shape NEVER includes the sealed credential blob — operators see
// "this tenancy is connected"; they cannot read back the RSA
// private-key material from the UI. Mirrors the Azure / GCP / AWS
// CloudConnection posture.
export interface OCIConnection {
  id: string;
  display_name: string;
  tenancy_ocid: string;
  user_ocid: string;
  fingerprint: string;
  region: string;
  learn_from_accepted_recommendations: boolean;
  // ISO-8601 timestamp strings. The page formats relative time via
  // an inline helper rather than pulling in date-fns — same posture
  // as the Azure / GCP pages.
  created_at: string;
  updated_at: string;
}

// --- Create / list / get / delete -----------------------------------

// CreateOCIConnectionRequest is the POST /discovery/oci/connections
// body. sealed_private_key is the base64 encoding of the raw PEM
// bytes of the API Signing Key private key — base64 over the wire
// keeps the payload uniform across providers (Azure uses the same
// base64 wrap on its client_secret, GCP wraps its SA JSON) and
// avoids JSON-escape edge cases for PEM bytes that include newlines
// and special characters. The server base64-decodes then
// credstore-seals before storage; the plaintext key does not enter
// SQLite at any point.
export interface CreateOCIConnectionRequest {
  display_name: string;
  tenancy_ocid: string;
  user_ocid: string;
  fingerprint: string;
  // base64(API Signing Key PEM bytes). Encoded by the wizard's
  // encodePrivateKeyForWire helper below.
  sealed_private_key: string;
  region: string;
}

export function createOCIConnection(
  req: CreateOCIConnectionRequest,
): Promise<OCIConnection> {
  return apiPost<OCIConnection>("/discovery/oci/connections", req);
}

// listOCIConnectionsResponse mirrors the ociListConnectionsResponse
// wire shape — Connections is always a non-nil array even when the
// store is empty, so the UI's empty-state branch is a single
// .length === 0 check. Matches the Azure / GCP precedent.
interface listOCIConnectionsResponse {
  connections: OCIConnection[];
}

export async function listOCIConnections(): Promise<OCIConnection[]> {
  const resp = await apiGet<listOCIConnectionsResponse>(
    "/discovery/oci/connections",
  );
  return resp.connections ?? [];
}

export function getOCIConnection(id: string): Promise<OCIConnection> {
  return apiGet<OCIConnection>(
    `/discovery/oci/connections/${encodeURIComponent(id)}`,
  );
}

export function deleteOCIConnection(id: string): Promise<void> {
  return apiDelete<void>(
    `/discovery/oci/connections/${encodeURIComponent(id)}`,
  );
}

// --- Validate -------------------------------------------------------

// OCIValidateErrorKind enumerates the discriminated failure modes
// the server's classifier returns (see classifyOCIScanError in
// internal/api/handlers/discovery_oci.go). The wizard branches on
// the kind to render a humanized remediation hint specific to the
// failure:
//
//   - permission_denied      : User lacks compute.instances:read on
//                              the tenancy.
//   - tenancy_not_found      : The configured tenancy_ocid doesn't
//                              resolve in the configured region.
//   - fingerprint_mismatch   : The fingerprint doesn't match the
//                              public key uploaded to OCI Console.
//   - private_key_invalid    : The pasted PEM is malformed or the
//                              cipher rejected the unseal.
//   - network                : Squadron's outbound path to
//                              *.oraclecloud.com is blocked.
//   - unknown                : Catch-all — surface the raw message
//                              and ask the operator to file an issue.
export type OCIValidateErrorKind =
  | "permission_denied"
  | "tenancy_not_found"
  | "fingerprint_mismatch"
  | "private_key_invalid"
  | "network"
  | "unknown";

export interface ValidateOCIResponse {
  ok: boolean;
  instance_count?: number;
  error_kind?: OCIValidateErrorKind;
  message?: string;
}

export function validateOCIConnection(
  id: string,
): Promise<ValidateOCIResponse> {
  return apiPost<ValidateOCIResponse>(
    `/discovery/oci/connections/${encodeURIComponent(id)}/validate`,
  );
}

// --- Scan ----------------------------------------------------------

// ComputeInstanceSnapshot mirrors scanner.ComputeInstanceSnapshot.
// The same shared shape AWS / GCP / Azure use — chunk 2 of the GCP
// arc settled on the shared struct. The Inventory tab renders one
// row per entry. For OCI slice 1 the OS family is left "unknown"
// (per design doc §9 + §14 Q5) — OCI exposes OS via the Image
// relationship which needs a secondary lookup. Slice 2 adds
// detection.
export interface ComputeInstanceSnapshot {
  resource_id: string;
  instance_type: string;
  tags: Record<string, string>;
  has_otel: boolean;
  os_family: string;
  region: string;
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
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

  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
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

  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
}

// ScanOCIResponse mirrors ociScanResponse on the wire. Unlike the
// Azure response which carries subscription_id + location, the OCI
// response carries tenancy_ocid + region — the OCI substrate scopes
// on tenancy and OCI's regional API endpoints make region a
// first-class field (per design doc §5).
//
// Field naming follows the Go json tags. instance_count is derived
// on the server from len(result.Compute) but kept on the wire so the
// UI can render a summary line without iterating the rows.
//
// Database tier slice 2 (v0.89.66, #695 Stream 93) — `databases`
// carries the OCI DB System + Autonomous Database inventory. The
// Inventory tab's Databases sub-tab treats undefined as empty so
// older scan rows from before the chunk 4 scanner extension render
// the empty-state placeholder.
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) — `clusters`
// carries the OKE cluster inventory the v0.89.70 chunk 4 OKE
// scanner populates. The Inventory tab's Kubernetes sub-tab treats
// undefined as empty so older scan rows from before the scanner
// extension render the empty-state placeholder.
export interface ScanOCIResponse {
  connection_id: string;
  tenancy_ocid: string;
  region: string;
  instance_count: number;
  instrumented_count: number;
  uninstrumented_count: number;
  partial: boolean;
  partial_reason?: string;
  failed_services?: string[];
  computes: ComputeInstanceSnapshot[];
  databases?: DatabaseInstanceSnapshot[];
  clusters?: ClusterSnapshot[];
  scan_id: string;
}

// scanOCIConnectionWireResponse mirrors the Go handler's
// ociScanResponse exactly. The Go field name is `compute` (singular)
// to parallel the AWS / GCP / Azure scan responses; the UI surface
// translates to `computes` (plural) for naming-consistency with the
// chunk-4 brief shape. Translation happens in scanOCIConnection
// below so callers only see the post-translate type.
interface scanOCIConnectionWireResponse {
  connection_id: string;
  tenancy_ocid: string;
  region: string;
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

export async function scanOCIConnection(
  id: string,
): Promise<ScanOCIResponse> {
  const wire = await apiPost<scanOCIConnectionWireResponse>(
    `/discovery/oci/connections/${encodeURIComponent(id)}/scan`,
  );
  const computes = wire.compute ?? [];
  return {
    connection_id: wire.connection_id,
    tenancy_ocid: wire.tenancy_ocid,
    region: wire.region,
    instance_count: computes.length,
    instrumented_count: wire.instrumented_count,
    uninstrumented_count: wire.uninstrumented_count,
    partial: wire.partial,
    partial_reason: wire.partial_reason,
    failed_services: wire.failed_services,
    computes,
    databases: wire.databases,
    clusters: wire.clusters,
    scan_id: wire.scan_id,
  };
}

// --- Wire-encoding helper ------------------------------------------

// encodePrivateKeyForWire base64-encodes the raw PEM bytes the
// operator pasted into the wizard textarea. The server expects the
// payload to be a base64 string
// (ociCreateConnectionRequest.SealedPrivateKey in the Go handler)
// rather than a raw PEM literal — design doc §7 calls out the
// rationale: PEM bytes include newlines, plus, slash, and equals
// characters (the BEGIN/END markers plus the base64-encoded DER
// body) that don't survive JSON string encoding cleanly without
// escaping. The base64 wrapping insulates the wire shape and keeps
// the Azure / GCP / OCI create-request shapes uniform.
//
// btoa() only accepts Latin-1; PEM bytes are ASCII (the encoded body
// is base64 alphabet + the markers + newlines) so the encoding is
// safe, but we use the TextEncoder + base64 path for forward-compat
// with any future non-ASCII content.
export function encodePrivateKeyForWire(pem: string): string {
  const bytes = new TextEncoder().encode(pem);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) {
    binary += String.fromCharCode(bytes[i]);
  }
  return btoa(binary);
}
