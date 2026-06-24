# Orchestration tier slice 1 — fifth tier across three clouds

**Status:** design doc, locked for slice 1 implementation.
Builds on the existing compute / database / kubernetes /
serverless tier work + the trace integration arc + the span
quality arc.

**See also:**
[Serverless tier slice 1](./serverless-tier-slice1.md),
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Trace integration slice 2](./trace-integration-slice2.md),
[Span quality slice 1](./span-quality-slice1.md).

## 1. Problem

Squadron's discovery surface now covers four tiers (compute /
database / kubernetes / serverless) across four clouds with the
trace integration + span quality arcs validating that telemetry
actually flows from each discovered resource. But operators
running production workloads almost universally have a fifth
surface Squadron doesn't see: **orchestration**.

Orchestration is where workflows live — Step Functions on AWS,
Workflows on GCP, Logic Apps on Azure. It's the layer where
business logic gets sequenced across the serverless +
compute + database + kubernetes surfaces Squadron already
covers. A Step Function with 12 states where state 7 silently
swallows the trace context is a real production failure mode;
operators see "the Step Function ran successfully" in
CloudWatch but Squadron's traceindex sees nothing past state 6.

This matters for three reasons:

- **Orchestration is where trace context propagation breaks
  most often.** Each state transition is a potential boundary
  where the W3C traceparent header may not propagate. A
  workflow that calls Lambda → Lambda → DynamoDB → another
  Lambda has 3 propagation boundaries; any of them dropping
  the header breaks correlation.
- **Squadron's existing serverless tier coverage is
  incomplete without orchestration.** Operators look at the
  Lambda inventory and see "this function has tracing on, has
  ADOT layer, last seen 2h ago — looks good." Then they look
  at the Step Function dashboard and see retries, failures,
  state-transition timeouts that don't show up in the
  per-function view. The orchestration tier closes that gap.
- **The competitive landscape:** Datadog / Honeycomb / Grafana
  Cloud have first-class orchestration coverage. Squadron's
  OSS positioning needs the same.

Slice 1 adds a fifth tier across three clouds:

- AWS: Step Functions (state machines)
- GCP: Workflows (workflow definitions)
- Azure: Logic Apps (workflow definitions)

For each, slice 1 detects:
1. Whether the state machine / workflow has the cloud-native
   trace axis enabled (X-Ray active tracing for Step Functions,
   Cloud Trace integration for Workflows, Diagnostic
   Settings → App Insights for Logic Apps).
2. The orchestration version / type (STANDARD vs EXPRESS for
   Step Functions; GA vs preview for Workflows; Standard vs
   Consumption for Logic Apps) — affects which spans get
   emitted.
3. Last span observation per orchestration via the existing
   traceindex.

Slice 1 wires this into the existing Discovery dashboard +
per-provider Inventory sub-tab pattern, mirroring the
serverless tier slice 1 arc structure.

## 2. Non-goals (slice 1)

- **OCI orchestration coverage.** OCI's orchestration
  primitives (Resource Manager, Process Automation) are shape-
  different from the AWS/GCP/Azure trio. Resource Manager is
  Terraform-as-a-service (infrastructure orchestration);
  Process Automation targets business processes (BPMN). Neither
  maps cleanly to the Step Functions / Workflows / Logic Apps
  pattern. Slice 2 candidate, scoped separately.
- **Per-state-transition trace propagation analysis.**
  Detecting "state 7 dropped the traceparent header" is the
  high-leverage detection but requires correlating per-state
  span emissions with the orchestration definition. Slice 2
  candidate; slice 1 only checks the top-level trace axis.
- **Workflow definition introspection.** Slice 1 reads the
  control-plane metadata (whether tracing is on, what type,
  current state). It does NOT parse the workflow definition
  JSON / YAML / ASL to detect propagation hops. Slice 2+.
- **Step Functions Distributed Map state analysis.**
  Distributed Map is a 2022+ feature that fans out across
  child executions; trace continuity across child boundaries
  is its own challenge. Slice 2+.
