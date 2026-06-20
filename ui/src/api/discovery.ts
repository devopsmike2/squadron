// API client for v0.85 Stream 2D discovery surface. Mirrors the Go
// handlers in internal/api/handlers/discovery.go and the wizard
// definitions in internal/discovery/wizard — keep these in sync.
//
// The wizard React shell consumes ConnectorWizard / WizardStep /
// WizardAction / ValidationRule as the declarative definition that
// drives rendering. Validate/Save are the two server endpoints the
// shell calls during the test-before-commit and persist phases of the
// AWS connect flow.
//
// Slice-1 design trade-off: the AWS wizard definition is hardcoded
// client-side (see ./../data/awsWizard.ts) rather than fetched from
// the server. The Go AWSWizard() factory remains the canonical
// definition; a future slice will add a GET endpoint and the React
// shell will swap to a server-fetched value with the same types.
// Keeping it client-side for slice 1 avoids the extra round-trip in
// the very first frame of the connect flow and removes one moving
// piece — the wizard is small (5 steps) and rarely changes.

import { apiGet, apiPost } from "./base";
import type { Recommendation } from "./recommendations";

// --- Validation endpoint shapes -------------------------------------

// HumanizedError mirrors scanner.HumanizedError. The wizard renders
// `message` verbatim and uses `suggested_step` to deep-link back to
// the step the operator needs to fix. `code` is the provider's raw
// error code, surfaced so a support thread can pattern-match against
// AWS's own documentation.
export interface HumanizedError {
  code: string;
  message: string;
  suggested_step: string;
  doc_link?: string;
}

// PreflightCheck mirrors scanner.PreflightCheck. One entry per service
// the validate endpoint probed (slice 1: ec2 + lambda). `sample_count`
// is capped at 5 — this is a permissions probe, not an inventory walk.
export interface PreflightCheck {
  service: string;
  ok: boolean;
  sample_count: number;
  err?: HumanizedError;
}

// ValidationResult is the typed payload the validate endpoint returns.
// The shell renders this directly as the "what just happened" panel.
// `errors` is a flattened convenience list — every HumanizedError in
// the result (assume-role + per-service) lands here in walk order so
// the panel can show one list without recursing the typed structs.
export interface ValidationResult {
  assume_role_ok: boolean;
  assume_role_err?: HumanizedError;
  preflight: PreflightCheck[];
  errors?: HumanizedError[];
}

// ValidateRequest is the wire shape for POST /discovery/aws/validate.
// `account_id` is optional on the wire — the server falls back to
// sts:GetCallerIdentity when absent — but the wizard always populates
// it from Step 1 so the response is self-describing even on a failed
// assume-role.
export interface ValidateRequest {
  role_arn: string;
  external_id: string;
  regions: string[];
  account_id?: string;
}

export function validateAWSConnection(
  req: ValidateRequest,
): Promise<ValidationResult> {
  return apiPost<ValidationResult>("/discovery/aws/validate", req);
}

// --- Save endpoint shapes -------------------------------------------

// SaveConnectionRequest is the wire shape for
// POST /discovery/aws/connections. The server re-runs validate one
// last time before persisting (operator may have edited the role
// between Validate and Save), then encrypts the role ARN + ExternalId
// into the credstore and emits discovery.aws.connection_created.
export interface SaveConnectionRequest {
  account_id: string;
  role_arn: string;
  external_id: string;
  display_name: string;
  regions: string[];
}

export interface SaveConnectionResponse {
  connection_id: string;
  status: string;
}

export function saveAWSConnection(
  req: SaveConnectionRequest,
): Promise<SaveConnectionResponse> {
  return apiPost<SaveConnectionResponse>("/discovery/aws/connections", req);
}

// --- List endpoint shapes (Stream 2E) -------------------------------

