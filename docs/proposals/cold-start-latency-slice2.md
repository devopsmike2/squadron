# Cold-start latency analysis slice 2 — 4-cloud generalization

**Status:** design doc, locked for slice 2 implementation.
Extends the MEASURES verb from slice 1's AWS-only Lambda
coverage to all 4 clouds via the existing
`MetricQuerier` substrate.

**See also:**
[Cold-start latency slice 1](./cold-start-latency-slice1.md),
[Serverless tier slice 1](./serverless-tier-slice1.md),
[Span quality slice 1](./span-quality-slice1.md).

## 1. Problem

Slice 1 of cold-start latency analysis shipped AWS Lambda
coverage and the substrate (`MetricQuerier` interface +
rate-limited CloudWatch client + `cold_start_observation`
storage). The substrate was deliberately designed as the
foundation for all 4 clouds; slice 2 closes the
qualification on the 5th verb (MEASURES is currently
1-cloud, qualified in the universal claim).

The challenge slice 2 confronts honestly: each cloud's
"cold-start" metric is shape-different. There is no
cross-cloud `InitDuration` standardization. Operators
running multi-cloud serverless fleets need consistent
detection logic across clouds; Squadron provides this by
TRANSLATING each cloud's native metric into the same
`AggregateMetricResult` shape the substrate's slice 1
already established.

The shape differences:

- **AWS Lambda** (slice 1): `AWS/Lambda InitDuration` —
  isolated cold-start init time in milliseconds.
- **GCP Cloud Run**: `request_latencies` — request-level
  latency that INCLUDES cold-start when applicable; slice
  2 filters by `serving_state` dimension or by querying the
  separate `instance_count` start events.
- **GCP Cloud Functions**: `execution_time` — function
  execution time; cold-start is the first invocation per
  instance.
- **Azure Functions**: `FunctionExecutionDuration` —
  filterable by `IsAfterColdStart` dimension to isolate
  cold-start invocations.
- **OCI Functions**: `function_duration` aggregate +
  `cold_start_count` counter — slice 2 derives an
  approximated cold-start P95 by joining the two.

After slice 2, the MEASURES verb in the universal claim is
4-cloud. The detection logic stays uniform (1.5x ratio + 500ms
floor + 50 baseline samples); the only thing that varies
per-cloud is the metric source.

## 2. Non-goals (slice 2)

- **Per-cloud threshold tuning.** The 1.5x ratio + 500ms
  floor + 50 baseline sample minimums from slice 1 apply
  uniformly across all 4 clouds. Some clouds may benefit
  from per-cloud tuning (e.g. Cloud Run's request_latency
  metric includes warm-path invocations; the ratio might
  need adjustment). Slice 3 candidate.
- **Sampling rate analysis.** Same substrate, different
  detection rules; closes span quality §13 deferral. Slice 3
  candidate.
- **Error rate correlation.** Same substrate. Slice 3+.
- **Cross-cloud cold-start correlation.** A Lambda that
  invokes a Cloud Function across cloud boundaries — slice
  4+ work.
- **Per-language fingerprinting.** Slice 1 deferral that
  stays deferred; the 3-failure-mode reasoning applies
  uniformly.
- **Provisioned-equivalent recommendations per cloud.** AWS
  Lambda has provisioned concurrency; Cloud Run has
  `min-instances`; Azure Functions has Premium Plan; OCI
  Functions has no equivalent. Slice 2 emits per-cloud
  Terraform patterns that target the closest equivalent.
- **Real-time metric streaming.** Same as slice 1 — Squadron
  stays a discovery + correlation surface.
- **Auto-fix.** Squadron remains a recommender.

## 3. Per-cloud detection

### 3.1 GCP Cloud Run

API: Cloud Monitoring V3
`projects/{project}/timeSeries` with metric filter
`metric.type = "run.googleapis.com/request_latencies"`.

Required GCP permissions:
`monitoring.timeSeries.list`. Slice 1 trust policy already
includes `monitoring.metricDescriptors.list`; slice 2 adds
the timeSeries permission.

Detection logic:
- Query `request_latencies` filtered by
  `resource.labels.service_name = "{cloud_run_service_name}"`
  AND `metric.labels.response_code_class = "2xx"` (filter to
  successful responses for cleaner baseline).
- Aggregate at 5-minute intervals over 24h current + 168h
  baseline windows.
- Use P95 statistic.

Coverage caveat: Cloud Run's `request_latencies` includes
warm-path invocations. A service that is permanently warm
(min-instances set, regular traffic) will show low latency
that doesn't reflect cold-start behavior. The recommendation
reasoning notes this; the verdict learning loop records
declines.

Recommendation kind: `cloudrun-cold-start-baseline`.

