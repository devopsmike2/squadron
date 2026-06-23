# Serverless tier slice 1 — fourth tier across all four clouds

**Status:** design doc, locked for slice 1 implementation. Builds
on the existing compute / database / kubernetes tier work
across all four clouds + the trace integration arc.

**See also:**
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Trace integration slice 2](./trace-integration-slice2.md),
[Span quality slice 1](./span-quality-slice1.md).

## 1. Problem

Squadron has the three-tier compute/database/kubernetes
discovery surface across all four clouds. The four-cloud claim
holds. But operators running production workloads on AWS,
GCP, Azure, and Oracle Cloud almost always have a non-trivial
serverless surface too: Lambda functions for event handlers,
Cloud Run services for HTTP APIs, Cloud Functions for cron,
Azure Functions for queue consumers, Functions for OCI events.

Today Squadron ignores the serverless surface entirely. That
matters because:

- Serverless workloads are exactly where observability gaps
  bite hardest. A Lambda cold-start that takes 4 seconds to
  initialize the OTel SDK loses the first invocation's spans
  by default. A Cloud Function whose execution context is
  recycled between invocations might lose batched spans on
  exit. A Cloud Run service with a misconfigured concurrency
  setting can mix spans across requests.
- Serverless is the canonical "where did my trace go" surface.
  The trace integration arc Squadron shipped (slices 1 + 2 +
  span quality slice 1) covers compute / db / k8s; without
  serverless, a multi-cloud operator with significant Lambda
  spend sees only part of the picture.
- The competitive landscape: Datadog / Honeycomb / Grafana
  Cloud all have first-class serverless integration, but they
  ship as SaaS pricing per-span. Squadron's OSS positioning
  is "the four-cloud control plane that respects the
  ephemerality" — that requires shipping the serverless
  surface natively.

Slice 1 of serverless tier adds a fourth tier to Squadron's
discovery surface alongside compute / db / k8s, scanning five
serverless surfaces across the four clouds:

- AWS: Lambda functions
- GCP: Cloud Run services + Cloud Functions
- Azure: Azure Functions
- OCI: OCI Functions

For each, slice 1 detects:
1. Whether the function/service has the cloud-native trace
   axis enabled (X-Ray active tracing for Lambda, Cloud Trace
   for Cloud Run / Functions, Application Insights for Azure
   Functions, APM for OCI Functions).
2. Whether the function/service has the ADOT / Cloud Run OTel
   sidecar / OpenTelemetry distribution attached.
3. Last span observation per function (via the existing
   traceindex from the trace integration arc).

Slice 1 wires this into the existing Discovery dashboard +
per-provider Inventory tab pattern, mirroring the database
and kubernetes tier slice 2 arcs.

## 2. Non-goals (slice 1)

- **Serverless cold-start latency analysis.** Detecting "your
  Lambda cold-start is 4s" needs metric data, not discovery
  data. Slice 2 candidate via cloud-native metrics
  correlation.
- **Concurrent execution analysis on Cloud Run / Functions.**
  Misconfigured concurrency mixing spans across requests is
  a real failure mode; the detection requires span-content
  inspection that doesn't fit slice 1's read-only discovery
  posture.
- **Per-function Terraform pattern depth.** Slice 1 ships
  the discovery surface + the SDK-enable recommendation
  kinds. Slice 2 ships the per-language SDK customization
  (which Lambda layer to attach for Python vs Node.js etc.).
- **Knative-on-bare-metal or Lambda-on-OCI** (or other
  serverless deployments that don't use the cloud's native
  serverless control plane). Slice 1 detects what the cloud
  control plane lists as serverless; non-native deployments
  are slice 3+.
- **Step Functions / Workflows / Logic Apps / OCI
  Resource Manager** — orchestration tier. Slice 2+.
- **EventBridge / Cloud Tasks / Azure Service Bus / OCI
  Streams** — event source tier. Slice 2+.

## 3. Per-cloud detection surfaces

Five serverless surfaces total. Each gets a per-cloud
serverless detection axis enumerated below. The recommendation
kinds follow the same `{primitive}-otel-{tier}` pattern as the
existing compute/db/k8s kinds — except for the serverless tier
the kind value is `{primitive}-otel-serverless` (e.g.
`lambda-otel-layer`, `cloudrun-otel-sidecar`).

### 3.1 AWS Lambda

