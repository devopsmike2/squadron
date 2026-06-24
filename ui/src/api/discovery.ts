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

// RowSpanQuality — v0.89.87 span quality slice 1 chunk 3. Compact
// per-row summary of the three quality percentages the receiver's
// quality counters expose. Travels on every Inventory row that
// supports a Quality dot (compute, functions, databases, clusters).
// The drill-down panel hits the dedicated per-resource endpoint for
// placeholder observations; this row payload is intentionally
// minimal so the scan response stays small.
//
// Chunk 2 (sibling branch, v0.89.86) extends the scan marshalling
// to populate this field server-side. Until chunk 2 merges this is
// always undefined and the QualityDot renders gray.
export interface RowSpanQuality {
  orphan_pct: number;
  missing_attr_pct: number;
  attr_mismatch_pct: number;
}

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
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4. ISO
  // timestamp of the most recent span the receiver observed for
  // this resource. Undefined when traces have never been seen — the
  // Inventory table renders "never" with a warning indicator on
  // that path.
  last_seen_at?: string;
  // span_quality — v0.89.87 span quality slice 1 chunk 3. Server-
  // side summary of the three pathology percentages. See
  // RowSpanQuality godoc.
  span_quality?: RowSpanQuality;
}

// FunctionRuntimeSnapshot mirrors scanner.FunctionRuntimeSnapshot.
export interface FunctionRuntimeSnapshot {
  resource_id: string;
  name: string;
  runtime: string;
  has_otel_layer: boolean;
  region: string;
  // span_quality — v0.89.87 span quality slice 1 chunk 3. See
  // RowSpanQuality godoc.
  span_quality?: RowSpanQuality;
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
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4. See
  // ComputeInstanceSnapshot.last_seen_at godoc.
  last_seen_at?: string;
  // span_quality — v0.89.87 span quality slice 1 chunk 3. See
  // RowSpanQuality godoc.
  span_quality?: RowSpanQuality;
}

// ObjectStoreSnapshot mirrors scanner.ObjectStoreSnapshot. Slice 3a
// (v0.88.0) — S3 observability is a single-axis rule:
// `server_access_logging_enabled` is the only field that gates the
// instrumented-count tally. `request_metrics_enabled` is
// informational only — surfaced as an additional badge column so the
// operator sees per-bucket request-rate observability state, but it
// does NOT participate in the rule. The proposer prompt only emits
// enablement recommendations for the logging lever.
export interface ObjectStoreSnapshot {
  resource_id: string;
  region: string;
  server_access_logging_enabled: boolean;
  request_metrics_enabled: boolean;
  tags: Record<string, string>;
}

// LoadBalancerSnapshot mirrors scanner.LoadBalancerSnapshot. Slice 3a
// (v0.88.0) — ALB / NLB / GWLB observability is a single-axis rule
// on `access_logs_enabled`. `access_logs_s3_bucket` is the
// operator-chosen target the proposer cross-references against the
// scan's `object_stores` list — recommendations prefer naming an
// existing bucket Squadron already sees. The Inventory tab renders
// the target bucket inline with the badge so the relationship is
// visible at a glance.
export interface LoadBalancerSnapshot {
  resource_id: string;
  name: string;
  // type: one of "application" | "network" | "gateway" on AWS.
  type: string;
  // scheme: one of "internet-facing" | "internal" on AWS.
  scheme: string;
  access_logs_enabled: boolean;
  access_logs_s3_bucket?: string;
  region: string;
  tags: Record<string, string>;
}

// ClusterAddon mirrors scanner.ClusterAddon. Slice 3b (v0.89.0).
// Name + status drive the EKS observability-detection rule the
// proposer reads (ADOT or amazon-cloudwatch-observability addon,
// ACTIVE status). version is informational; it renders in the
// Inventory tab badge tooltip but does not gate the rule.
export interface ClusterAddon {
  name: string;
  version: string;
  status: string;
}