Terraform pattern: `google_cloud_run_service.template.spec.containerConcurrency`
adjustment + `metadata.annotations["autoscaling.knative.dev/minScale"] = "1"`.

### 3.2 GCP Cloud Functions

API: Cloud Monitoring V3
`projects/{project}/timeSeries` with metric filter
`metric.type = "cloudfunctions.googleapis.com/function/execution_times"`.

Detection logic:
- Query `execution_times` filtered by
  `resource.labels.function_name = "{function_name}"` AND
  `metric.labels.status = "ok"` (filter to successful
  invocations).
- Aggregate at 5-minute intervals over 24h current + 168h
  baseline windows.
- Use P95 statistic.

Coverage caveat: similar to Cloud Run, `execution_times`
includes warm invocations. Squadron's detection treats this
as the operator-facing cold-start metric (since `execution_times`
is the primary perceived-latency metric for Cloud Functions).
Slice 3 may add `initialization_time` separation.

Recommendation kind: `cloudfunc-cold-start-baseline`.

Terraform pattern:
`google_cloudfunctions2_function.service_config.min_instance_count = 1`.

### 3.3 Azure Functions

API: Azure Monitor REST API
`/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Web/sites/{name}/providers/microsoft.insights/metrics`
with metric name `FunctionExecutionDuration`.

Required Azure RBAC: existing Reader role covers
`microsoft.insights/metrics/read`. Slice 2 requires no
additional permission grant.

Detection logic:
- Query `FunctionExecutionDuration` filtered by
  `IsAfterColdStart = true` dimension.
- Aggregate at PT5M intervals over 24h current + 168h
  baseline windows.
- Use P95 statistic.

Coverage caveat: the `IsAfterColdStart` dimension was
introduced in 2023+ runtime versions. Function Apps on older
runtimes may not emit this dimension; slice 2 detects
absence and falls back to unfiltered P95 with an
informational note. The runbook documents the runtime
version requirement.

Recommendation kind: `azfunc-cold-start-baseline`.

Terraform pattern: migrate from Consumption tier to Premium
Plan tier (`azurerm_service_plan.sku_name = "EP1"`) OR
configure `azurerm_linux_function_app.app_settings["WEBSITE_USE_PLACEHOLDER"] = "0"`.

### 3.4 OCI Functions

API: OCI Monitoring
`/20180401/metricData/actions/summarizeMetricsData` with
namespace `oci_functions` and metric query
`function_duration[24h]{resourceId = "{function-ocid}"}.p95()`.

Required OCI policy: `read metrics in compartment`.

Detection logic:
- Query `function_duration` aggregated at p95 over 24h current
  + 168h baseline windows.
- Cross-reference with `cold_start_count` over the same window
  to verify the function actually experienced cold starts.
- If `cold_start_count == 0`, skip detection (no cold starts
  in window).

Coverage caveat: OCI Functions doesn't have an isolated
cold-start latency metric. Slice 2 uses `function_duration`
as the proxy when `cold_start_count > 0` — this is the
honest approximation given OCI's metric surface today.
Slice 3 may add per-execution trace correlation when OCI
exposes more granular metrics.

Recommendation kind: `ocifunc-cold-start-baseline`.

Terraform pattern: `oci_functions_function.config["WARMUP_DELAY"]`
adjustment + `provisioned_concurrent_executions` increase
when OCI Functions adds that field (currently in preview).

## 4. Storage schema

NO migration. The existing `cold_start_observation` table from
slice 1 already carries `provider` + `surface` columns. Slice
2 just adds rows with `provider in ("gcp", "azure", "oci")`
and `surface in ("cloudrun", "cloudfunc", "azfunc", "ocifunc")`.

Schema stays at v14.

## 5. Scanner contract

Per-cloud Scanner types satisfy the existing
`MetricQuerier` interface. The slice 1 AWS implementation is
the template:

- `internal/discovery/gcp/metrics.go` — Cloud Monitoring V3
  wrapper supporting both `run.googleapis.com/request_latencies`
  and `cloudfunctions.googleapis.com/function/execution_times`.
- `internal/discovery/azure/metrics.go` — Azure Monitor REST
  wrapper supporting `FunctionExecutionDuration` with
  optional `IsAfterColdStart` dimension filter.
- `internal/discovery/oci/metrics.go` — OCI Monitoring
  wrapper supporting `function_duration` + `cold_start_count`.

Each implementation:
- Authenticates via existing per-cloud credentials.
- Implements the rate limiter pattern from slice 1 (per-cloud
  rate limits: GCP 60 RPM, Azure 12K RPH, OCI 10 TPS).
- Handles empty result sets (return zero, no error).
- Surfaces ErrMetricNotImplemented for unsupported metric
  names (not all metrics work across all clouds; the
  interface is stable but per-cloud coverage varies).