API: `lambda:ListFunctions`, `lambda:GetFunction`. Required IAM
in the slice 1 trust policy: `lambda:ListFunctions`,
`lambda:GetFunctionConfiguration`.

Detection axes:

| Axis                     | Source                                  | Recommendation kind         |
|--------------------------|-----------------------------------------|-----------------------------|
| X-Ray active tracing     | `tracing_config.mode == "Active"`       | `lambda-xray-active`        |
| ADOT layer attached      | Layer ARN starts with `arn:aws:lambda:*:901920570463:layer:aws-otel-` | `lambda-otel-layer` |
| OTel env vars set        | `environment.variables[AWS_LAMBDA_EXEC_WRAPPER]` is set | `lambda-otel-wrapper` |

Coverage caveat: a Lambda function with X-Ray active tracing
but no ADOT layer emits X-Ray segments (visible in AWS console)
but NO OTel spans — Squadron's traceindex would see zero.
Slice 1's last_seen_at column accurately reflects "no OTel
spans seen" even when X-Ray is active. The runbook explains
this.

### 3.2 GCP Cloud Run

API: `run.googleapis.com/v1/projects/*/locations/*/services`
(via the Cloud Run Admin API). Required GCP permissions in
slice 1 SA: `run.services.list`, `run.services.get`.

Detection axes:

| Axis                            | Source                                       | Recommendation kind             |
|---------------------------------|----------------------------------------------|---------------------------------|
| Cloud Trace integration enabled | Service annotation `run.googleapis.com/trace` | `cloudrun-trace-enable`         |
| OTel sidecar container present  | Container with name matching `otel-collector*` | `cloudrun-otel-sidecar`         |
| OTEL_EXPORTER_OTLP_ENDPOINT env | Container env contains it                    | `cloudrun-otel-export-endpoint` |

### 3.3 GCP Cloud Functions

API: `cloudfunctions.googleapis.com/v1/projects/*/locations/*/functions`.
Required GCP permissions: `cloudfunctions.functions.list`,
`cloudfunctions.functions.get`.

Detection axes:

| Axis                          | Source                                       | Recommendation kind             |
|-------------------------------|----------------------------------------------|---------------------------------|
| Cloud Trace integration       | function.environmentVariables[GOOGLE_CLOUD_TRACE] | `cloudfunc-trace-enable`        |
| OpenTelemetry layer attached  | function.runtime in supported list AND function has the OpenTelemetry distribution layer | `cloudfunc-otel-layer` |

### 3.4 Azure Functions

API: `Microsoft.Web/sites?$filter=kind eq 'functionapp'` via
the Resource Manager API. Required Azure RBAC: existing
`Reader` role on the resource group covers this.

Detection axes:

| Axis                            | Source                                          | Recommendation kind            |
|---------------------------------|-------------------------------------------------|--------------------------------|
| Application Insights enabled    | `app_settings[APPLICATIONINSIGHTS_CONNECTION_STRING]` is set | `azfunc-appinsights-enable`    |
| OpenTelemetry distro attached   | `app_settings[OTEL_DOTNET_AUTO_HOME]` OR `OTEL_PYTHON_DISTRO` set | `azfunc-otel-distro`           |

### 3.5 OCI Functions

API: `functions.GetFunction`, `functions.ListFunctions`.
Required OCI policy: `inspect functions in compartment`.

Detection axes:

| Axis                          | Source                                       | Recommendation kind             |
|-------------------------------|----------------------------------------------|---------------------------------|
| APM trace integration enabled | function `config[OCI_APM_ENABLED]` is true   | `ocifunc-apm-enable`            |
| OpenTelemetry distro attached | function `config[OTEL_DISTRO]` is set        | `ocifunc-otel-distro`           |

## 4. Storage schema

New `serverless_instance` storage table mirroring the existing
compute/db/k8s instance tables. Schema migration v10 → v11.

