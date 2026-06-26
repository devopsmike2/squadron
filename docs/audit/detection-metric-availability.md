# Detection metric availability audit

Status: in progress (v0.89.229). Mirror of the proposer-snippet correctness
audit (v0.89.224–228), applied to the **detection** side: do Squadron's cloud
metric queries name metrics that actually exist on the provider's monitoring
API?

The cold-start, error-rate, and sampling-rate detectors were built
incrementally with mock-based unit tests. Mocks return canned datapoints for
whatever metric name the code asks for, so a wrong metric name passes every
test while returning **empty** against the real cloud — the detector silently
never fires. This is the detection-side analogue of the proposer's silent
no-ops.

## Findings

| Cloud | Metric (code) | Real metric? | Effect |
|-------|---------------|--------------|--------|
| AWS | `AWS/Lambda` → `InitDuration` (p95) | **No** — init duration is a CloudWatch **Logs** REPORT field, not a CloudWatch metric. Available only via Lambda Insights (`LambdaInsights` namespace) or a log-metric-filter. | Lambda cold-start detection never accumulates samples → never fires. |
| Azure | `FunctionInvocations` | **No** — the real invocation metric is `FunctionExecutionCount`. | Error-rate denominator empty. |
| Azure | `FunctionErrors` | **No** — no native per-function error metric; requires Application Insights (`requests/failed`). | Error-rate numerator empty. |
| Azure | `FunctionExecutionDuration` | **No** — no native per-function duration metric; requires Application Insights. | Cold-start detection never fires. |
| OCI | namespace `oci_functions`, `function_invocation_count`, `function_duration`, `function_invocation_count` + `result="error"` | **Misnamed** — real namespace is `oci_faas`; metrics are `FunctionInvocationCount`, `FunctionExecutionDuration`, `FunctionResponseCount` (error responses). No `result` dimension. | All OCI Functions detection queried a non-existent namespace. **Fixed v0.89.229.** |
| OCI | `cold_start_count` | **No** — `oci_faas` has no cold-start counter (only FunctionInvocationCount / FunctionExecutionDuration / FunctionResponseCount / AllocatedProvisionedConcurrency). | OCI cold-start gate unsatisfiable. |
| AWS | SQS poison-rate via DLQ `NumberOfMessagesSent` (SUM) | **Wrong metric** — redrive-moved messages (the poison) are not counted by `NumberOfMessagesSent`; only manual SendMessage is. Reported a confident 0/hr. **Reverted v0.89.229** to the honest absent sentinel; depth-based (`ApproximateNumberOfMessagesVisible`) detection is the planned fix. |
| GCP | `run.googleapis.com/request_latencies`, `.../execution_times`, `cloudtasks.googleapis.com/queue/{task_attempt_count,depth}` | **Yes** — valid Cloud Monitoring metric types. | OK. |
| AWS | `AWS/Lambda` → `Invocations`, `Errors`; `AWS/SQS` → `NumberOfMessagesSent` | **Yes** | OK. |

Sources: AWS Lambda metrics docs (InitDuration is a Logs field, not a metric);
Azure Functions monitoring data reference (`FunctionExecutionCount`,
`FunctionExecutionUnits` are the only native Function metrics); OCI Function
Metrics reference (`oci_faas` namespace, PascalCase metric names).

## Fixed in v0.89.229

OCI namespace + metric names corrected to the real `oci_faas` values, and the
error path switched from a synthetic `result="error"` tag on the invocation
counter to the real `FunctionResponseCount` metric. This un-breaks OCI
sampling-rate and error-rate detection.

## Open (needs a data-source decision — deferred, see follow-up tasks)

AWS Lambda cold-start and Azure Functions cold-start/error detection cannot run
on native CloudWatch / Azure Monitor metrics. Making them functional requires
choosing a data source, each with operator-facing setup + IAM implications:

- **AWS**: switch to Lambda Insights (`LambdaInsights` / `init_duration`),
  which requires the customer to enable Lambda Insights on the function; OR a
  CloudWatch Logs metric-filter on the REPORT line.
- **Azure**: switch to Application Insights (`requests`, `requests/failed`,
  `requests/duration`), which requires the Function App to have App Insights
  wired and queries a different API (App Insights, not Azure Monitor metrics).
- **OCI cold-start**: no cold-start counter exists; redesign to a
  `FunctionExecutionDuration` p95-regression heuristic (cannot isolate
  cold-start specifically) or drop the kind.

Because these change which detections function and what operators must enable,
they are deferred for an explicit decision rather than silently switching data
sources. The code comments for the affected constants point here.