- **Logic Apps connector trace pollution.** Each Logic Apps
  connector (HTTP / SQL / SharePoint / etc.) has its own trace
  emission behavior; some emit, some don't. Slice 1 reports
  the workflow's top-level trace axis; per-connector quality
  is slice 2+.
- **GCP Workflows callback patterns** that span longer than
  a 24h window. Slice 1 uses the same 24h "last seen"
  threshold as serverless; long-running callbacks are slice 2+.
- **EventBridge / Cloud Tasks / Service Bus event sources.**
  These are the event source tier — feed orchestrations and
  Lambda but conceptually different. Separate arc.
- **Auto-fix.** Squadron remains a recommender. Slice 1
  surfaces gaps + drafts PRs.

## 3. Per-cloud detection surfaces

Three orchestration surfaces total.

### 3.1 AWS Step Functions

API: `states:ListStateMachines`, `states:DescribeStateMachine`.
Required IAM in the slice 1 trust policy update:
`states:ListStateMachines`, `states:DescribeStateMachine`.

Detection axes:

| Axis                | Source                                       | Recommendation kind         |
|---------------------|----------------------------------------------|-----------------------------|
| X-Ray active tracing | `tracingConfiguration.enabled == true`      | `stepfunc-xray-active`      |
| Log destination     | `loggingConfiguration.level != "OFF"`        | `stepfunc-logging-enable`   |
| State machine type  | `type` is "STANDARD" OR "EXPRESS"            | informational only          |

