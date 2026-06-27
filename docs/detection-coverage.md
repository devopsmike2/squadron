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

> **v0.89.258:** when a serverless function lacks its detection-prerequisite
> add-on (Lambda Insights / Application Insights), the discovery proposer now
> emits a recommendation to enable it — explaining that without it Squadron is
> blind to that function's cold-start latency + error rate, and noting the
> add-on is paid (Lambda Insights per function-month; App Insights on
> ingestion). For AWS a cheaper CloudWatch Logs metric-filter alternative is
> offered. Kinds: `lambda-insights-enable`, `azfunc-appinsights-enable`.
| GCP | Cloud Run / Functions | Yes — `request_latencies` / `execution_times`. | ✅ (includes warm-path invocations; a permanently-warm service can show false positives). |
| Azure | Functions | **No** — Azure Monitor exposes only `FunctionExecutionCount` / `FunctionExecutionUnits`; there is no per-function duration metric and no `IsAfterColdStart` dimension. | ⚠️ Requires **Application Insights** (`requests`/duration). |
| OCI | Functions | Duration only — `oci_faas` has `FunctionExecutionDuration` but no cold-start counter. | ✅ Duration-regression heuristic (P95 current vs 7-day baseline); **not cold-start-isolated** — a spike may be a cold start or a slow dependency (v0.89.232). |

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
| AWS | SQS | **No** native poison RATE — but DLQ DEPTH is available. | ✅ **Depth/presence (v0.89.259, #156):** the source queue reads its DLQ's current `ApproximateNumberOfMessages` (free from the scan's attribute walk; same-account/region DLQs) and surfaces `poison_dlq_depth` + `poison_dlq_nonempty`. A non-empty DLQ ⇒ poison present. Proxy, not a rate: a drained DLQ reads empty. |
| GCP | Cloud Tasks | Yes — failed `task_attempt_count`. | ✅ |
| Azure | Service Bus | Yes — `DeadletteredMessages` gauge delta. | ✅ (delta approximation). |
| OCI | Queue | **No** — `oci_queue` has no dead-letter depth metric (`MessagesInDlq` does not exist; verified v0.89.236). | ⛔ Deferred — honest absent sentinel + monitor recommendation; depth-based signal via the `deadLetterQueueDeliveryCount` attribute is the planned fix. |

## Consumer-lag and cost-correlation

**Consumer-lag** detection does not depend on Monitoring metric *names* and so
is not subject to the availability gaps above. It reads queue **attributes**
directly — AWS SQS `ApproximateNumberOfMessages` + `ApproximateAgeOfOldestMessage`
(GetQueueAttributes), OCI `visibleMessages` + `timeStateLastChanged` (queue
list) — or GCP's verified `cloudtasks.googleapis.com/queue/depth` metric. Azure
Service Bus lag is honest-framed at the namespace level (absent sentinel + a
per-namespace queue-walk-prerequisite recommendation) until the per-queue ARM
walk lands. Verified v0.89.236.

**Cost-correlation** is opt-in (off by default) and reads each cloud's billing
/ cost-management API (AWS Cost Explorer, Azure Cost Management, GCP billing),
not the Monitoring namespaces — a separate surface from this matrix.

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
