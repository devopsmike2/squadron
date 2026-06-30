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
- 🏢 **Commercial-tier detection** — the regression detection depends on a paid
  telemetry add-on (Lambda Insights / Application Insights) and is part of the
  commercial tier. **OSS does not compute it**; instead OSS surfaces the gap by
  recommending you enable the add-on (`lambda-insights-enable`,
  `azfunc-appinsights-enable`). The detectors are **implemented and activated
  end-to-end** behind `commercial_detectors.enabled` (default off; AWS
  v0.89.312 / Azure v0.89.313): when enabled, a real discovery scan runs the
  regression detectors against the add-on telemetry (Lambda Insights
  `init_duration`; Application Insights `requests/duration` / `requests/failed`)
  and surfaces the result on the serverless inventory rows. With the gate off —
  the OSS default — the detectors stay dormant and behaviour is unchanged. See
  [Enabling commercial-tier detection](#enabling-commercial-tier-detection)
  below and [what's OSS vs Enterprise](./oss-vs-enterprise.md).

## Cold-start latency

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | Lambda | **No** — `InitDuration` is a CloudWatch **Logs** REPORT field, not an `AWS/Lambda` metric. | 🏢 **Commercial-tier (implemented, gated — v0.89.306, #152)** — the regression detector re-points to **Lambda Insights** (`LambdaInsights`/`init_duration`, dimension `function_name`) when `commercial_detectors.enabled=true`. OSS default off: queries `AWS/Lambda` (empty → never fires) and recommends enabling the add-on (`lambda-insights-enable`). |

> **v0.89.258:** when a serverless function lacks its detection-prerequisite
> add-on (Lambda Insights / Application Insights), the discovery proposer now
> emits a recommendation to enable it — explaining that without it Squadron is
> blind to that function's cold-start latency + error rate, and noting the
> add-on is paid (Lambda Insights per function-month; App Insights on
> ingestion). For AWS a cheaper CloudWatch Logs metric-filter alternative is
> offered. Kinds: `lambda-insights-enable`, `azfunc-appinsights-enable`.
| GCP | Cloud Run / Functions | Yes — `request_latencies` / `execution_times`. | ✅ (includes warm-path invocations; a permanently-warm service can show false positives). |
| Azure | Functions | **No** — Azure Monitor exposes only `FunctionExecutionCount` / `FunctionExecutionUnits`; there is no per-function duration metric and no `IsAfterColdStart` dimension. | 🏢 **Commercial-tier (implemented, gated — v0.89.307, #153)** — the regression detector re-points to **Application Insights** `requests/duration` (queried on the App Insights component resource via Azure Monitor) when `commercial_detectors.enabled=true`. OSS default off: queries `FunctionExecutionDuration` (empty → never fires) and recommends enabling the add-on (`azfunc-appinsights-enable`). |
| OCI | Functions | Duration only — `oci_faas` has `FunctionExecutionDuration` but no cold-start counter. | ✅ Duration-regression heuristic (P95 current vs 7-day baseline); **not cold-start-isolated** — a spike may be a cold start or a slow dependency (v0.89.232). |

## Error rate

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | Lambda | Yes — `AWS/Lambda` `Errors` + `Invocations` (Sum). | ✅ |
| GCP | Cloud Run / Functions | Yes — `request_count` (5xx) / `execution_count` (status != ok). | ✅ |
| Azure | Functions | **No** — no native per-function error metric (`FunctionErrors` does not exist). | 🏢 **Commercial-tier (implemented, gated — v0.89.307, #153)** — the error-rate detector re-points to **Application Insights** `requests/failed` over `requests/count` when `commercial_detectors.enabled=true`. OSS default off: queries `FunctionErrors` (empty → never fires) and recommends enabling the add-on (`azfunc-appinsights-enable`). |
| OCI | Functions | Yes — `oci_faas` `FunctionResponseCount` (error responses) over `FunctionInvocationCount` (fixed v0.89.229). | ✅ |

## Poison-message rate

| Cloud | Surface | Native metric? | Status |
|-------|---------|----------------|--------|
| AWS | SQS | **No** native poison RATE — but DLQ DEPTH is available. | ✅ **Depth/presence (v0.89.259, #156):** the source queue reads its DLQ's current `ApproximateNumberOfMessages` (free from the scan's attribute walk; same-account/region DLQs) and surfaces `poison_dlq_depth` + `poison_dlq_nonempty`. A non-empty DLQ ⇒ poison present. Proxy, not a rate: a drained DLQ reads empty. |
| GCP | Cloud Tasks | Yes — failed `task_attempt_count`. | ✅ |
| Azure | Service Bus | Yes — `DeadletteredMessages` gauge delta. | ✅ (delta approximation). |
| OCI | Queue | **No** Monitoring metric — verified the `oci_queue` namespace has no dead-letter metric, and `deadLetterQueueDeliveryCount` is a config threshold, not a poison count. | ✅ **Depth/presence (v0.89.305, #159):** the data-plane GetStats call (`{messagesEndpoint}/20210201/queues/{id}/stats`) returns `dlqStats.visibleMessages`; the scanner surfaces `poison_dlq_depth` + `poison_dlq_nonempty`, mirroring AWS #156. Best-effort & nil-tolerant: no DLQ configured / unknown messagesEndpoint / failed call ⇒ `-1` absent sentinel, never a fabricated zero. Proxy, not a rate: a drained DLQ reads empty. |

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

## Enabling commercial-tier detection

The Lambda Insights / Application Insights regression detectors ship **off**.
To turn them on (commercial tier), set in your Squadron config:

```yaml
commercial_detectors:
  enabled: true
```

With the flag on, every discovery scan runs the cold-start + error-rate
detectors against the add-on telemetry and annotates the serverless inventory
rows (Cold-start P95, Error rate). The flag is the *only* switch — there is no
per-resource toggle. Default off preserves the OSS posture exactly (the
detectors never run, no extra cloud calls).

**Prerequisites the operator must provide** — Squadron reads telemetry, it does
not provision it:

| Cloud | Add-on (paid) | Squadron RBAC needed |
|-------|---------------|----------------------|
| AWS Lambda | **Lambda Insights** extension enabled per function (`CloudWatchLambdaInsightsExecutionRolePolicy` on the function role). | `cloudwatch:GetMetricStatistics` — already in the connect-account scan role; it is namespace-agnostic, so it covers the `LambdaInsights` namespace with no change. |
| Azure Functions | **Application Insights** linked to the Function App (`APPLICATIONINSIGHTS_CONNECTION_STRING`). | `Microsoft.Insights/metrics/read` (already used) **plus the new** `Microsoft.Insights/components/read` (subscription-scope component LIST, to resolve the component the metrics live on). Both are covered by the built-in **Reader** / **Monitoring Reader** roles; operators on a narrow custom role must add `Microsoft.Insights/components/read`. |

**Cost / latency note:** with the flag on, each scan issues extra metric reads
per serverless resource (AWS: 2 cold-start + 4 error-rate CloudWatch
`GetMetricStatistics` calls per Lambda; Azure: the same shape against
Application Insights, plus one `Microsoft.Insights/components` LIST per scan).
CloudWatch `GetMetricStatistics` is billed per request; Azure Monitor metric
reads are free. Functions with no add-on enabled safe-degrade (no datapoints,
no annotation) — never a scan failure.

## From detection to recommendation

A fired regression detector no longer dead-ends as an inventory cell. For **AWS
Lambda**, the discovery recommendations flow now turns a detected cold-start
latency regression or error-rate spike into an actionable, merge-ready
recommendation — the same envelope the LLM-proposed instrumentation steps use,
carrying a deterministic Terraform snippet (provisioned-concurrency for
cold-start; a memory/concurrency bump for error-rate) that opens as a PR through
the normal IaC flow. These deterministic recommendations are:

- **Additive + best-effort** — they are appended alongside the LLM recs and
  never block them; a store miss or build error is logged and skipped.
- **Naturally gated by data availability** — a cold-start rec only appears once
  a prior scan persisted the cold-start observation (i.e. the commercial-tier
  detector ran against Lambda Insights). The error-rate rec is reconstructed
  from the persisted `error_rate_observation` rows and re-gated through the same
  thresholds as live detection (rate ratio > 2.0x AND ≥ 1000 invocations AND
  ≥ 50 errors in the 24h window), so the threshold logic lives in exactly one
  place — no drift between live detection and the recommendation path.
- **Exclusion-aware** — an operator-excluded regression rec stays excluded, by
  the same verdict-learning machinery the rest of the discovery recs honor.

GCP, Azure, and OCI surface the regression **annotations** on their inventory
rows today; turning those into recommendations is a tracked follow-up (it
depends on those clouds' scan-response wire shape carrying the serverless rows,
which AWS already does).