// CloudConnection mirrors the awsConnectionRow wire shape the list
// handler emits. ONLY display fields — the role ARN, ExternalId, and
// encrypted credential bytes are never returned to the browser.
// Operators see "this account is connected"; they cannot read back
// trust-policy material from the UI.
export interface CloudConnection {
  account_id: string;
  display_name: string;
  regions: string[];
  // created_at is an ISO-8601 timestamp string. The card UI formats it
  // as a relative time via lib/utils helpers.
  created_at: string;
}

export interface ListConnectionsResponse {
  connections: CloudConnection[];
}

export function listAWSConnections(): Promise<ListConnectionsResponse> {
  return apiGet<ListConnectionsResponse>("/discovery/aws/connections");
}

// --- Scan endpoint shapes (Stream 2E) -------------------------------

// ComputeInstanceSnapshot mirrors scanner.ComputeInstanceSnapshot. The
// Inventory tab renders one row per entry — resource_id, instance_type,
// region, and a HasOTel detection badge.
export interface ComputeInstanceSnapshot {
  resource_id: string;
  instance_type: string;
  tags: Record<string, string>;
  has_otel: boolean;
  os_family: string;
  region: string;
}

// FunctionRuntimeSnapshot mirrors scanner.FunctionRuntimeSnapshot.
export interface FunctionRuntimeSnapshot {
  resource_id: string;
  name: string;
  runtime: string;
  has_otel_layer: boolean;
  region: string;
}

// DatabaseInstanceSnapshot mirrors scanner.DatabaseInstanceSnapshot.
// Slice 2 (v0.87) — RDS observability is a two-part rule:
// `performance_insights_enabled` AND `enhanced_monitoring_enabled`
// must both be true for the instance to count as covered. The
// Inventory tab surfaces them as independent badge columns so the
// operator can see at a glance which lever is missing, matching the
// proposer prompt's "PI and EM are independent levers" framing.
export interface DatabaseInstanceSnapshot {
  resource_id: string;
  engine: string;
  engine_version: string;
  instance_class: string;
  performance_insights_enabled: boolean;
  enhanced_monitoring_enabled: boolean;
  region: string;
  tags: Record<string, string>;
}

// ScanResult is the typed payload the scan endpoint returns. Mirrors
// scanner.Result via the marshalScanResult wire shape on the Go side.
// scan_started_at / scan_completed_at are ISO-8601 strings; partial
// flags an incomplete walk (the UI renders a yellow warning above the
// section list when set).
export interface ScanResult {
  scan_id: string;
  scan_started_at: string;
  scan_completed_at: string;
  account_id: string;
  provider: string;
  regions: string[];
  compute: ComputeInstanceSnapshot[];
  functions: FunctionRuntimeSnapshot[];
  // databases is the v0.87 slice 2 addition. The field is non-optional
  // on the wire (the Go handler always emits an array, never null) so
  // the UI's empty-state branch is a single `.length === 0` check.
  databases: DatabaseInstanceSnapshot[];
  instrumented_count: number;
  uninstrumented_count: number;
  partial: boolean;
  partial_reason?: string;
}

// runAWSScan triggers an on-demand scan against the supplied
// connection. Empty regions array falls back to the connection's
// stored Regions list (the server resolves the default). The endpoint
// blocks for the duration of the scan — slice 1 doesn't have async
// scans, which is fine for typical accounts but a known trade-off for
// 50k+ resource counts.
export function runAWSScan(
  accountID: string,
  regions?: string[],
): Promise<ScanResult> {
  return apiPost<ScanResult>(
    `/discovery/aws/connections/${encodeURIComponent(accountID)}/scan`,
    { regions: regions ?? [] },
  );
}

// --- Generate-recommendations endpoint shapes (Stream 2F) -----------

// GenerateRecommendationsResponse mirrors the Go
// awsGenerateRecommendationsResponse. When the proposer declines (no
// productive plan exists for the scan), `declined` is true and
// `reason` carries the model's brief explanation; the recommendations
// array is empty. Otherwise `reasoning` is the prose explanation that
// renders above the recommendations and `recommendations` is one
// entry per plan step — each with the IaC Terraform the operator
// runs through their existing IaC pipeline.
export interface GenerateRecommendationsResponse {
  declined: boolean;
  reason?: string;
  reasoning?: string;
  recommendations: Recommendation[];
}

