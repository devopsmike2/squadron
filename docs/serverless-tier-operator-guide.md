# Serverless tier — operator guide

This is the operator-facing runbook for the v0.89.89 through
v0.89.93 serverless tier slice 1 arc. Squadron now scans five
serverless surfaces across all four clouds — AWS Lambda, GCP
Cloud Run, GCP Cloud Functions, Azure Functions, OCI Functions
— for the observability primitives the trace integration arc
already verifies + the span quality arc already validates.

The strategic frame: Squadron previously covered three tiers
(compute / database / kubernetes) across four clouds. Serverless
is the fourth tier — the canonical "where did my trace go?"
surface where ephemeral execution makes trace-emission
guarantees most fragile. Squadron now reads the cloud
control plane's serverless surface, detects which functions /
services have observability enabled, and surfaces the
last-seen-span signal per-function for reconciliation.

For a first test, the walkthrough takes about 20 minutes
total — most of it spent confirming your cloud connections
have the additional read permissions for the serverless API.

## What this is good for

- A team running Lambda heavily for event handlers and wanting
  to confirm every function actually emits OTel spans (X-Ray
  alone doesn't satisfy Squadron — see §3 of the design doc).
- A Cloud Run / Cloud Functions deployment with mixed OTel
  adoption: some services have the sidecar, some don't.
  Squadron lists both populations and flags the gaps.
- An Azure Functions deployment where App Insights coverage
  is uneven because the OTEL_DOTNET_AUTO_HOME setting was
  only ever applied to the .NET 8 functions and the older
  ones still emit nothing.
- An OCI Functions deployment where the team enabled
  OCI_APM_ENABLED on most but missed a handful.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of serverless tier is
intentionally narrow:

- **Squadron does NOT analyze cold-start latency.** Detecting
  "your Lambda cold-start is 4s" is a metric-correlation
  problem, not a discovery problem. Slice 2 candidate.
- **Squadron does NOT inspect function code.** Detection is
  metadata-only: control-plane API responses, config maps,
  app_settings, environment variables. No zip downloads, no
  code parsing.
- **Squadron does NOT analyze concurrent execution.**
  Misconfigured concurrency mixing spans across requests is
  real but requires span-content inspection that doesn't
  fit slice 1's read-only discovery posture.
- **Knative on bare-metal, Lambda on OCI** — non-native
  serverless deployments are slice 3+. Slice 1 scans what
  each cloud's native control plane lists as serverless.
- **Step Functions / Workflows / Logic Apps / OCI Resource
  Manager** — orchestration tier. Slice 2+.
- **EventBridge / Cloud Tasks / Azure Service Bus / OCI
  Streams** — event source tier. Slice 2+.
- **Per-language SDK depth.** Slice 1 ships the cloud-native
  generic auto-instrumentation paths. Per-language deep
  customization (Python asyncio, Node.js async_hooks, JVM
  agent variants) is slice 2.
- **Auto-fix.** Slice 1 surfaces gaps + drafts PRs. Squadron
  remains a recommender.

## The five serverless surfaces

| Cloud | Surface       | Trace axis                            | OTel distro axis                           |
|-------|---------------|---------------------------------------|--------------------------------------------|
| AWS   | Lambda        | `tracing_config.mode == "Active"`     | ADOT layer ARN OR `AWS_LAMBDA_EXEC_WRAPPER` env |
| GCP   | Cloud Run     | `run.googleapis.com/trace` annotation | OTel sidecar container OR `OTEL_EXPORTER_OTLP_ENDPOINT` env |
| GCP   | Cloud Functions | `GOOGLE_CLOUD_TRACE` env var       | `OTEL_INSTRUMENTATION_AUTO_ENABLED` env var |
| Azure | Functions     | `APPLICATIONINSIGHTS_CONNECTION_STRING` app_setting | `OTEL_DOTNET_AUTO_HOME` OR `OTEL_PYTHON_DISTRO` |
| OCI   | Functions     | `config[OCI_APM_ENABLED] == "true"`   | `config[OTEL_DISTRO]` set                  |

Each surface contributes one row per discovered function /
service to the new Serverless Inventory sub-tab on the
per-provider Discovery page.

## The X-Ray-without-ADOT caveat

This catches first-time operators. A Lambda function with
`tracing_config.mode = "Active"` shows X-Ray segments in the
AWS X-Ray console — operators sometimes assume that satisfies
"telemetry is flowing." It does not satisfy Squadron's
traceindex.

Why: X-Ray is AWS's proprietary trace format. It does NOT
emit OTel spans. Squadron's traceindex only counts spans that
arrive over the OTLP receiver (port 4318 HTTP / 4317 gRPC).
A function with X-Ray active but no ADOT layer will have:

- `has_trace_axis = true` (the X-Ray axis is the AWS-native
  trace primitive)
- `has_otel_distro = false`
- `last_seen_at = nil` (no OTel spans arriving)

Squadron will draft a `lambda-otel-layer` recommendation for
this function. The Terraform PR adds the ADOT layer to the
function's layers list. After merge + apply + first
invocation, `last_seen_at` populates and the row's Quality
dot turns green or yellow depending on span content.

The runbook for trace coverage (v0.89.78) covers the
last_seen_at column in depth.

## The 11 new recommendation kinds

Following the pattern from prior tier arcs, 11 kinds across
the 5 surfaces:

```
lambda-xray-active            cloudrun-trace-enable          azfunc-appinsights-enable
lambda-otel-layer             cloudrun-otel-sidecar          azfunc-otel-distro
lambda-otel-wrapper           cloudrun-otel-export-endpoint  ocifunc-apm-enable
                              cloudfunc-trace-enable         ocifunc-otel-distro
                              cloudfunc-otel-layer
```

Each kind targets ONE Terraform pattern. The proposer drafts
the PR; the operator reviews and merges or declines.

## Per-cloud Terraform patterns

### AWS Lambda

- **`lambda-xray-active`** — `aws_lambda_function tracing_config { mode = "Active" }`
- **`lambda-otel-layer`** — `aws_lambda_function layers = [...existing, "arn:aws:lambda:<region>:901920570463:layer:aws-otel-{lang}-{ver}"]`
- **`lambda-otel-wrapper`** — `aws_lambda_function environment { variables { AWS_LAMBDA_EXEC_WRAPPER = "/opt/otel-instrument" } }`

The ADOT layer ARN format embeds region (e.g.
`arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-python-amd64-ver-1-25-0:1`).
The proposer's reasoning notes the layer published by AWS for
the function's runtime; the operator updates the ARN to the
latest available version on review.

### GCP Cloud Run

- **`cloudrun-trace-enable`** — `google_cloud_run_service metadata { annotations = { "run.googleapis.com/trace" = "true" } }`
- **`cloudrun-otel-sidecar`** — adds a containers block with
  name = "otel-collector" pointing at the upstream collector
  image.
- **`cloudrun-otel-export-endpoint`** — adds
  `env { name = "OTEL_EXPORTER_OTLP_ENDPOINT" value = "http://localhost:4318" }`
  on the user's container pointing at the sidecar.

### GCP Cloud Functions

- **`cloudfunc-trace-enable`** — `google_cloudfunctions_function environment_variables { GOOGLE_CLOUD_TRACE = "true" }`
- **`cloudfunc-otel-layer`** — same block adding
  `OTEL_INSTRUMENTATION_AUTO_ENABLED = "true"`.

The supported runtimes for Cloud Functions OTel auto-instrumentation:
`python310`+, `nodejs18`+, `java17`+, `go121`+. Functions on
older runtimes get a `cloudfunc-trace-enable` recommendation
but NOT a `cloudfunc-otel-layer` — the proposer respects the
runtime constraint.

### Azure Functions

- **`azfunc-appinsights-enable`** —
  `azurerm_linux_function_app app_settings = { APPLICATIONINSIGHTS_CONNECTION_STRING = "..." }`
- **`azfunc-otel-distro`** — same app_settings block adding
  `OTEL_DOTNET_AUTO_HOME` for .NET function apps OR
  `OTEL_PYTHON_DISTRO` for Python function apps. The proposer
  picks based on the function's runtime.

### OCI Functions

- **`ocifunc-apm-enable`** — `oci_functions_function config = { OCI_APM_ENABLED = "true" }`
- **`ocifunc-otel-distro`** — same config block adding
  `OTEL_DISTRO = "auto"`.

## The Serverless Inventory sub-tab

Each per-provider Discovery page (DiscoveryAWS, DiscoveryGCP,
DiscoveryAzure, DiscoveryOCI) now has a fourth Inventory
sub-tab alongside Compute / Databases / Kubernetes:

> [ Compute ]  [ Databases ]  [ Kubernetes ]  [ Serverless ]

The Serverless table shows:

| Column        | Source                                |
|---------------|---------------------------------------|
| Resource Name | function name / service name          |
| Surface       | lambda / cloudrun / cloudfunc / azfunc / ocifunc |
| Runtime       | python3.11 / nodejs20.x / dotnet6 / etc. |
| Region        | function's region                     |
| Trace axis    | ✓ if `has_trace_axis` else ✗          |
| OTel distro   | ✓ if `has_otel_distro` else ✗         |
| Last seen     | relative time (per v0.89.77)          |
| Quality       | dot indicator (per v0.89.87)          |

Implementation note: only DiscoveryAWS uses inventory
*sections* today; GCP/Azure/OCI added the Serverless tab via
Radix Tabs (4th sub-tab). The QualityDot column ships on AWS
only in slice 1 — GCP/Azure/OCI pages don't render the dot
component elsewhere yet, so adding it on Serverless alone
would be inconsistent. Slice 2 unifies the column across
all 4 providers.

## Dashboard surfaces

### Discovery summary endpoint extension

`GET /api/v1/discovery/summary` per-provider response gains a
`serverless_count` field, summed alongside the existing
compute/database/cluster counts. The Unified Discovery
dashboard at `/discovery` shows the count in each provider's
card.

### Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` per-provider response
gains a `serverless_pct` field — % of inventoried serverless
functions that have emitted a span in the last 24h.

The TRACE COVERAGE dashboard panel chip breakdown now reads:

```
COMPUTE 67%  |  DB 42%  |  K8S 89%  |  SERVERLESS 33%
```

When `serverless_pct` is zero across all providers, the chip
line hides. Operators with no serverless adoption don't see a
permanent 0% indicator.

## Webhook routing

The kind-prefix detection in the IaC webhook handler extends
with 4 new cases:

```
lambda-       → aws
cloudrun-     → gcp
cloudfunc-    → gcp
azfunc-       → azure
ocifunc-      → oci
```

All 11 new recommendation kinds route to the correct
provider's audit scope. SIEM consumers can filter on:

```
recommendation_kind ~= "^(lambda-|cloudrun-|cloudfunc-|azfunc-|ocifunc-)"
```

## Workflow — first serverless scan

1. Open the per-provider Discovery page (e.g.
   `/discovery/aws`). Note your existing AWS connection.
2. If the connection was created before v0.89.90, you may
   need to upgrade the IAM policy to include the new
   serverless action: `lambda:ListFunctions`. The in-product
   IAM upgrade path (#590) shows the diff.
3. Click "Run scan" — the default tier list now includes
   `serverless`. The scan walks Lambda functions in addition
   to compute / db / k8s.
4. Click the Serverless Inventory sub-tab. Each function row
   shows the two axes + Last seen + Quality.
5. Click the Recommendations tab. Any function missing an
   axis fires a corresponding `lambda-*` / `cloudrun-*` /
   etc. recommendation.
6. Review the Terraform PR. Merge or decline.
7. After merge + apply + first invocation, wait ~5 minutes.
   Re-load the Serverless sub-tab; the Last seen column
   populates for the function.

## Reading the audit

Slice 1 reuses the existing audit event types — no new event
constants. The discovery scan emits the existing
`discovery.{provider}.scan_completed` event with the
`serverless_count` field now included in the payload.

The recommendation lifecycle (`recommendation.created`,
`recommendation.pr_opened`, `recommendation.pr_merged`,
`recommendation.pr_closed`) carries the new kind values.

## Troubleshooting

- **Lambda functions don't appear in the Serverless sub-tab.**
  Check the IAM policy — `lambda:ListFunctions` is required.
  The in-product IAM upgrade documentation shows the action
  to add. If the policy is correct but functions still don't
  appear, check the scan audit for `partial_reason` —
  Lambda's API may have rate-limited the scan, in which case
  the next scan should pick up the remaining functions.
- **A function with X-Ray active shows `last_seen_at = null`.**
  This is expected — see the X-Ray-without-ADOT caveat above.
  Merge the `lambda-otel-layer` recommendation.
- **Cloud Run scanner reports `has_otel_distro = false` but
  I have the sidecar.** Slice 1 detects the sidecar by
  container name prefix (`otel-*`). If your sidecar is named
  differently (e.g. `obs-agent`, `telemetry-relay`), the
  scanner misses it. The `cloudrun-otel-sidecar`
  recommendation that fires is a false positive — decline it
  and the verdict learning loop records the decline. Slice 2
  ships a configurable matcher list.
- **Cloud Functions runtime field is empty for an older
  function.** Cloud Functions Gen 1 doesn't return runtime
  in the same field as Gen 2. Slice 1 may show empty Runtime
  for Gen 1 functions; the trace + distro axes still work.
- **Azure function shows `has_otel_distro = false` despite
  `OTEL_DOTNET_AUTO_HOME` being set in the portal.** App
  settings sometimes don't read back immediately after a
  config change. Re-scan after waiting 60s for Azure to
  propagate. If the issue persists, check the function app's
  authentication — the scanner may have hit an older
  configuration revision.
- **OCI function with APM enabled shows
  `has_trace_axis = false`.** The config map value comparison
  is case-sensitive — `"true"` matches, `"True"` or `"TRUE"`
  doesn't. Slice 2 may relax this. For now, normalize the
  value in your Terraform to lowercase `"true"`.

## What slice 2 will add

Per §13 of the design doc:

- Cold-start latency analysis via cloud-native metrics
  correlation.
- Concurrent execution analysis on Cloud Run / Cloud Functions.
- Per-language SDK customization (Python asyncio, Node.js
  async_hooks, JVM agent variants, .NET profiler).
- Knative-on-bare-metal / non-native serverless patterns.
- Step Functions / Workflows / Logic Apps orchestration tier.
- EventBridge / Cloud Tasks / Service Bus / Streams event
  source tier.
- Function-specific span quality (cold-start span health,
  initialization-time attribute completeness).
- Per-surface configurable sidecar / layer matcher list.
- QualityDot column on GCP / Azure / OCI inventory tabs.

## The universal claim grows a fourth tier

After serverless slice 1, Squadron's positioning reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, AND SERVERLESS for
> observability gaps, verifies telemetry is actually flowing,
> validates the spans Squadron receives are healthy, AND
> drafts the IaC PRs that close the gaps it finds.

Four clouds. Four tiers. Four verbs. One control plane.
Seventeen scanner surfaces (4 clouds × 3 prior tiers + 5
serverless surfaces). Serverless is the canonical "where
did my trace go?" surface — the trace integration arc + span
quality arc together close the loop the slice promises:
Squadron sees what your cloud control plane lists as
serverless, verifies whether OTel spans actually arrive from
each function, and drafts the PR that gets the missing
primitive on.

## Cross-references

- [Serverless tier slice 1 design doc](./proposals/serverless-tier-slice1.md) —
  the locked spec this runbook operationalizes.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the trace integration arc this composes with.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  the span quality arc that validates the spans Squadron
  receives.
- [Database tier slice 2](./proposals/database-tier-slice2.md) —
  the prior tier-expansion arc this mirrors structurally.
- [Kubernetes tier slice 2](./proposals/kubernetes-tier-slice2.md) —
  same pattern, prior tier.
- [Audit log](./audit-log.md) — full catalog of event types.
