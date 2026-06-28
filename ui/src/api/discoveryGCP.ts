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
import { pollRecommendationJob } from "./discovery";
import type { LoadBalancerSnapshot, ObjectStoreSnapshot } from "./discovery";
import type {
  EventSourceRow,
  GenerateRecommendationsResponse,
  RecommendationJobAccepted,
} from "./discovery";
import type { AWSTerraformImportResponse } from "./discovery";

// --- Storage type --------------------------------------------------

// GCPConnection mirrors gcpconnstore.GCPConnection. The SealedSA
// bytes carry json:"-" on the Go struct so this wire shape NEVER
// includes the sealed credential blob — operators see "this project
// is connected"; they cannot read back the SA key material from the
// UI. Mirrors the AWS CloudConnection posture.
export type { EventSourceRow };

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

// enableGCPDemoConnection provisions the built-in credential-free demo GCP
// project (v0.89.243 first-user onboarding parity). Returns the demo
// connection so the caller can refresh the list and scan it.
export function enableGCPDemoConnection(): Promise<GCPConnection> {
  return apiPost<GCPConnection>("/discovery/gcp/demo/enable", {});
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
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
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
  enhanced_monitoring_enabled?: boolean; // AWS RDS (slice 1)

  query_insights_enabled?: boolean; // GCP Cloud SQL (slice 2)
  sql_insights_diag_enabled?: boolean; // Azure SQL (slice 2)
  database_management_enabled?: boolean; // OCI DB (slice 2)

  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
}

// ClusterSnapshot mirrors scanner.ClusterSnapshot — the shared
// cross-cloud Kubernetes cluster row shape. The three per-cloud
// observability axis flags are optional because each cloud only
// populates the one that matches its scanner: AWS EKS uses the
// composite control_plane_logging + addons rule (slice 1); GCP GKE
// sets managed_prometheus_enabled (slice 2); Azure AKS sets
// azure_monitor_enabled (slice 2); OCI OKE sets
// operations_insights_enabled (slice 2). The `provider` discriminator
// routes the inventory table's instrumentation-column rendering to
// the right axis.
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
  managed_prometheus_enabled?: boolean; // GCP GKE
  azure_monitor_enabled?: boolean; // Azure AKS
  operations_insights_enabled?: boolean; // OCI OKE

  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
}

// ServerlessRow mirrors scanner.ServerlessInstanceSnapshot — the
// shared cross-cloud serverless row shape. Serverless tier slice 1
// chunk 5 (v0.89.92, #725 Stream 123). Surface is one of "cloudrun" or
// "cloudfunc" on GCP; AWS / Azure / OCI per-cloud helpers reuse this
// same shape with their own surface values. has_trace_axis is the
// cloud-native trace primitive (Cloud Trace annotation / env for GCP);
// has_otel_distro is the OpenTelemetry distribution / sidecar / layer.
export interface ServerlessRow {
  provider: "aws" | "gcp" | "azure" | "oci";
  surface: "lambda" | "cloudrun" | "cloudfunc" | "azfunc" | "ocifunc";
  account_id: string;
  region: string;
  resource_name: string;
  resource_arn: string;
  runtime?: string;
  has_trace_axis: boolean;
  has_otel_distro: boolean;
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4.
  last_seen_at?: string;
  // cold_start_p95_ms — Cold-start latency analysis slice 2 chunk 4
  // (v0.89.119, #759 Stream 157). 24-hour rolling P95 cold-start
  // observation sourced from the cold_start_observation table at
  // scan-response time. Undefined when no observation has been
  // persisted yet — the column renders "—".
  cold_start_p95_ms?: number;
  // cold_start_exceeds_threshold — Cold-start latency analysis slice
  // 2 chunk 4 (v0.89.119). Pre-computed amber-color predicate the
  // UI's ColdStartCell reads to color the Cold-start P95 cell amber
  // when true. The server applies the 1.5x ratio + 500ms floor rule
  // so the UI keeps a single definition of "amber" across columns
  // and the per-resource cold-start drill-down endpoint.
  cold_start_exceeds_threshold?: boolean;
  // sampling_ratio + sampling_exceeds_floor — Sampling rate analysis
  // slice 1 chunk 3 (v0.89.124, #764 Stream 162). See discovery.ts
  // ServerlessRow godoc for the join + amber-color semantics. Cloud
  // Run + Cloud Functions both participate (the slice 1 contract
  // covers all 5 serverless surfaces).
  sampling_ratio?: number | null;
  sampling_exceeds_floor?: boolean | null;
  // current_error_rate + error_rate_exceeds_threshold — Error rate
  // correlation slice 1 chunk 3 (v0.89.129). See discovery.ts
  // ServerlessRow godoc; all 5 serverless surfaces participate.
  current_error_rate?: number | null;
  error_rate_exceeds_threshold?: boolean | null;
  detail?: Record<string, unknown>;
}