```sql
CREATE TABLE IF NOT EXISTS serverless_instance (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    scan_id TEXT NOT NULL,
    provider TEXT NOT NULL, -- "aws" / "gcp" / "azure" / "oci"
    surface TEXT NOT NULL, -- "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc"
    account_id TEXT NOT NULL,
    region TEXT NOT NULL,
    resource_name TEXT NOT NULL, -- function name / service name
    resource_arn TEXT, -- ARN / fully-qualified resource id
    runtime TEXT, -- python3.11, nodejs20.x, dotnet6, etc.
    has_trace_axis INTEGER NOT NULL, -- bool: cloud-native trace primitive active
    has_otel_distro INTEGER NOT NULL, -- bool: OTel SDK/layer present
    last_seen_at TIMESTAMP, -- joined from traceindex; null when no spans
    snapshot_json TEXT NOT NULL, -- full per-cloud detail
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (connection_id, scan_id, resource_arn)
);
CREATE INDEX IF NOT EXISTS idx_serverless_scan ON serverless_instance(scan_id);
CREATE INDEX IF NOT EXISTS idx_serverless_conn ON serverless_instance(connection_id);
```

Schema version bumps to v11. Migration adds the table without
backfilling — pre-slice-1 scans don't have serverless data.

## 5. Scanner contract

Each per-cloud scanner adds a `ScanServerless(ctx, scope)
([]ServerlessInstanceSnapshot, error)` method. The
ServerlessInstanceSnapshot struct lives in
`internal/discovery/scanner/scanner.go`:

```go
type ServerlessInstanceSnapshot struct {
    Provider      string `json:"provider"`
    Surface       string `json:"surface"` // lambda / cloudrun / etc.
    AccountID     string `json:"account_id"`
    Region        string `json:"region"`
    ResourceName  string `json:"resource_name"`
    ResourceARN   string `json:"resource_arn"`
    Runtime       string `json:"runtime,omitempty"`
    HasTraceAxis  bool   `json:"has_trace_axis"`
    HasOTelDistro bool   `json:"has_otel_distro"`
    LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
    Detail        map[string]any `json:"detail,omitempty"` // surface-specific bag
}
```

Per-cloud scanner implementations live alongside the existing
ScanCompute / ScanDatabase / ScanClusters methods:

- `internal/discovery/scanner/aws/lambda.go`
- `internal/discovery/scanner/gcp/cloudrun.go` and `cloudfunc.go`
- `internal/discovery/scanner/azure/functions.go`
- `internal/discovery/scanner/oci/functions.go`

## 6. API surface

### 6.1 Existing per-provider scan endpoint

The existing `POST /api/v1/discovery/{provider}/scan` endpoint
extends to optionally include the serverless tier. Today the
scan request body accepts a list of tiers; slice 1 adds
`"serverless"` as a valid value.

When the scan request omits `tiers`, the default behavior
extends from `[compute, database, kubernetes]` to
`[compute, database, kubernetes, serverless]`. Existing
operators who passed the explicit list get the old behavior;
default-callers get the wider surface.

### 6.2 Existing per-provider inventory endpoint

The existing `GET /api/v1/discovery/{provider}/inventory`
response shape extends:

```json
{
  "compute": [...],
  "databases": [...],
  "clusters": [...],
  "serverless": [...]  // new
}
```

### 6.3 Discovery summary endpoint extension

`GET /api/v1/discovery/summary` (the unified dashboard
aggregation from v0.89.61) extends the per-provider shape:

```json
{
  "providers": {
    "aws": {
      "connection_count": N,
      "compute_count": N,
      "database_count": N,
      "cluster_count": N,
      "serverless_count": N,  // new
      "instrumented_count": N,  // unchanged — sums across tiers
      ...
    },
    ...
  }
}
```

### 6.4 Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` (v0.89.76 extended in
v0.89.82) per-provider response gets a `serverless_pct` field
alongside the existing tier coverage breakdown. The aggregation
counts a serverless function as "emitting" when its
last_seen_at is within 24h, same rule as the other tiers.

## 7. UI

The four per-provider pages (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure / DiscoveryOCI) each gain a new "Serverless"
sub-tab in their Inventory section, alongside the existing
Compute / Databases / Kubernetes sub-tabs.

Each Serverless sub-tab table columns:
- Resource Name
- Surface (lambda / cloudrun / cloudfunc / azfunc / ocifunc)
- Runtime
- Region
- Trace axis (✓ / ✗)
- OTel distro (✓ / ✗)
- Last seen (per the v0.89.77 column pattern)
- Quality dot (per the v0.89.87 column pattern)

The Discovery dashboard's TRACE COVERAGE panel gains a
"Serverless" line on the per-provider chip breakdown:

