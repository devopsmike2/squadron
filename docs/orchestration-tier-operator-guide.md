# Orchestration tier — operator guide

This is the operator-facing runbook for the v0.89.94 through
v0.89.98 orchestration tier slice 1 arc. Squadron now scans
three orchestration surfaces across three clouds — AWS Step
Functions, GCP Workflows, Azure Logic Apps — for the
observability primitives operators sequence business logic
through.

The strategic frame: Squadron previously covered four tiers
(compute / database / kubernetes / serverless) across four
clouds. Orchestration is the fifth tier — the layer where
state transitions happen and trace context propagation breaks
most often. A Step Function with 12 states where state 7
silently swallows the traceparent header is a real production
failure mode; operators see "the orchestration ran successfully"
in the cloud console but Squadron's traceindex sees nothing
past state 6.

For a first test, the walkthrough takes about 25 minutes —
most of it spent confirming your cloud connections have the
additional read permissions for the orchestration APIs.

## What this is good for

- A team running Step Functions for event orchestration and
  wanting to confirm X-Ray active tracing is actually on
  across all state machines.
- A GCP Workflows deployment with mixed `callLogLevel` settings
  where some workflows trace fully and others don't.
- An Azure Logic Apps deployment with a mix of Standard tier
  (App Service hosted) and Consumption tier workflows where
  observability adoption is uneven between tiers.
- An auditor who needs to confirm "every Step Function in
  production has tracing on" and wants a one-pane view across
  all accounts and regions.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of orchestration tier is
intentionally narrow:

- **OCI orchestration is NOT covered.** OCI's primitives
  (Resource Manager, Process Automation) are shape-different
  from the AWS/GCP/Azure trio. Resource Manager is
  Terraform-as-a-service (infrastructure orchestration);
  Process Automation targets business processes (BPMN).
  Neither maps cleanly to the Step Functions / Workflows /
  Logic Apps pattern. Slice 2 covers OCI with its own
  analysis.
- **Per-state-transition trace propagation analysis is slice
  2+.** Detecting "state 7 dropped the traceparent header"
  is the high-leverage detection but requires correlating
  per-state span emissions with the orchestration definition.
  Slice 1 only checks the top-level trace axis.
- **Workflow definition introspection is slice 2+.** Slice 1
  reads control-plane metadata (whether tracing is on, what
  type, current state). It does NOT parse the workflow
  definition JSON / YAML / ASL to detect propagation hops.
- **Step Functions Distributed Map state analysis is slice
  2+.** Distributed Map fans out across child executions;
  trace continuity across child boundaries is its own
  challenge.
- **Logic Apps connector trace pollution is slice 2+.** Each
  Logic Apps connector (HTTP / SQL / SharePoint / etc.) has
  its own trace emission behavior. Slice 1 reports the
  workflow's top-level trace axis only.
- **Long-running GCP Workflows callback handling.** Slice 1
  uses the same 24h "last seen" threshold as the other tiers;
  workflows with callbacks longer than 24h get false
  "no recent emission" signal. Slice 2+.
- **Auto-fix.** Squadron remains a recommender. Slice 1
  surfaces gaps + drafts PRs.

## The three orchestration surfaces

| Cloud | Surface         | Trace axis                                       | Log axis                                     |
|-------|-----------------|--------------------------------------------------|----------------------------------------------|
| AWS   | Step Functions  | `tracingConfiguration.enabled == true`           | `loggingConfiguration.level != "OFF"`        |
| GCP   | Workflows       | `callLogLevel == "LOG_ALL_CALLS"` (soft proxy)   | `callLogLevel != "CALL_LOG_LEVEL_UNSPECIFIED"` |
| Azure | Logic Apps Standard | `app_settings[APPLICATIONINSIGHTS_CONNECTION_STRING]` | (implicit via App Service logging)       |
| Azure | Logic Apps Consumption | Diagnostic settings → Application Insights or workspace | Diagnostic settings → any logs/metrics |

Each surface contributes one row per discovered orchestration
to the new Orchestration Inventory sub-tab on the per-provider
Discovery page.