// ClusterSnapshot mirrors scanner.ClusterSnapshot. Slice 3b
// (v0.89.0) — EKS / GKE / AKS observability is a COMPOSITE rule:
// `control_plane_logging` must contain BOTH "api" AND "audit" AND
// at least one `addons` entry must have name == "adot" OR
// "amazon-cloudwatch-observability" with status == "ACTIVE". The
// Inventory tab surfaces both axes as independent badge groups so
// the operator sees at a glance which axis is missing, matching
// the proposer prompt's "BOTH must hold" framing.
export interface ClusterSnapshot {
  resource_id: string;
  name: string;
  kubernetes_version: string;
  status: string;
  control_plane_logging: string[];
  addons: ClusterAddon[];
  nodegroup_count: number;
  fargate_profile_count: number;
  region: string;
  tags: Record<string, string>;
  // last_seen_at — v0.89.77 trace integration slice 1 chunk 4. See
  // ComputeInstanceSnapshot.last_seen_at godoc.
  last_seen_at?: string;
  // span_quality — v0.89.87 span quality slice 1 chunk 3. See
  // RowSpanQuality godoc.
  span_quality?: RowSpanQuality;
}

// ServerlessRow — serverless tier slice 1 chunk 5 (v0.89.92, #725
// Stream 123). Mirrors scanner.ServerlessInstanceSnapshot. The
// per-provider Inventory tab's Serverless sub-tab renders one row per
// entry with the universal columns documented in
// docs/proposals/serverless-tier-slice1.md §7:
//
//   - resource_name + surface + runtime + region
//   - has_trace_axis as a check / cross (the cloud-native trace
//     primitive — X-Ray for Lambda, Cloud Trace for Cloud Run /
//     Functions, App Insights for Azure, APM for OCI)
//   - has_otel_distro as a check / cross (the OpenTelemetry
//     distribution / layer / sidecar / wrapper)
//   - last_seen_at as a relative-time column (per the v0.89.77 column
//     pattern); undefined when traces have never been seen
//   - span_quality as a QualityDot (per the v0.89.87 column pattern)
//
// surface is one of "lambda" | "cloudrun" | "cloudfunc" | "azfunc" |
// "ocifunc". GCP rows can be either cloudrun OR cloudfunc; AWS / Azure
// / OCI rows are always a single surface per provider.
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
  // last_seen_at — joined from the traceindex (v0.89.77). Undefined
  // when no spans have been observed; the table renders "never".
  last_seen_at?: string;
  // span_quality — span quality slice 1 chunk 3 (v0.89.87). See
  // RowSpanQuality godoc. Undefined renders the QualityDot gray.
  span_quality?: RowSpanQuality;
  // detail — surface-specific bag. Slice 1 surfaces it as raw JSON in
  // the row's drill-down tooltip; not rendered in the columns.
  detail?: Record<string, unknown>;
}

// OrchestrationRow — orchestration tier slice 1 chunk 4 (v0.89.97,
// #731 Stream 129). Mirrors scanner.OrchestrationInstanceSnapshot. The
// per-provider Inventory tab's Orchestration sub-tab renders one row
// per entry with the columns documented in
// docs/proposals/orchestration-tier-slice1.md §7:
//
//   - resource_name + surface + workflow_type + region
//   - has_trace_axis as a check / cross (X-Ray active tracing for
//     Step Functions, callLogLevel == LOG_ALL_CALLS for Workflows,
//     APPLICATIONINSIGHTS_CONNECTION_STRING / diagnostic settings for
//     Logic Apps)
//   - has_log_axis as a check / cross (loggingConfiguration for
//     Step Functions, callLogLevel != UNSPECIFIED for Workflows,
//     diagnostic settings for Logic Apps)
//   - last_seen_at as a relative-time column; undefined when traces
//     have never been seen
//
// surface is one of "stepfunc" | "workflows" | "logicapps".
// workflow_type carries STANDARD/EXPRESS for stepfunc and
// Standard/Consumption for logicapps; workflows leaves it empty.
//
// OCI is intentionally absent from the provider union — slice 1
// doesn't ship an OCI orchestration scanner, so OCI scan responses
// always return orchestrations: [] and the OCI page conditionally
// hides the sub-tab.
export interface OrchestrationRow {
  provider: "aws" | "gcp" | "azure";
  surface: "stepfunc" | "workflows" | "logicapps";
  account_id: string;
  region: string;
  resource_name: string;
  resource_arn?: string;
  // workflow_type — surface subtype. Step Functions: "STANDARD" /
  // "EXPRESS". Logic Apps: "Standard" / "Consumption". Workflows
  // leaves it empty (the API has a single workflow type).
  workflow_type?: string;
  has_trace_axis: boolean;
  has_log_axis: boolean;
  // last_seen_at — joined from the traceindex; undefined when the
  // index has no observation for this resource.
  last_seen_at?: string;
  // span_quality — same posture as ServerlessRow.span_quality.
  span_quality?: RowSpanQuality;
  // detail — per-surface bag the per-cloud Inventory tab can render
  // as a per-row drill-down.
  detail?: Record<string, unknown>;
}