// generateAWSRecommendations asks the discovery proposer to draft an
// instrumentation plan from the supplied scan result. The browser
// POSTs the scan result it just rendered; the server converts it to
// an ai.DiscoveryScanContext, calls ProposeFromDiscoveryScan, and
// walks the plan-kind result into typed Recommendations.
//
// The call may take 5-15 seconds depending on model latency — the
// Inventory tab's UX is "set a loading state, then auto-switch to the
// Recommendations tab when the response lands". The server emits a
// discovery.aws.recommendations_generated audit event on success and
// no audit event on declined.
export function generateAWSRecommendations(
  accountID: string,
  scanResult: ScanResult,
): Promise<GenerateRecommendationsResponse> {
  return apiPost<GenerateRecommendationsResponse>(
    `/discovery/aws/connections/${encodeURIComponent(accountID)}/recommendations`,
    { scan_result: scanResult },
  );
}

// --- Wizard shape (mirrors Go internal/discovery/wizard) ------------

// ActionKind enumerates the slice-1 step renderers the React shell
// supports. Adding a kind requires a matching branch in the shell.
export type ActionKind =
  | "copy_value"
  | "fill_field"
  | "deep_link"
  | "test_connection";

// ValidationKind enumerates the slice-1 inline-validation rules.
// `none` is the right choice for steps without an input
// (copy_value / deep_link / test_connection).
export type ValidationKind = "none" | "not_empty" | "regex";

export interface WizardAction {
  kind: ActionKind;
  // Payload is action-specific. Slice-1 contracts:
  //   - copy_value: { value?: string; field?: string; language?: string }
  //     (value may be computed client-side, e.g. the trust-policy JSON
  //     after the wizard inserts the ExternalId)
  //   - fill_field: { field: string; placeholder?: string }
  //   - deep_link: { url: string; label?: string }
  //   - test_connection: undefined
  //
  // `unknown` rather than a typed union because the Go side carries
  // `any` and a typed union here would require a codegen step to keep
  // synced. The shell branches on `kind` and reads the known shape.
  payload?: unknown;
}

export interface ValidationRule {
  kind: ValidationKind;
  pattern?: string; // required when kind === "regex"
  message?: string; // shown inline when the rule fails
}

export interface WizardStep {
  id: string;
  title: string;
  description: string;
  action: WizardAction;
  validation: ValidationRule;
  doc_link: string;
  recovery_hint: string;
}

export interface ConnectorWizard {
  provider: string;
  title: string;
  steps: WizardStep[];
}

// --- Draft shape ----------------------------------------------------

// WizardDraft is the in-flight state the shell carries across steps.
// All fields are optional because each step populates one or two; the
// validate and save endpoints validate the final shape server-side.
//
// The shell's ConnectorWizard component owns this state and threads it
// into onValidate / onSave callbacks the caller wires to the API
// functions above.
export interface WizardDraft {
  account_id?: string;
  external_id?: string;
  role_arn?: string;
  display_name?: string;
  regions?: string[];
  // principal_override is an optional ARN the operator can supply on
  // the trust-policy step to scope the trust to a specific IAM
  // identity instead of the default arn:aws:iam::<account>:root.
  // Validated client-side as an IAM ARN shape; the value flows into
  // the rendered trust-policy template only and is never sent to the
  // server (the customer is the only consumer — AWS itself).
  principal_override?: string;
  // external_id_override lets an operator resume an interrupted
  // wizard flow with the ExternalId they already pasted into AWS.
  // Validated client-side as a UUID v4 shape; when set, it replaces
  // the auto-generated UUID in both the rendered trust policy and
  // the validate/save payload. See #578.
  external_id_override?: string;
}