Coverage caveat: EXPRESS state machines emit logs to CloudWatch
Logs, not X-Ray segments. The X-Ray axis only meaningfully
applies to STANDARD type. Slice 1 surfaces this in the
recommendation reasoning ("EXPRESS state machine detected;
X-Ray tracing applies to per-state Lambda invocations not
the orchestration itself"); slice 2 may add a per-type
recommendation kind set.

### 3.2 GCP Workflows

API: `workflows.googleapis.com/v1/projects/*/locations/*/workflows`.
Required GCP permission: `workflows.workflows.list`,
`workflows.workflows.get`.

Detection axes:

| Axis                       | Source                                       | Recommendation kind             |
|----------------------------|----------------------------------------------|---------------------------------|
| Cloud Trace integration    | workflow.callLogLevel = "LOG_ALL_CALLS" (proxy for trace emission) | `workflows-trace-enable` |
| Cloud Logging level        | workflow.callLogLevel != "CALL_LOG_LEVEL_UNSPECIFIED" | `workflows-logging-enable` |

GCP Workflows doesn't have a separate "trace" toggle — log
level acts as the trace primitive surface. Slice 1's detection
treats `LOG_ALL_CALLS` as a soft proxy for trace emission;
slice 2 may refine when GCP exposes a more granular trace
toggle.

### 3.3 Azure Logic Apps

API: `Microsoft.Logic/workflows` (Standard tier) and
`Microsoft.Web/sites?$filter=kind eq 'workflowapp'` (Consumption
tier hosted on App Service). Required Azure RBAC: existing
Reader role on the resource group covers both.

Detection axes:

| Axis                              | Source                                          | Recommendation kind            |
|-----------------------------------|-------------------------------------------------|--------------------------------|
| Application Insights enabled      | `app_settings[APPLICATIONINSIGHTS_CONNECTION_STRING]` OR `properties.diagnosticSettings → AppInsights` | `logicapps-appinsights-enable` |
| Diagnostic settings to Log Analytics | resource has a Microsoft.Insights/diagnosticSettings child  | `logicapps-diagnostics-enable` |

Logic Apps Consumption and Standard tiers have different
detection surfaces. Standard (the newer one, hosted on App
Service) carries the app_settings axis the same way Functions
do (and v0.89.91 chunk 3 ships the Functions scanner pattern
to mirror). Consumption (the older one, managed) reports via
the Insights diagnostic settings path.

## 4. Storage schema

New `orchestration_instance` storage table. Schema migration
v11 → v12.

```sql
CREATE TABLE IF NOT EXISTS orchestration_instance (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    scan_id TEXT NOT NULL,
    provider TEXT NOT NULL, -- "aws" / "gcp" / "azure"
    surface TEXT NOT NULL, -- "stepfunc" / "workflows" / "logicapps"
    account_id TEXT NOT NULL,
    region TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    resource_arn TEXT,
    workflow_type TEXT, -- STANDARD/EXPRESS for stepfunc, Standard/Consumption for logicapps
    has_trace_axis INTEGER NOT NULL,
    has_log_axis INTEGER NOT NULL,
    last_seen_at TIMESTAMP,
    snapshot_json TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (connection_id, scan_id, resource_arn)
);
CREATE INDEX IF NOT EXISTS idx_orchestration_scan ON orchestration_instance(scan_id);
CREATE INDEX IF NOT EXISTS idx_orchestration_conn ON orchestration_instance(connection_id);
```

Schema version bumps to v12. Migration is idempotent.

## 5. Scanner contract

Each per-cloud scanner gains `ScanOrchestrations(ctx, scope)`.
The OrchestrationInstanceSnapshot struct lives in
`internal/discovery/scanner/scanner.go`:

```go
type OrchestrationInstanceSnapshot struct {
    Provider      string         `json:"provider"`
    Surface       string         `json:"surface"`   // stepfunc / workflows / logicapps
    AccountID     string         `json:"account_id"`
    Region        string         `json:"region"`
    ResourceName  string         `json:"resource_name"`
    ResourceARN   string         `json:"resource_arn"`
    WorkflowType  string         `json:"workflow_type,omitempty"` // STANDARD/EXPRESS etc.
    HasTraceAxis  bool           `json:"has_trace_axis"`
    HasLogAxis    bool           `json:"has_log_axis"`
    LastSeenAt    *time.Time     `json:"last_seen_at,omitempty"`
    Detail        map[string]any `json:"detail,omitempty"`
}
```

Per-cloud scanner files:
- `internal/discovery/aws/stepfunctions.go`
- `internal/discovery/gcp/workflows.go`
- `internal/discovery/azure/logicapps.go`

OCI scanner stays at the existing 4-tier surface for slice 1;
it returns `nil, nil` for ScanOrchestrations until slice 2.

## 6. API surface

### 6.1 Per-provider scan endpoint extension

`POST /api/v1/discovery/{provider}/scan` accepts
`"orchestration"` as a valid tier value. Default tier list for
AWS/GCP/Azure extends from
`[compute, database, kubernetes, serverless]` to
`[compute, database, kubernetes, serverless, orchestration]`.
OCI default tier list stays at the existing 4 tiers.

### 6.2 Per-provider inventory endpoint extension

Response shape gains `orchestrations: []` field (empty for OCI
until slice 2):

```json
{
  "compute": [...],
  "databases": [...],
  "clusters": [...],
  "serverless": [...],
  "orchestrations": [...]
}
```

### 6.3 Discovery summary endpoint extension

`GET /api/v1/discovery/summary` per-provider response gains
`orchestration_count` field.

### 6.4 Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` per-provider response
gains `orchestration_pct` field — % of orchestrations emitting
in 24h. For OCI it's always nil / 0 until slice 2.

## 7. UI

Each per-provider page (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure) gains a fifth Inventory sub-tab:

> [ Compute ]  [ Databases ]  [ Kubernetes ]  [ Serverless ]  [ Orchestration ]

DiscoveryOCI stays at 4 sub-tabs until slice 2 (the
Orchestration tab is conditionally hidden when the
orchestrations[] field is empty).

Orchestration sub-tab columns:
- Resource Name
- Surface (stepfunc / workflows / logicapps)
- Type (STANDARD / EXPRESS / Standard / Consumption)
- Region
- Trace axis (✓ / ✗)
- Log axis (✓ / ✗)
- Last seen (relative time)
- Quality dot (AWS only in slice 1 per v0.89.92 pattern)

The Discovery dashboard TRACE COVERAGE panel chip breakdown
adds an "ORCH" column:

```
COMPUTE 67% | DB 42% | K8S 89% | SERVERLESS 33% | ORCH 12%
```

The chip line hides when zero across all providers (same
behavior as the serverless line).

## 8. Recommendation kinds

5 new kinds across 3 surfaces:

```
stepfunc-xray-active            workflows-trace-enable          logicapps-appinsights-enable
stepfunc-logging-enable         workflows-logging-enable        logicapps-diagnostics-enable
```

Webhook kind-prefix routing extends:
- `stepfunc-` → AWS
- `workflows-` → GCP
- `logicapps-` → Azure

The 3 new prefixes extend the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

## 9. Slice 1 contract

**In:**

1. Storage schema v11 → v12 with `orchestration_instance` table.
2. OrchestrationInstanceSnapshot struct on scanner package.
3. ScanOrchestrations methods on the 3 provider scanners
   (AWS/GCP/Azure); OCI returns empty.
4. Per-provider scan + inventory endpoint extensions.
5. Discovery summary + trace_coverage endpoint extensions.
6. Per-provider page Orchestration sub-tab (3 pages; OCI
   conditional hide).
7. Dashboard TRACE COVERAGE chip breakdown gains ORCH column.
8. Proposer prompt extension with 5 new recommendation kinds.
9. Webhook routing for the 3 new kind prefixes.
10. Operator runbook covering all the above.
11. Acceptance tests covering all 3 surfaces' scanner
    detection, endpoint shape, UI rendering, cold-start parity.

**Out:**

- OCI orchestration coverage (slice 2).
- Per-state-transition trace propagation analysis.
- Workflow definition introspection.
- Distributed Map / Logic Apps connector trace pollution.
- Long-running GCP Workflows callback handling.
- Event source tier (separate arc).
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation + AWS Step Functions scanner.**
  OrchestrationInstanceSnapshot struct, storage migration
  v11→v12, AWS Step Functions scanner (simplest of the 3),
  scan endpoint tier extension. ~900-1100 lines.
  **v0.89.95.**
- **Chunk 2: GCP Workflows scanner.** Parallel-eligible with
  chunk 3. ~500-700 lines. **v0.89.96.**
- **Chunk 3: Azure Logic Apps scanner.** Parallel-eligible.
  Covers both Standard and Consumption tiers. ~600-800 lines.
  **v0.89.96 (same release as chunk 2 via parallel merge if
  both clean).**
- **Chunk 4: Proposer prompt + UI Orchestration sub-tab +
  webhook routing.** ~900-1100 lines. **v0.89.97.**
- **Chunk 5: Operator runbook + dashboard chip extension +
  README index entry.** ~400-500 lines. **v0.89.98.**

Total: 4 release tags via parallel scanner fan-out.

## 11. Acceptance tests

1. **AWS Step Functions scanner — STANDARD machine with
   X-Ray enabled.** Mock states:DescribeStateMachine to return
   `tracingConfiguration.enabled = true`, `type = "STANDARD"`.
   Assert: snapshot has HasTraceAxis=true, WorkflowType="STANDARD".
2. **AWS Step Functions scanner — STANDARD machine without
   X-Ray.** Assert: HasTraceAxis=false.
3. **AWS Step Functions scanner — EXPRESS machine.** Assert:
   snapshot has WorkflowType="EXPRESS"; HasTraceAxis reflects
   tracingConfiguration as-is (not auto-suppressed).
4. **AWS Step Functions scanner — logging level not OFF.**
   Assert: HasLogAxis=true.
5. **GCP Workflows scanner — workflow with LOG_ALL_CALLS.**
   Assert: HasTraceAxis=true, HasLogAxis=true.
6. **GCP Workflows scanner — workflow with CALL_LOG_LEVEL_UNSPECIFIED.**
   Assert: HasTraceAxis=false, HasLogAxis=false.
7. **Azure Logic Apps — Standard tier with APPLICATIONINSIGHTS_CONNECTION_STRING.**
   Assert: HasTraceAxis=true, WorkflowType="Standard".
8. **Azure Logic Apps — Consumption tier with diagnostic settings.**
   Assert: HasTraceAxis=true via Insights path,
   WorkflowType="Consumption".
9. **Azure Logic Apps — neither tier with anything.**
   Assert: HasTraceAxis=false, HasLogAxis=false.
10. **Storage migration v11→v12 idempotent.** Run twice; no
    error, table exists.
11. **Discovery summary includes orchestration_count.** Seed
    3 orchestrations; assert per-provider count=3.
12. **Trace coverage includes orchestration_pct.** 2 orchs,
    1 emitting; assert orchestration_pct=50.
13. **Webhook routes stepfunc-xray-active to aws.**
14. **Webhook routes workflows-trace-enable to gcp.**
15. **Webhook routes logicapps-appinsights-enable to azure.**
16. **Cold-start parity preserved.** All 4 providers cold-start
    prompts byte-identical to v0.89.93 when no orchestration
    rows trigger recommendations.
17. **OCI inventory orchestrations field is empty in slice 1.**
    Scan OCI; assert response shape includes
    `orchestrations: []` but never populated.

## 12. Threat model

**Wider API permission requests.** Each cloud's slice 1 IAM
template grows to cover the new orchestration API calls. For
AWS: `states:ListStateMachines`,
`states:DescribeStateMachine`. Both read-only. Operators get
the in-product IAM upgrade path (#590).

**Step Functions EXPRESS coverage caveat.** Slice 1 detects
`tracingConfiguration.enabled` uniformly, but EXPRESS
machines don't emit X-Ray segments for their orchestration
runtime — only for the per-state Lambda invocations. A
`stepfunc-xray-active` recommendation on an EXPRESS machine
is technically valid (it enables X-Ray for the Lambdas) but
won't show in X-Ray as a state-machine span. The runbook
documents this; the verdict learning loop records declines
on EXPRESS-specific cases.

**GCP Workflows trace-axis softness.** Slice 1 treats
`callLogLevel = LOG_ALL_CALLS` as a soft proxy for trace
emission. GCP may expose a more granular trace toggle in
future API versions; until then the proxy is the best
available signal. False positives possible; the runbook
documents and the verdict learning loop handles.

**Logic Apps two-tier confusion.** Standard tier on App
Service shares the `azfunc-*` detection surface
(app_settings). Consumption tier uses Insights diagnostic
settings. An operator with mixed tiers may see different
recommendation kinds for what looks like the same workflow;
the runbook clarifies the per-tier distinction.

**Logic Apps connector trace pollution.** Connectors
(HTTP / SQL / SharePoint / etc.) can emit spans that don't
include the parent workflow's traceparent. Slice 1 doesn't
inspect connector-level spans; the workflow's top-level
trace axis is the unit of detection. Slice 2 may add
per-connector quality detection.

## 13. Slice 2 candidates

- OCI orchestration coverage (Resource Manager + Process
  Automation).
- Per-state-transition trace propagation analysis.
- Workflow definition introspection.
- Step Functions Distributed Map state analysis.
- Logic Apps connector trace pollution detection.
- Long-running GCP Workflows callback handling.
- Per-execution last-seen-at (instead of per-workflow).
- Per-state Lambda invocation correlation back to parent
  state-machine span.

---

**Strategic frame:**

Squadron's universal claim grows from four tiers to five:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, AND ORCHESTRATION
> for observability gaps, verifies telemetry is actually
> flowing, validates the spans Squadron receives are healthy,
> AND drafts the IaC PRs that close the gaps it finds.

Four clouds. Five tiers. Four verbs. One control plane. The
honest framing: slice 1 covers orchestration on AWS / GCP /
Azure but NOT OCI — OCI's primitives are shape-different and
deserve their own analysis. The four-cloud claim still holds
for the prior four tiers; the five-tier claim qualifies as
three-cloud at this slice and grows to full coverage in slice
2.

Orchestration is where business logic gets sequenced. The
Tuesday LinkedIn drumbeat narrative gains another concrete
answer to the operator's "where did the trace go" question:
"your Step Function state 7 dropped the propagation header."
Slice 1 ships the visibility (last seen per orchestration);
slice 2 will ship per-state diagnosis.