// EventSourceRow — event source tier slice 1 chunk 5 (v0.89.102,
// #738 Stream 136). Mirrors scanner.EventSourceInstanceSnapshot. The
// per-provider Inventory tab's Event sources sub-tab renders one row
// per entry with the columns documented in
// docs/proposals/event-source-tier-slice1.md §7:
//
//   - resource_name + surface + source_type + region
//   - has_trace_axis as a check / cross (Schemas Discoverer / log-target
//     proxy for EventBridge, tracingConfig.samplingRatio > 0 for
//     Pub/Sub, diagnostic settings for Service Bus, OCI Logging for
//     Streaming)
//   - has_log_axis as a check / cross (log-target rule for
//     EventBridge, schemaSettings for Pub/Sub, diagnostic settings
//     routing to Log Analytics for Service Bus, Logging log group for
//     Streaming)
//   - last_seen_at as a relative-time column
//   - span_quality (AWS only, per the v0.89.92 / v0.89.97 slice 1
//     constraint)
//
// surface is one of "eventbridge" | "pubsub" | "servicebus" |
// "streaming". source_type carries the per-surface subtype string
// (bus / topic / queue / namespace / stream).
//
// Unlike OrchestrationRow which excluded OCI from the provider union,
// EventSourceRow includes all four providers because OCI Streaming
// ships as a real surface in slice 1 (see design doc §3.4).
export interface EventSourceRow {
  provider: "aws" | "gcp" | "azure" | "oci";
  surface: "eventbridge" | "pubsub" | "servicebus" | "streaming";
  account_id: string;
  region: string;
  resource_name: string;
  resource_arn?: string;
  // source_type — per-surface subtype: bus / topic / queue / namespace
  // / stream.
  source_type?: string;
  has_trace_axis: boolean;
  has_log_axis: boolean;
  // last_seen_at — joined from the traceindex; undefined when the
  // index has no observation for this resource.
  last_seen_at?: string;
  // span_quality — same posture as ServerlessRow.span_quality. AWS
  // only in slice 1 per the established pattern.
  span_quality?: RowSpanQuality;
  // detail — per-surface bag the per-cloud Inventory tab can render
  // as a per-row drill-down.
  detail?: Record<string, unknown>;
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
  // object_stores and load_balancers join the wire shape in slice 3a
  // (v0.88.0). Same non-optional posture — Go handler always emits
  // arrays, the UI's empty-state branch is a single `.length === 0`
  // check.
  object_stores: ObjectStoreSnapshot[];
  load_balancers: LoadBalancerSnapshot[];
  // clusters joins the wire shape in slice 3b (v0.89.0). Same
  // non-optional posture as the other category arrays.
  clusters: ClusterSnapshot[];
  // serverless — serverless tier slice 1 chunk 5 (v0.89.92, #725
  // Stream 123). The per-provider Inventory tab's Serverless sub-tab
  // reads from this field; empty array on cold start and on
  // deployments scanning before the v0.89.90 scanner extension. The
  // Go handler emits an array (never null) so the UI's empty-state
  // branch is a single `.length === 0` check.
  serverless?: ServerlessRow[];
  // orchestrations — orchestration tier slice 1 chunk 4 (v0.89.97,
  // #731 Stream 129). The per-provider Inventory tab's Orchestration
  // sub-tab reads from this field. The Go handler emits an array
  // (never null) so the UI's empty-state branch is a single
  // `.length === 0` check. Always empty in OCI inventory responses
  // in slice 1; the OCI page hides the sub-tab when empty.
  orchestrations?: OrchestrationRow[];
  // event_sources — event source tier slice 1 chunk 5 (v0.89.102,
  // #738 Stream 136). The per-provider Inventory tab's Event sources
  // sub-tab reads from this field. The Go handler emits an array
  // (never null) so the UI's empty-state branch is a single
  // `.length === 0` check. Unlike orchestrations, OCI inventory
  // responses populate this field in slice 1 since OCI Streaming
  // ships as a real surface.
  event_sources?: EventSourceRow[];
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

// --- Scan-all endpoint shapes (v0.89.7b Stream 23, #619) ------------

// AWSScanAllSucceededAccount mirrors the awsScanAllAccountRow shape on
// the wire (snake_case keys). Per-account counts are surfaced so the
// UI can compute per-account coverage ratios without re-fetching the
// per-account /scan endpoint for each succeeded row.
export interface AWSScanAllSucceededAccount {
  account_id: string;
  scan_id: string;
  resource_count: number;
  instrumented_count: number;
  uninstrumented_count: number;
}

// AWSScanAllFailedAccount mirrors the awsScanAllFailureRow shape on
// the wire. error_code is the stable identifier (matches the per-
// account scan handler's HumanizedError.Code convention);
// humanized_message is the operator-visible prose the UI renders
// verbatim under the per-account "scan failed" card.
export interface AWSScanAllFailedAccount {
  account_id: string;
  error_code: string;
  humanized_message: string;
}

// AWSScanAllResponse mirrors awsScanAllResponse on the wire. partial
// is true when at least one account in the fan-out failed; the UI
// renders a yellow banner above the per-account result grid in that
// case. concurrency is the EFFECTIVE bound the orchestrator used
// after defaults + cap were applied — diagnostic surface for the
// operator who passed a value and wants to see what was honored.
export interface AWSScanAllResponse {
  scan_all_id: string;
  total_accounts: number;
  succeeded_accounts: AWSScanAllSucceededAccount[];
  failed_accounts: AWSScanAllFailedAccount[];
  total_resources: number;
  total_instrumented: number;
  total_uninstrumented: number;
  partial: boolean;
  concurrency: number;
}

// scanAllAWS fans out a multi-account scan via the v0.89.7a Stream 21
// orchestrator endpoint. The request shape is query-param-only — the
// orchestrator drives every connection it can find in the credstore
// rather than accepting a per-call account list. Empty regions slips
// the connection's stored region list through; explicit regions
// override per-connection storage. Empty / omitted concurrency falls
// back to the orchestrator's default (3) and clamps at the cap (8).
//
// The endpoint blocks for the duration of the slowest per-account
// scan in the fan-out — slice 1 (UI half) doesn't stream per-account
// completions. The spinner UX in DiscoveryAWS.tsx is "all accounts
// scanning… → all flip at once" which is honest about the response
// shape and forward-compatible with a future streaming endpoint.
export function scanAllAWS(opts: {
  regions?: string[];
  concurrency?: number;
}): Promise<AWSScanAllResponse> {
  const params = new URLSearchParams();
  if (opts.regions && opts.regions.length > 0) {
    params.set("regions", opts.regions.join(","));
  }
  if (opts.concurrency && opts.concurrency > 0) {
    params.set("concurrency", String(opts.concurrency));
  }
  const qs = params.toString();
  const path = `/discovery/aws/scan-all${qs ? `?${qs}` : ""}`;
  return apiPost<AWSScanAllResponse>(path);
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

// --- Recommendation-exclusion endpoint (v0.89.38 #658 Stream 56) -----
//
// v0.89.38 (#658 Stream 56, #531 slice 2 chunk 5) — operator-set
// exclusion affordance on the discovery Recommendations tab. The
// "Don't propose this again" button on each recommendation row POSTs
// the wire shape below; the Go handler
// HandleAWSRecommendationExclude (internal/api/handlers/discovery.go)
// upserts the iac_recommendation_verdicts row and emits the
// discovery_recommendation.excluded /
// discovery_recommendation.exclude_cleared audit event on transitions
// only. See docs/proposals/531-proposer-learning-slice2.md §4.2 +
// §10 contract item 9.
//
// ResourceID is optional. When absent / empty the exclusion is scoped
// to the entire recommendation kind at (connection_id × account_id ×
// region). When present, the exclusion targets a single resource
// (the §11 Q4 distinction). v1 (chunk 5) picks the granularity from
// whether the recommendation row carries affected_resources; v2 may
// surface a dropdown letting the operator pick explicitly.
//
// Excluded is the desired final state. true on "Don't propose this
// again"; false on the inverse "Restore as recommendation" click.

export interface ExcludeRecommendationRequest {
  recommendation_id: string;
  connection_id: string;
  account_id: string;
  region: string;
  recommendation_kind: string;
  // Optional. Empty / undefined scopes the exclusion to the entire
  // recommendation kind at the given scope; non-empty scopes it to a
  // single resource. The proposer's prompt renderer surfaces the
  // distinction with different instruction text (§11 Q4).
  resource_id?: string;
  excluded: boolean;
}

export interface ExcludeRecommendationResponse {
  recommendation_id: string;
  excluded: boolean;
  // ISO timestamp stamped by the handler on a false -> true
  // transition. Absent on excluded=false responses (the row stays
  // around with cleared stamps).
  excluded_at?: string;
  excluded_by?: string;
}

export function setRecommendationExclusion(
  req: ExcludeRecommendationRequest,
): Promise<ExcludeRecommendationResponse> {
  return apiPost<ExcludeRecommendationResponse>(
    "/discovery/aws/recommendations/exclude",
    req,
  );
}

// --- Recommendation-exclusion list endpoint (v0.89.40 #660 Stream 58)
//
// v0.89.40 (#660 Stream 58, #531 slice 2 chunk 5 follow-on) — read
// surface for the operator-set exclusion table. The Go handler
// HandleAWSRecommendationListExcluded (internal/api/handlers/
// discovery.go) returns every persisted iac_recommendation_verdicts
// row whose exclude_from_learning bit is set, scoped to the supplied
// (connection_id × account_id × region) tuple. The Recommendations
// tab calls this helper on mount and seeds its `excludedSet` from
// the returned recommendation_id values, so the Excluded badges
// survive a page refresh — closing the chunk 5 TODO.
//
// Errors propagate as Error objects (the base fetch wrapper builds
// HumanizedError-aware messages). The caller in DiscoveryAWS.tsx
// degrades gracefully on failure: console.error + leave the set
// empty, so the page still renders the toggle.

export interface ListExcludedRecommendationsRequest {
  connection_id: string;
  account_id: string;
  region: string;
  // Optional. When omitted the server uses its default (100) and
  // clamps at the storage method's hard ceiling (1000). The UI
  // doesn't paginate today — chunk 5 ships a small per-tab list, so
  // the default is comfortably above any realistic Recommendations
  // tab size.
  limit?: number;
}

export interface ExcludedRecommendation {
  recommendation_id: string;
  recommendation_kind: string;
  // Optional. Empty / undefined means the exclusion is kind-level
  // (scoped to all resources of recommendation_kind in this scope);
  // non-empty scopes to a specific resource ID. Mirrors the
  // server-side projection over iac_recommendation_verdicts.
  resource_id?: string;
  // ISO timestamp stamped when the row's exclude_from_learning bit
  // was last flipped from false → true. Cleared rows are filtered
  // server-side, so non-empty here means the exclusion is currently
  // active.
  excluded_at: string;
  excluded_by: string;
}

interface listExcludedRecommendationsResponse {
  excluded: ExcludedRecommendation[];
}

export async function listExcludedRecommendations(
  req: ListExcludedRecommendationsRequest,
): Promise<ExcludedRecommendation[]> {
  const params: Record<string, string> = {
    connection_id: req.connection_id,
    account_id: req.account_id,
    region: req.region,
  };
  if (req.limit && req.limit > 0) {
    params.limit = String(req.limit);
  }
  const resp = await apiGet<listExcludedRecommendationsResponse>(
    "/discovery/aws/recommendations/excluded",
    params,
  );
  return resp.excluded ?? [];
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