```
COMPUTE 67%  |  DB 42%  |  K8S 89%  |  SERVERLESS 33%
```

## 8. Recommendation kinds

Following the pattern from prior arcs, 11 new kinds across the
5 surfaces × per-axis recommendations:

```
lambda-xray-active           cloudrun-trace-enable          azfunc-appinsights-enable
lambda-otel-layer            cloudrun-otel-sidecar          azfunc-otel-distro
lambda-otel-wrapper          cloudrun-otel-export-endpoint  ocifunc-apm-enable
                             cloudfunc-trace-enable         ocifunc-otel-distro
                             cloudfunc-otel-layer
```

Webhook routing extends to recognize the new prefixes:
- `lambda-` → AWS
- `cloudrun-`, `cloudfunc-` → GCP
- `azfunc-` → Azure
- `ocifunc-` → OCI

The kind-prefix detection switch in
`internal/api/handlers/iac_github_webhook.go` extends with
these 4 new cases.

Proposer system prompt extension lives in
`internal/ai/proposer_discovery_prompt.go`, mirroring the
v0.89.66 (database tier slice 2 chunk 5) and v0.89.71
(kubernetes tier slice 2 chunk 5) patterns.

## 9. Slice 1 contract

**In:**

1. Storage schema v10 → v11 with serverless_instance table.
2. ServerlessInstanceSnapshot struct on scanner package.
3. ScanServerless methods on all 4 provider scanners (5
   surfaces total — Lambda + Cloud Run + Cloud Functions +
   Azure Functions + OCI Functions).
4. Existing per-provider scan/inventory endpoints extended.
5. Discovery summary + trace_coverage endpoints extended.
6. Discovery dashboard TRACE COVERAGE panel serverless line.
7. Per-provider Inventory sub-tab "Serverless" with columns
   per §7.
8. Proposer prompt extension with 11 new recommendation kinds.
9. Webhook routing for the 4 new kind prefixes.
10. Operator runbook covering all the above.
11. Acceptance tests covering all 5 surfaces' scanner
    detection logic, the new endpoint shape, the new UI
    sub-tab rendering, and cold-start parity.

**Out:**

- Cold-start latency analysis.
- Concurrent execution analysis.
- Per-language SDK customization.
- Knative / non-native serverless.
- Orchestration tier (Step Functions / Workflows / Logic Apps).
- Event source tier (EventBridge / Cloud Tasks / Service Bus /
  Streams).

## 10. Implementation chunks

- **Chunk 1: Foundation + AWS Lambda scanner.**
  ServerlessInstanceSnapshot struct, storage migration v10→v11,
  AWS Lambda scanner. The foundation chunk picks AWS to land
  first because the API is the simplest of the five (one call,
  one detection axis loop). ~900-1100 lines.
  **v0.89.90.**
- **Chunk 2: GCP Cloud Run + Cloud Functions scanners.**
  Parallel-eligible with chunks 3/4. ~600-800 lines.
  **v0.89.91.**
- **Chunk 3: Azure Functions scanner.** Parallel-eligible.
  ~500-700 lines. **v0.89.91 (same release as chunk 2 via
  parallel merge if both clean).**
- **Chunk 4: OCI Functions scanner.** Parallel-eligible.
  ~500-700 lines. **v0.89.91.**
- **Chunk 5: Proposer prompt + UI Serverless sub-tab +
  webhook routing.** ~900-1100 lines. **v0.89.92.**
- **Chunk 6: Operator runbook + dashboard chip line +
  README index entry.** ~400-500 lines. **v0.89.93.**

Total: ~4-5 release tags. The parallel scanner pattern from
the kubernetes tier slice 2 arc (v0.89.70 chunks 2-4)
composes cleanly here.

## 11. Acceptance tests

1. **AWS Lambda scanner — function with X-Ray Active and
   ADOT layer.** Mock Lambda API returning a function with
   `tracing_config.mode = "Active"` and a layer ARN starting
   with the ADOT prefix. Assert: snapshot has
   HasTraceAxis=true AND HasOTelDistro=true.
2. **AWS Lambda scanner — function with X-Ray Active only,
   no ADOT.** Same but no ADOT layer. Assert: HasTraceAxis=true
   AND HasOTelDistro=false.
3. **AWS Lambda scanner — function with neither.** Assert:
   both false.