## 6. API surface

The slice 1 per-resource cold_start endpoint extends to all
4 providers without changing the response shape — the
`AggregateMetricResult` substrate already abstracts the
per-cloud differences.

```
GET /api/v1/discovery/aws/inventory/serverless/{id}/cold_start
GET /api/v1/discovery/gcp/inventory/serverless/{id}/cold_start
GET /api/v1/discovery/azure/inventory/serverless/{id}/cold_start
GET /api/v1/discovery/oci/inventory/serverless/{id}/cold_start
```

The per-provider inventory endpoint extends the new fields
from slice 1 (`cold_start_p95_ms` + `cold_start_exceeds_threshold`)
to all 4 providers' Serverless rows.

## 7. UI

The DiscoveryAWS Serverless section's Cold-start P95 (24h)
column from slice 1 extends to:

- **DiscoveryGCP**: Cloud Run + Cloud Functions rows get the
  column with the same amber state on `exceeds_threshold`.
- **DiscoveryAzure**: Azure Functions rows get the column.
- **DiscoveryOCI**: OCI Functions rows get the column.

The dashboard's TRACE COVERAGE chip doesn't change — cold-start
is a separate diagnostic dimension. Slice 3 may add a
"LATENCY OUTLIERS" panel summarizing exceedances across all
4 clouds.

## 8. Recommendation kinds

4 new kinds, one per cloud × surface (counting both GCP
surfaces as separate kinds):

```
cloudrun-cold-start-baseline       azfunc-cold-start-baseline
cloudfunc-cold-start-baseline      ocifunc-cold-start-baseline
```

All reuse existing webhook prefixes from v0.89.92 (serverless
tier chunk 5) — NO new routing.

Reasoning template per kind mirrors the slice 1
`lambda-cold-start-baseline` 3-failure-mode pattern, with
per-cloud Terraform patterns from §3.

## 9. Slice 2 contract

**In:**

1. GCP `metrics.go` implementing `MetricQuerier` for Cloud
   Monitoring V3 with Cloud Run + Cloud Functions metric
   support.
2. Azure `metrics.go` implementing `MetricQuerier` for
   Azure Monitor REST with `FunctionExecutionDuration`.
3. OCI `metrics.go` implementing `MetricQuerier` for OCI
   Monitoring with `function_duration` + `cold_start_count`.
4. Per-cloud scan extension running cold-start detection
   for the relevant surfaces.
5. Per-cloud rate limiters (GCP 60 RPM, Azure 12K RPH, OCI
   10 TPS).
6. 4 new recommendation kinds in the proposer prompt with
   per-cloud Terraform patterns.
7. iacpicker per-cloud emitters for the cold-start
   recommendation Terraform patterns.
8. UI Cold-start P95 column extended to DiscoveryGCP /
   Azure / OCI Serverless sections.
9. Inventory endpoint extension for all 4 providers (not
   just AWS).
10. Per-resource cold_start endpoint extended to all 4
    providers.
11. Operator runbook covering all the above.
12. Acceptance tests for all 4 per-cloud `MetricQuerier`
    implementations, detection ratio + floor consistency
    across clouds, cold-start parity.

**Out:**

- Per-cloud threshold tuning.
- Sampling rate analysis.
- Error rate correlation.
- Cross-cloud cold-start correlation.
- Per-language fingerprinting.
- Real-time metric streaming.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: GCP `MetricQuerier` + Cloud Run + Cloud Functions
  scanners.** Parallel-eligible with chunks 2+3. ~900-1100
  lines. **v0.89.118.**
- **Chunk 2: Azure `MetricQuerier` + Azure Functions scanner.**
  Parallel-eligible. ~700-900 lines. **v0.89.118.**
- **Chunk 3: OCI `MetricQuerier` + OCI Functions scanner.**
  Parallel-eligible. ~600-800 lines. **v0.89.118.**
- **Chunk 4: Per-cloud detection branch + proposer prompt
  extension + UI per-provider column extension.** ~900-1100
  lines. **v0.89.119.**
- **Chunk 5: Operator runbook extension + README index.**
  ~300-400 lines. **v0.89.120.**

Total: 3 release tags. Parallel scanner fan-out reduces tag
count.

## 11. Acceptance tests

1. **GCP MetricQuerier — Cloud Run service with high P95**:
   Cloud Monitoring V3 fake returns timeSeries with high P95.
   Assert: AggregateMetricResult.Value > floor; ExceedsFloor true.
2. **GCP MetricQuerier — empty timeSeries response**:
   Assert: Value=0, SampleCount=0, no error.
3. **GCP MetricQuerier — Cloud Functions execution_times
   returns P95**.