## The Step Functions EXPRESS coverage caveat

This catches first-time operators. AWS Step Functions has two
type values: STANDARD and EXPRESS.

- **STANDARD** state machines emit X-Ray segments for the
  orchestration runtime itself when `tracingConfiguration.enabled = true`.
- **EXPRESS** state machines do NOT emit X-Ray segments for
  the orchestration runtime — only for per-state Lambda
  invocations the state machine triggers. The
  `tracingConfiguration.enabled` flag still applies (it enables
  X-Ray for those per-state Lambda invocations), but you won't
  see a parent "state-machine execution" span in X-Ray.

The Squadron scanner detects the type and records it in the
`workflow_type` field. A `stepfunc-xray-active` recommendation
on an EXPRESS state machine is technically valid (it enables
per-state Lambda tracing) but the operator should expect a
different X-Ray surface than they'd see for STANDARD. The
recommendation's Reasoning text calls this out; decline the PR
if you've made the deliberate choice not to trace
per-invocation Lambda calls from EXPRESS workflows.

## The GCP Workflows trace-axis softness

GCP Workflows doesn't expose a separate "trace" toggle distinct
from logging. Squadron treats `callLogLevel = LOG_ALL_CALLS` as
a soft proxy for trace emission. If GCP exposes a more
granular trace toggle in future API versions, slice 2 will
refine.

In practice this means: a workflow set to `LOG_ERRORS_ONLY`
gets `has_trace_axis = false` from Squadron even though some
trace data may flow on error. The
`workflows-trace-enable` recommendation drafts a PR setting
`call_log_level = "LOG_ALL_CALLS"`. If your team has
deliberately set LOG_ERRORS_ONLY for cost reasons, decline
the recommendation; the verdict learning loop records.

## The Azure Logic Apps two-tier distinction

Logic Apps comes in two tiers with different detection
surfaces:

- **Standard tier** is hosted on Azure App Service. It carries
  the same `app_settings` surface as Functions (v0.89.91 chunk
  3). Squadron's Logic Apps scanner reuses the
  Microsoft.Web/sites + app_settings/list flow already wired
  for Functions, filtered by `kind eq 'workflowapp'`.
  The `workflow_type` field reports "Standard".

- **Consumption tier** is fully managed. Detection happens via
  `Microsoft.Logic/workflows` + the resource's
  `Microsoft.Insights/diagnosticSettings` child. Squadron
  considers the workflow to have a trace axis if ANY diagnostic
  setting routes to either an Application Insights resource
  OR a Log Analytics workspace.
  The `workflow_type` field reports "Consumption".

A team with mixed tiers may see different recommendation
kinds for what looks like the same workflow:

- Standard tier missing the trace axis →
  `logicapps-appinsights-enable` (app_setting fix).
- Consumption tier missing the trace axis →
  `logicapps-diagnostics-enable` (diagnostic setting fix).

Both flow through the same Recommendations tab; the operator
reads the recommendation's `workflow_type` field and picks
the appropriate fix path.

## The 6 new recommendation kinds

```
stepfunc-xray-active            workflows-trace-enable          logicapps-appinsights-enable
stepfunc-logging-enable         workflows-logging-enable        logicapps-diagnostics-enable
```

## Per-cloud Terraform patterns

### AWS Step Functions

- **`stepfunc-xray-active`** — `aws_sfn_state_machine tracing_configuration { enabled = true }`
- **`stepfunc-logging-enable`** — `aws_sfn_state_machine logging_configuration { level = "ALL" log_destination = aws_cloudwatch_log_group.sfn.arn }` (requires an existing log group resource; the PR body includes the dependency note)

### GCP Workflows

- **`workflows-trace-enable`** — `google_workflows_workflow call_log_level = "LOG_ALL_CALLS"`
- **`workflows-logging-enable`** — same block with `call_log_level = "LOG_ERRORS_ONLY"` minimum

### Azure Logic Apps