4. **GCP Cloud Run scanner — service with Cloud Trace
   annotation.** Assert: HasTraceAxis=true.
5. **GCP Cloud Run scanner — service with OTel sidecar.**
   Container list includes `otel-collector` name. Assert:
   HasOTelDistro=true.
6. **GCP Cloud Functions scanner — function with OTel layer.**
   Assert: HasOTelDistro=true.
7. **Azure Functions scanner — function with App Insights.**
   Assert: HasTraceAxis=true.
8. **Azure Functions scanner — function with OTel distro env.**
   Assert: HasOTelDistro=true.
9. **OCI Functions scanner — function with APM enabled.**
   Assert: HasTraceAxis=true.
10. **Storage migration v10→v11 idempotent.** Run migration
    twice. Assert: no error, table exists, no data loss on
    pre-existing compute/db/k8s tables.
11. **Discovery summary includes serverless_count.** Seed
    inventory with 3 serverless functions. Assert: summary
    response per-provider includes serverless_count=3.
12. **Trace coverage includes serverless_pct.** Seed
    inventory with 2 serverless functions, 1 emitting.
    Assert: serverless_pct=50.
13. **Discovery dashboard chip renders serverless line when
    non-zero.** Mock summary to return non-zero serverless
    counts. Assert: chip displays serverless line.
14. **Webhook routes lambda-xray-active to aws.**
15. **Webhook routes cloudrun-otel-sidecar to gcp.**
16. **Webhook routes azfunc-appinsights-enable to azure.**
17. **Webhook routes ocifunc-apm-enable to oci.**
18. **Cold-start parity preserved.** All 4 providers cold-start
    prompts byte-identical to v0.89.88 when no serverless
    rows trigger recommendations.

## 12. Threat model

Slice 1 introduces no new external surface (Squadron's
existing cloud connections + IAM/SA/SP/policy substrates cover
the new APIs). The new threat surface is:

**Wider API permission requests.** Each cloud's slice 1 IAM
template grows to cover the new serverless API calls. For
AWS that's `lambda:ListFunctions` +
`lambda:GetFunctionConfiguration`. These are read-only and
sit on the existing IAM upgrade flow (#590) — operators get
the in-product upgrade path documentation.

**Lambda layer ARN whitelist drift.** Slice 1 detects the
ADOT layer by ARN prefix
(`arn:aws:lambda:*:901920570463:layer:aws-otel-`). AWS may
publish new ADOT layer ARNs over time. The whitelist lives in
a constant; the runbook documents how to keep it current. A
miss on the whitelist is a false negative (Squadron reports
"no ADOT layer" when one is present), surfacing as the operator
declining an `lambda-otel-layer` recommendation that was
inappropriate. The verdict learning loop records the decline.

**Cloud Run container image opacity.** Slice 1's Cloud Run
scanner inspects the container name (e.g. `otel-collector`)
to detect the sidecar. A team that names their sidecar
something different (`telemetry-agent`, `obs-relay`) won't
match. The runbook documents the matcher; slice 2 may add a
configurable matcher list.

**Function code surface.** Slice 1 does NOT inspect function
code or zip content. The detection is metadata-only.

## 13. Slice 2 candidates

- Cold-start latency analysis via cloud-native metrics.
- Concurrent execution analysis on Cloud Run / Functions.
- Per-language SDK customization (Python vs Node.js vs JVM
  per surface).
- Knative-on-bare-metal / Lambda-on-OCI patterns.
- Step Functions / Workflows / Logic Apps orchestration tier.
- EventBridge / Cloud Tasks / Service Bus / Streams event
  source tier.
- Function-specific span quality (cold-start span health,
  initialization-time attribute completeness).
- Per-surface configurable sidecar / layer matcher list.

---

**Strategic frame:**

Squadron's universal claim grows from three tiers to four:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, AND SERVERLESS for
> observability gaps, verifies telemetry is actually
> flowing, validates the spans Squadron receives are healthy,
> AND drafts the IaC PRs that close the gaps it finds.

Four clouds. Four tiers. Four verbs. One control plane.
Serverless is the canonical "where did my trace go" surface;
the trace integration arc + span quality arc already in place
make this the natural next step. The Tuesday LinkedIn drumbeat
narrative now has a concrete answer to the question every
serverless team asks: "did my Lambda cold-start eat the span?"
— Squadron checks, surfaces, and drafts the fix.