4. **GCP rate limiter caps at 60 RPM** — 120 requests should
   take ~60 seconds.
5. **Azure MetricQuerier — FunctionExecutionDuration with
   IsAfterColdStart=true dimension filter applied**.
6. **Azure MetricQuerier — function on older runtime without
   IsAfterColdStart dimension** falls back to unfiltered with
   informational note.
7. **Azure rate limiter caps at 12000 RPH** (200 RPM target).
8. **OCI MetricQuerier — function_duration with
   cold_start_count > 0**: Assert P95 returned.
9. **OCI MetricQuerier — function_duration with
   cold_start_count = 0**: Assert detection skipped (no cold
   starts in window).
10. **OCI rate limiter caps at 10 TPS**.
11. **Per-cloud detection thresholds (1.5x ratio + 500ms
    floor) match slice 1**: pin to identical values across
    AWS / GCP / Azure / OCI.
12. **Recommendation kind dispatched per provider**:
    cloudrun-cold-start-baseline → GCP, azfunc-cold-start-baseline → Azure, etc.
13. **Cold-start parity preserved**: all 4 providers
    cold-start prompts byte-identical to v0.89.116 when no
    rows trigger recommendations.

## 12. Threat model

**Wider per-cloud permissions:**
- GCP: `monitoring.timeSeries.list` (read-only).
- Azure: existing Reader role covers `microsoft.insights/metrics/read`.
- OCI: `read metrics in compartment` policy statement.

All read-only. Operators get the in-product IAM/SA/policy
upgrade flow (#590) for each cloud.

**Per-cloud rate limit thresholds:**
- GCP Cloud Monitoring: 60 RPM (well under the 6000 RPM API
  quota).
- Azure Monitor: 12000 RPH (200 RPM, well under Azure
  Monitor's per-subscription quota).
- OCI Monitoring: 10 TPS (matches OCI's documented limit).

For a 4-cloud fleet of 1000 functions across all clouds, the
substrate adds roughly the same scan-time cost per cloud as
slice 1's AWS overhead.

**Cost surface:**
- GCP Cloud Monitoring: free through 1M API calls/month.
- Azure Monitor: free for basic metrics.
- OCI Monitoring: free for the first 50 ingestion endpoints;
  metric query is free.

Slice 2's cost surface is essentially $0 for typical fleets.
Documented in the runbook so operators know.

**False-positive risks per cloud:**
- GCP Cloud Run: `request_latencies` includes warm-path
  invocations. Permanently-warm services will show low
  latency that doesn't reflect cold-start behavior.
- GCP Cloud Functions: similar — `execution_times` includes
  warm. Slice 3 may switch to `initialization_time` when
  GCP exposes it as a separate metric.
- Azure: functions on older runtimes without
  IsAfterColdStart dimension get unfiltered P95 with an
  informational note. Operators with older runtimes may see
  false positives.
- OCI: when `cold_start_count > 0`, the `function_duration`
  P95 is the best approximation but is NOT cold-start-isolated.

The exclusion table from #531 slice 2 chunk 4 handles all
4 cases; verdict learning loop records.

## 13. Slice 3 candidates

- Per-cloud threshold tuning (Cloud Run may need 2.0x
  instead of 1.5x because warm-path skews the baseline).
- Sampling rate analysis using the same substrate.
- Error rate correlation.
- Cross-cloud cold-start correlation.
- Per-language fingerprinting.
- Per-execution trace correlation (cold-start spans by
  trace ID).
- Time-of-day-aware baseline windows.
- "LATENCY OUTLIERS" dashboard panel summarizing across all
  4 clouds.
- GCP `initialization_time` separation when GCP exposes it.
- Azure dimension fallback graceful degradation when older
  runtime detected.

---

**Strategic frame:**

After slice 2, Squadron's universal claim's 5th verb drops
the qualification asterisk:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, MEASURES cold-start latency
> across all four clouds against expected baselines, AND
> drafts the IaC PRs that close the gaps it finds.

**Five verbs.** Four clouds. Six tiers. One control plane.
The MEASURES verb is now uniformly 4-cloud, matching the
other four verbs.

The substrate that slice 1 built is what makes this slice
small. The detection logic stays uniform (1.5x ratio + 500ms
floor + 50 baseline samples); the only variable is the
per-cloud metric source. After slice 2, sampling rate
analysis becomes a small detection-logic arc that reuses
the generalized substrate across all 4 clouds — the
infrastructure investment compounds.

The Tuesday LinkedIn drumbeat narrative gains a cross-cloud
diagnostic dimension. An operator running a multi-cloud
fleet now sees cold-start regressions on the same dashboard
chip regardless of which cloud holds the function. The
"where did my trace go?" question gains a third sibling:
"where did my latency go?"