- **`logicapps-appinsights-enable`** (Standard tier) —
  `azurerm_logic_app_standard app_settings = { APPLICATIONINSIGHTS_CONNECTION_STRING = "..." }`
- **`logicapps-diagnostics-enable`** (Consumption tier) —
  `azurerm_monitor_diagnostic_setting` resource attached to
  the `azurerm_logic_app_workflow`, routing to an existing
  Application Insights resource OR Log Analytics workspace.

## The Orchestration Inventory sub-tab

Each per-provider Discovery page (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure) now has a fifth Inventory sub-tab:

> [ Compute ]  [ Databases ]  [ Kubernetes ]  [ Serverless ]  [ Orchestration ]

DiscoveryOCI hides the Orchestration sub-tab when the
`orchestrations[]` field is empty. After slice 2 ships OCI
coverage, the OCI page will render the tab on the same
conditional.

The Orchestration table shows:

| Column        | Source                                |
|---------------|---------------------------------------|
| Resource Name | state machine name / workflow name    |
| Surface       | stepfunc / workflows / logicapps      |
| Type          | STANDARD / EXPRESS / Standard / Consumption |
| Region        | resource region                       |
| Trace axis    | ✓ if `has_trace_axis` else ✗          |
| Log axis      | ✓ if `has_log_axis` else ✗            |
| Last seen     | relative time (per v0.89.77)          |
| Quality       | dot indicator (AWS only; per v0.89.92 pattern) |

QualityDot ships on AWS only in slice 1 — GCP/Azure pages
don't render the dot elsewhere yet, so adding it on
Orchestration alone would be inconsistent. Slice 2 unifies.

## Dashboard surfaces

### Discovery summary endpoint extension

`GET /api/v1/discovery/summary` per-provider response gains
`orchestration_count`. For OCI the count is always 0 in slice
1.

### Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` per-provider response
gains `orchestration_pct` — % of inventoried orchestrations
emitting a span within 24h. OCI returns 0.

The Discovery dashboard TRACE COVERAGE chip breakdown adds an
ORCH column:

```
COMPUTE 67% | DB 42% | K8S 89% | SERVERLESS 33% | ORCH 12%
```

When `orchestration_pct` is zero across all 4 providers, the
ORCH column hides — same pattern as the SERVERLESS column
behavior the chunk-4 ships alongside.

## Webhook routing

The kind-prefix detection in the IaC webhook handler extends
with 3 new cases:

```
stepfunc-   → aws
workflows-  → gcp
logicapps-  → azure
```

All 6 new recommendation kinds route to the correct provider's
audit scope. SIEM consumers can filter on:

```
recommendation_kind ~= "^(stepfunc-|workflows-|logicapps-)"
```

## Workflow — first orchestration scan

1. Open the per-provider Discovery page (e.g.
   `/discovery/aws`). Note your existing AWS connection.
