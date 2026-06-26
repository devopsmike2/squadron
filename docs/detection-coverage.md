# Detection coverage & requirements

Squadron's **metric-based** detections (cold-start latency, error-rate,
poison-message rate) are only as good as the cloud metric they read. Some of
those metrics don't exist natively on every cloud — where that's true, the
detection either requires extra operator setup (Lambda Insights / Application
Insights) or is honestly deferred to a monitor recommendation. This page is the
authoritative, honest statement of what works where.

This page covers metric-based detections only. **Structural/config detections**
— trace-coverage presence (is the OTel primitive enabled?), event-source
diagnostic-settings presence, schema enforcement — read resource configuration
directly and work wherever the discovery RBAC is granted; they are not affected
by metric availability.

## Legend

- ✅ **Works** on native cloud metrics, no extra setup.
- ⚠️ **Requires setup** — the signal exists only in an add-on (Lambda Insights,
  Application Insights). Without it the detection cannot fire.
- ⛔ **Deferred (honest framing)** — no native metric exists; Squadron emits an
  absent sentinel (`-1`) and a monitor-add recommendation rather than asserting
  a value it can't measure.

## Cold-start latency

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | Lambda | **No** — `InitDuration` is a CloudWatch **Logs** REPORT field, not an `AWS/Lambda` metric. | ⚠️ Requires **Lambda Insights** (`LambdaInsights`/`init_duration`) or a Logs metric-filter. |
| GCP | Cloud Run / Functions | Yes — `request_latencies` / `execution_times`. | ✅ (includes warm-path invocations; a permanently-warm service can show false positives). |
| Azure | Functions | **No** — Azure Monitor exposes only `FunctionExecutionCount` / `FunctionExecutionUnits`; there is no per-function duration metric and no `IsAfterColdStart` dimension. | ⚠️ Requires **Application Insights** (`requests`/duration). |
| OCI | Functions | **No** — `oci_faas` has `FunctionInvocationCount` / `FunctionExecutionDuration` / `FunctionResponseCount` but no cold-start counter. | ⛔ Deferred (no cold-start gate metric). |

## Error rate

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | Lambda | Yes — `AWS/Lambda` `Errors` + `Invocations` (Sum). | ✅ |
| GCP | Cloud Run / Functions | Yes — `request_count` (5xx) / `execution_count` (status != ok). | ✅ |
| Azure | Functions | **No** — no native per-function error metric (`FunctionErrors` does not exist). | ⚠️ Requires **Application Insights** (`requests/failed`). |
| OCI | Functions | Yes — `oci_faas` `FunctionResponseCount` (error responses) over `FunctionInvocationCount` (fixed v0.89.229). | ✅ |

## Poison-message rate

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | SQS | **No** — there is no native counter for messages moved to a DLQ by the redrive policy. `NumberOfMessagesSent` excludes them (only manual SendMessage counts). | ⛔ Deferred (reverted v0.89.230) — monitor recommendation on the DLQ's `ApproximateNumberOfMessages`. Depth-based detection planned (#156). |
| GCP | Cloud Tasks | Yes — failed `task_attempt_count`. | ✅ |
| Azure | Service Bus | Yes — `DeadletteredMessages` gauge delta. | ✅ (delta approximation). |
| OCI | Queue | Per implementation (`oci_queue` depth gauge delta). | ✅ (not independently re-verified in the v0.89.229–231 audit). |

## Why some detections need an add-on

Cloud providers expose cold-start init duration and per-function error/duration
breakdowns through their **application-instrumentation** products (AWS Lambda
Insights, Azure Application Insights), not their base infrastructure-metrics
namespaces (`AWS/Lambda`, Azure Monitor `Microsoft.Web/sites`). Squadron is a
discovery control-plane that reads metrics; it does not enable those add-ons for
you. Where an add-on is required, enabling it makes the corresponding detection
start working on the next scan with no Squadron change.

See [docs/audit/detection-metric-availability.md](./audit/detection-metric-availability.md)
for the verification details and the open data-source decisions.