// OrchestrationRow — orchestration tier slice 1 chunk 4 (v0.89.97,
// #731 Stream 129). Mirrors scanner.OrchestrationInstanceSnapshot —
// the shared cross-cloud orchestration row shape. Surface is one of
// "stepfunc" (AWS Step Functions), "workflows" (GCP Workflows), or
// "logicapps" (Azure Logic Apps). OCI orchestration is deferred to
// slice 2; OCI scan responses always return orchestrations: [].
//
// workflow_type carries STANDARD/EXPRESS for stepfunc and
// Standard/Consumption for logicapps; workflows leaves it empty.
export interface OrchestrationRow {
  provider: "aws" | "gcp" | "azure";
  surface: "stepfunc" | "workflows" | "logicapps";
  account_id: string;
  region: string;
  resource_name: string;
  resource_arn?: string;
  workflow_type?: string;
  has_trace_axis: boolean;
  has_log_axis: boolean;
  last_seen_at?: string;
  detail?: Record<string, unknown>;
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
//
// Kubernetes tier slice 2 (v0.89.71, #702 Stream 100) — `clusters`
// carries the GKE cluster inventory the chunk 2 scanner extension
// populates on result.Clusters. Optional on the wire (omitempty Go
// JSON tag) so older scan rows from before the v0.89.70 scanner
// extension don't render an undefined array; the Inventory tab's
// Kubernetes sub-tab treats undefined as empty.
export interface ScanGCPResponse {
  connection_id: string;
  project_id: string;
  region: string;
  compute: ComputeInstanceSnapshot[];
  databases?: DatabaseInstanceSnapshot[];
  clusters?: ClusterSnapshot[];
  object_stores?: ObjectStoreSnapshot[];
  load_balancers?: LoadBalancerSnapshot[];
  // serverless — serverless tier slice 1 chunk 5 (v0.89.92, #725
  // Stream 123). Cloud Run + Cloud Functions inventory from the
  // chunk 2 GCP scanner extension. Optional on the wire (omitempty
  // Go JSON tag); the Inventory tab's Serverless sub-tab treats
  // undefined as empty.
  serverless?: ServerlessRow[];
  // orchestrations — orchestration tier slice 1 chunk 4 (v0.89.97,
  // #731 Stream 129). GCP Workflows inventory from the chunk 2 GCP
  // Workflows scanner extension. Optional on the wire (omitempty Go
  // JSON tag); the Inventory tab's Orchestration sub-tab treats
  // undefined as empty.
  orchestrations?: OrchestrationRow[];
  // event_sources — event-source tier inventory (SNS / SQS / Cloud
  // Tasks / Service Bus / Queues / etc.) from the per-cloud
  // event-source scanner. Optional on the wire; the Inventory tab's
  // Event Sources sub-tab treats undefined as empty.
  event_sources?: EventSourceRow[];
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

// generateGCPRecommendations asks the discovery proposer to draft an
// instrumentation plan from a GCP scan result. Mirrors
// generateAWSRecommendations: the browser POSTs the scan it just
// rendered; the server builds a Provider="gcp" DiscoveryScanContext and
// walks the plan-kind result into typed Recommendations.
export async function generateGCPRecommendations(
  connectionID: string,
  scanResult: ScanGCPResponse,
): Promise<GenerateRecommendationsResponse> {
  // v0.89.210 async: kick off the proposer job, then poll the shared
  // provider-agnostic job-status endpoint to completion.
  const accepted = await apiPost<RecommendationJobAccepted>(
    `/discovery/gcp/connections/${encodeURIComponent(connectionID)}/recommendations`,
    { scan_result: scanResult },
  );
  return pollRecommendationJob(accepted.job_id);
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

// generateGCPTerraformImport renders Terraform import{} blocks for the
// GCP compute resources in a scan so the operator can adopt un-managed
// resources via `terraform plan -generate-config-out` (env->TF slice
// 3d). Synchronous: the endpoint returns the rendered .tf directly.
export async function generateGCPTerraformImport(
  connectionID: string,
  scan: ScanGCPResponse,
): Promise<AWSTerraformImportResponse> {
  return apiPost<AWSTerraformImportResponse>(
    `/discovery/gcp/connections/${encodeURIComponent(connectionID)}/terraform-import`,
    { scan_result: scan },
  );
}