2. If the connection was created before v0.89.95, you may
   need to upgrade the IAM policy to include
   `states:ListStateMachines` and `states:DescribeStateMachine`.
   The in-product IAM upgrade path (#590) shows the diff.
3. Click "Run scan" — the default tier list now includes
   `orchestration`. The scan walks Step Functions in addition
   to the existing four tiers.
4. Click the Orchestration Inventory sub-tab. Each state
   machine shows the two axes + workflow type + Last seen.
5. Click the Recommendations tab. Any state machine missing
   an axis fires the corresponding `stepfunc-*` recommendation.
6. Review the Terraform PR. For STANDARD machines the PR is
   straightforward. For EXPRESS machines the Reasoning text
   notes the per-state Lambda caveat — decide whether the
   semantics match your intent.
7. After merge + apply + first execution, wait ~5 minutes.
   Re-load the Orchestration sub-tab; the Last seen column
   populates.

## Reading the audit

Slice 1 reuses the existing audit event types — no new
constants. The discovery scan emits the existing
`discovery.{provider}.scan_completed` event with the
`orchestration_count` field included in the payload.

The recommendation lifecycle (`recommendation.created`,
`recommendation.pr_opened`, `recommendation.pr_merged`,
`recommendation.pr_closed`) carries the new kind values.

## Troubleshooting

- **Step Functions don't appear in the Orchestration sub-tab.**
  Check the IAM policy — `states:ListStateMachines` and
  `states:DescribeStateMachine` are required. If the policy
  is correct but state machines still don't appear, check
  the scan audit for `partial_reason` — the Describe call may
  have been rate-limited.
- **A STANDARD state machine has X-Ray on but shows
  `last_seen_at = null`.** The OTel collector receiving spans
  on Squadron's port 4318 may not be configured to forward
  the X-Ray-converted spans. Squadron's traceindex only
  counts OTel spans, not native X-Ray segments. Verify the
  ADOT collector configuration on whatever is processing
  X-Ray segments forwards to Squadron.
- **An EXPRESS state machine with X-Ray on shows
  `has_trace_axis = true` and `last_seen_at = null` and no
  recommendations.** This is the expected EXPRESS coverage
  caveat. The X-Ray tracing applies to per-state Lambda
  invocations; check each Lambda's individual Last seen in
  the Serverless sub-tab.
- **A GCP Workflow with LOG_ERRORS_ONLY shows
  `has_trace_axis = false`.** This is the soft-proxy
  detection. If you've deliberately chosen LOG_ERRORS_ONLY,
  decline the `workflows-trace-enable` recommendation. The
  verdict learning loop records.
- **A Logic Apps Standard tier workflow has App Insights set
  but shows `has_trace_axis = false`.** App settings sometimes
  don't read back immediately after a config change. Re-scan
  after waiting 60s. If the issue persists, check that the
  connection string is in `app_settings` and not in
  `connection_strings` (the latter is a different Azure surface).
- **A Logic Apps Consumption tier workflow has Diagnostic
  Settings configured but shows `has_trace_axis = false`.**
  Squadron requires the diagnostic settings route to EITHER
  an Application Insights resource OR a Log Analytics
  workspace. If your diagnostic settings route to neither
  (e.g. event hub or storage account only), Squadron doesn't
  count it as trace-enabled in slice 1. Slice 2 may widen.
- **OCI Discovery page doesn't show the Orchestration
  sub-tab.** This is correct slice 1 behavior. OCI
  orchestration ships in slice 2.

## What slice 2 will add

Per §13 of the design doc:

- OCI orchestration coverage (Resource Manager + Process
  Automation).
- Per-state-transition trace propagation analysis.
- Workflow definition introspection.
- Step Functions Distributed Map state analysis.
- Logic Apps connector trace pollution detection.
- Long-running GCP Workflows callback handling.
- Per-execution last-seen-at (instead of per-workflow).
- Per-state Lambda invocation correlation back to the parent
  state-machine span.
- QualityDot column on GCP / Azure inventory tabs.

## The universal claim grows a fifth tier (qualified)

After orchestration slice 1, Squadron's positioning reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, AND
> ORCHESTRATION for observability gaps, verifies telemetry
> is actually flowing, validates the spans Squadron receives
> are healthy, AND drafts the IaC PRs that close the gaps
> it finds.

Four clouds. Five tiers. Four verbs. One control plane.
Twenty scanner surfaces (4 clouds × 4 prior tiers + 3 new
orchestration surfaces; OCI orchestration deferred to slice
2 makes the 5th tier honestly 3-cloud at this slice).

Orchestration is where business logic gets sequenced — the
trace integration arc + span quality arc together close the
loop: Squadron sees the orchestrations your cloud control
plane lists, checks whether spans arrive from each, and
drafts the PR that enables the missing primitive.

## Cross-references

- [Orchestration tier slice 1 design doc](./proposals/orchestration-tier-slice1.md) —
  the locked spec this runbook operationalizes.
- [Serverless tier slice 1](./proposals/serverless-tier-slice1.md) —
  the prior tier-expansion arc this mirrors structurally.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the trace integration arc this composes with.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  the span quality arc that validates the spans Squadron
  receives.
- [Audit log](./audit-log.md) — full catalog of event types.
