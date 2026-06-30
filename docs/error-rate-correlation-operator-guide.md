# Error rate correlation — operator guide

This is the operator-facing runbook for the v0.89.126 through
v0.89.130 error rate correlation arc. Squadron now compares
each serverless resource's current 24h error rate against its
rolling 7-day baseline to flag error rate spikes that need
operator attention.

The strategic frame: this is the THIRD diagnostic running on
the cold-start latency substrate (v0.89.113 + v0.89.118). The
substrate has now paid for itself three times over —
cold-start latency, sampling rate, error rate, all sitting on
the same `MetricQuerier` interface with shared rate limiters
and storage patterns. The architectural bet that the
substrate compounds is proven.

Together with cold-start latency (latency) and sampling rate
(throughput), error rate completes the natural serverless
health diagnostic suite. Operators get three independent
signals on the same Discovery pages, surfacing whichever
dimension is regressed without having to switch between
cloud-native dashboards.

## What this is good for

- A team running production serverless functions and wanting
  to catch error rate regressions soon after deploys, before
  user-facing impact.
- An SRE team whose error budget burns get triggered by
  spikes in cloud-native monitors, who wants Squadron to
  draft the resource-tuning PR for the throttle/exhaustion
  failure mode.
- A platform team migrating between SDK versions, runtime
  versions, or container architectures who wants visibility
  into per-function error rate drift.
- An auditor who needs to verify "every Lambda has a
  documented error-rate baseline" and wants a single
  per-resource view of current vs baseline.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of error rate correlation is
intentionally narrow:

- **Compute / database / kubernetes error rate is slice 2+.**
  These tiers don't have a clean cloud-native error metric
  that correlates to per-resource ownership. EC2 doesn't
  have an instance-level error metric Squadron can query;
  the equivalent is application-level which Squadron
  doesn't see today.
- **Per-error-type analysis.** Slice 1 reports total error
  count. It does NOT break down by error category (4xx vs
  5xx, exception type, timeout vs throw, retryable vs
  non-retryable). Slice 2 candidate.
- **Error span content inspection.** Slice 1 reads
  cloud-native error counters. It does NOT inspect
  individual span events or exception messages. Slice 3+
  for PII concerns.
- **Deploy-correlated error spike detection.** If errors
  spike at deploy time, Squadron's baseline (7-day window)
  smooths over the spike slowly. Slice 2 may add
  deploy-event correlation when Squadron ingests deployment
  events.
- **Cascading error analysis.** When function A's errors
  cause function B to error, slice 1 sees both
  independently. Caller/callee correlation is slice 2+.
- **Per-language error fingerprinting.** Squadron does NOT
  guess which language SDK is in use. Operator picks the
  failure mode from the 3-failure-mode reasoning.
- **Top-level "Workload health" dashboard panel.** Slice 1
  ships per-row columns. A consolidated cold-start +
  sampling + error rate health panel is slice 2.
- **Auto-fix.** Squadron remains a recommender.

## The detection rule

Per serverless resource, per scan:

1. Squadron queries the cloud-native error count metric for
   the resource over the last 24h (`current_error_count`).
2. Squadron queries the invocation count over the same 24h
   window (`current_invocation_count`) — reused from
   sampling rate slice 1's substrate.
3. Squadron queries the baseline 168h (7-day) error count
   AND invocation count.
4. Computes:
   - `current_error_rate = current_error_count / current_invocation_count`
   - `baseline_error_rate = baseline_error_count / baseline_invocation_count`
5. Applies the **near-zero baseline guard**: when
   `baseline_error_rate < 0.0001` (0.01%), use 0.0001 as the
   comparison baseline. This avoids divide-by-zero spurious
   ratios on functions with essentially zero baseline error
   rate.
6. The recommendation fires when ALL of:
   - `current_error_rate / comparison_baseline > 2.0`
   - `current_invocation_count >= 1000`
   - `current_error_count >= 50`
   - Not on exclusion list

### Why 2.0x ratio?

Error rates are inherently noisier than latency metrics. A
healthy function may have a baseline of 0.5% that
occasionally spikes to 1% during deploys or backend issues
without being a regression. A 2.0x ratio catches genuine
sustained drift (1% → 2%, 0.3% → 0.7%) while filtering
day-to-day noise.

Cold-start latency uses 1.5x because latency is more stable.
Error rate gets the looser threshold.

### Why 1000 invocation minimum?

A function invoked 50 times in 24h with 2 errors gives a 4%
rate — looks aggressive but is statistical noise. 1000
invocations corresponds to roughly 40/hour sustained — a
meaningful traffic level where percentages are reliable.

### Why 50 absolute error count minimum?

A function with baseline of 1 error/day that has 3 errors
today shows a 3x ratio — but the absolute count is so low
that the ratio is statistical noise. The 50-error floor
ensures recommendations fire only when the error count is
large enough to be operationally meaningful.

50 errors in 24h corresponds to roughly 2/hour sustained —
a real signal worth surfacing.

### Why the near-zero baseline guard?

Without the guard, a function with `baseline_error_rate =
0.0001%` (essentially zero) and `current_error_rate = 0.5%`
would show a 5000x ratio — meaningless because the baseline
is at the noise floor.

The guard substitutes 0.01% as the comparison baseline when
the actual baseline is below it. The per-resource endpoint
exposes a `baseline_adjusted` flag so operators can see
when the guard kicked in.

### Why baseline 7d window?

Mirrors cold-start latency for consistency. Long enough to
smooth weekly cycles (Monday vs Saturday); short enough that
gradual rollouts can shift the baseline within a quarter.

## The five serverless surfaces — per-cloud error metrics

The substrate from cold-start + sampling rate already wired
per-cloud `MetricQuerier`. Slice 1 of error rate adds ONE
NEW METRIC NAME per cloud:

> **⚠️ Detection coverage correction (v0.89.231).** Azure Monitor has no native
> per-function error metric — `FunctionErrors` below does not exist, so Azure
> error-rate requires Application Insights. The OCI row is corrected to the real
> `oci_faas` metric. See [detection-coverage.md](./detection-coverage.md).

| Cloud | Surface         | Error metric                                              |
|-------|-----------------|-----------------------------------------------------------|
| AWS   | Lambda          | `AWS/Lambda Errors` (Sum)                                 |
| GCP   | Cloud Run       | `run.googleapis.com/request_count` filtered by `response_code_class = "5xx"` |
| GCP   | Cloud Functions | `cloudfunctions.googleapis.com/function/execution_count` filtered by `status != "ok"` |
| Azure | Functions       | **none native** — needs Application Insights (`FunctionErrors` does not exist in Azure Monitor) |
| OCI   | Functions       | `FunctionResponseCount` (`oci_faas` error responses; fixed v0.89.229) |

All five reuse the existing cold-start / sampling rate IAM —
no new permissions. The substrate's rate limiters absorb the
new queries:

- Cold-start: 2 queries per resource per scan (24h + 7d)
- Sampling rate: 1 query (24h invocation count)
- Error rate: 2 queries (24h error count + 7d error count)
- **Total: 5 queries per resource per scan**

For a 1000-function fleet scanned every 24h:
- AWS: 5000 queries / 24h = ~0.06 RPS — trivial
- GCP: 5000 queries / 24h = ~0.06 RPM — trivial
- Azure / OCI: similar

All well under per-cloud rate limits.

## The three failure modes

Following the recurring Squadron pattern, the
`span-quality-error-rate-spike` recommendation acknowledges
three possible causes. The PR drafts the resource-exhaustion
case (case 3) ONLY. Cases (1) and (2) are explicitly
documented as decline paths in the reasoning text.

### Case 1 — Recent deploy regression

The most common cause: an application deploy introduced a
bug that's failing at the application layer.

How to recognize:
- Check the function's deployment timeline. If errors
  started after a deploy, this is your case.
- Look at the function's CloudWatch / Cloud Monitoring /
  Application Insights / OCI logs for the specific exception
  pattern — application stack traces tell you which
  deployment broke what.

What to do: **decline the Squadron PR.** Revert the deploy,
or fix the regression at the application layer. The
Terraform PR does NOT fix application bugs. Add a decline
note like "deploy regression — reverting #123."

The verdict learning loop records the decline.

### Case 2 — Downstream dependency failure

The second most common cause: the function calls a database
/ API / queue that's failing, and errors propagate.

How to recognize:
- Check the downstream's error rate or availability.
- Look at the function's error pattern — connection refused,
  timeout, 503 from downstream often surface as Lambda /
  Functions errors.
- If multiple functions calling the same downstream are
  spiking simultaneously, this is almost certainly your
  case.

What to do: **decline the Squadron PR.** Fix the downstream
first. Add a decline note like "downstream X is failing —
investigating with team Y."

### Case 3 — Resource exhaustion under load

The case Squadron's Terraform PR targets: throttling, memory
pressure, connection pool exhaustion. The function's
application logic is fine but it's running out of resources
under the current load.

How to recognize:
- Check the function's memory utilization metric — if hitting
  the configured limit consistently, this is your case.
- Check throttling counts (per-cloud: Lambda throttles, Cloud
  Run instance limits, Azure quotas, OCI invocation limits).
- Look for OOMKilled / memory exhaustion / connection refused
  patterns.

What to do: **merge the Squadron PR.** It raises memory +
concurrency limits to give the function headroom. Tune the
specific values based on your traffic profile.

## Per-cloud Terraform patterns (case 3 only)

### AWS Lambda

```hcl
resource "aws_lambda_function" "<name>" {
  memory_size                    = 1024  # operator tunes (was: lower)
  reserved_concurrent_executions = 100   # operator tunes
}
```

The PR draft picks default raise values; operators tune
based on observed peak.

### GCP Cloud Run

```hcl
resource "google_cloud_run_service" "<name>" {
  template {
    spec {
      container_concurrency = 80
      containers {
        resources {
          limits = {
            memory = "1Gi"
          }
        }
      }
    }
  }
}
```

### GCP Cloud Functions

```hcl
resource "google_cloudfunctions2_function" "<name>" {
  service_config {
    available_memory = "1Gi"
  }
}
```

### Azure Functions

```hcl
# Premium Plan tier bump (EP1 → EP2) absorbs error-rate load
# by providing more compute headroom.
resource "azurerm_service_plan" "<name>" {
  sku_name = "EP2"  # was EP1
}
```

Operators on Consumption tier should consider migrating to
Premium for predictable behavior under load.

### OCI Functions

```hcl
resource "oci_functions_function" "<name>" {
  memory_in_mbs = 1024  # operator tunes
}
```

## The per-resource error_rate endpoint

```
GET /api/v1/discovery/{provider}/inventory/serverless/{id}/error_rate
```

Returns:

```json
{
  "resource_arn": "arn:aws:lambda:us-east-1:...",
  "current_window": {
    "window_hours": 24,
    "error_count": 87,
    "invocation_count": 3200,
    "error_rate": 0.02719,
    "observed_at": "..."
  },
  "baseline_window": {
    "window_hours": 168,
    "error_count": 192,
    "invocation_count": 22400,
    "error_rate": 0.00857,
    "observed_at": "..."
  },
  "rate_ratio": 3.17,
  "baseline_adjusted": false,
  "exceeds_rate_ratio_floor": true,
  "exceeds_minimum_invocations": true,
  "exceeds_minimum_errors": true,
  "would_fire_recommendation": true
}
```

The three underlying gate flags surface separately so
consumers can distinguish "all three gates met (fires)"
from "two gates met + one short (does NOT fire)." The
`baseline_adjusted` flag surfaces when the near-zero
baseline guard kicked in.

## The per-Serverless-row Error rate (24h) column

Each DiscoveryX Serverless table now has an "Error rate
(24h)" column between "Sampling rate (24h)" and "Last seen":

| Column            | Source                                |
|-------------------|---------------------------------------|
| Resource Name     | function name / service name          |
| Surface           | lambda / cloudrun / etc.              |
| Runtime           | python3.11 / nodejs20.x / etc.        |
| Region            | resource region                       |
| Trace axis        | existing                              |
| OTel distro       | existing                              |
| Cold-start P95    | existing (slice 2 of cold-start)      |
| Sampling rate     | existing (slice 1 of sampling rate)   |
| Error rate        | NEW — current 24h error rate; amber when all gates met |
| Last seen         | existing                              |
| Quality           | existing                              |

Hover shows the underlying error count + invocation count +
baseline comparison.

The dashboard SPAN QUALITY panel does NOT add an error rate
column. Error rate is workload-health, not span-quality —
slice 2 may add a top-level "Workload health" panel
summarizing cold-start + sampling + error rate together.

## Workflow — first error rate scan

1. Open the AWS Discovery page (`/discovery/aws`). Note the
   existing connection.
2. **No IAM upgrade required** — the cold-start arc's
   permissions already cover the new error metrics.
3. Click "Run scan". The scan walks serverless functions,
   queries the error count alongside cold-start + invocation
   metrics, persists observations to the new
   `error_rate_observation` table.
4. **Wait 7 days.** The recommendation logic requires a 7-day
   baseline. New functions and new Squadron deployments
   won't see recommendations until the baseline accumulates.
5. After 7 days, click the Serverless inventory section.
   Functions with Error rate in amber are exceeding all
   three gates.
6. Click the function name → opens the per-resource
   error_rate endpoint detail.
7. Open the Recommendations tab. Any function exceeding the
   gates has a `span-quality-error-rate-spike` recommendation.
8. **Read the 3-failure-mode reasoning carefully.** Cases (1)
   and (2) are the MORE COMMON causes; decline the PR and
   investigate. Case (3) is what the PR targets — merge if
   resource exhaustion is the actual issue.

## Reading the audit

Slice 1 reuses the existing audit event types — no new
constants. The discovery scan emits the existing
`discovery.{provider}.scan_completed` event with an
`error_rate_observations_count` field included in the payload.

The recommendation lifecycle carries the new
`span-quality-error-rate-spike` kind through the existing
`recommendation.*` events.

SIEM consumers can filter:

```
recommendation_kind = "span-quality-error-rate-spike"
```

## Troubleshooting

- **Error rate column shows "—" for all my functions.** Two
  possible causes:
  1. The scan hasn't run since v0.89.129 (chunk 3 wired the
     scan integration). Re-run the scan.
  2. The IAM didn't have `cloudwatch:GetMetricStatistics`
     when the scan ran. Check the scan audit for a partial
     reason.
- **Error rate shows a value but no recommendation fires.**
  Three possible reasons (corresponding to the three gates):
  1. The rate ratio is below 2.0x.
  2. The current invocation count is below 1000.
  3. The current error count is below 50.
  The per-resource error_rate endpoint shows all three flags
  so you can verify which gate held.
- **The rate_ratio looks huge but the recommendation
  doesn't fire.** The near-zero baseline guard may have
  pegged the comparison baseline at 0.01%. Check the
  `baseline_adjusted` flag in the per-resource response.
  If true, the absolute error count is what to focus on,
  not the ratio.
- **The recommendation fires and I deployed yesterday — is
  this case 1?** Almost certainly yes. Decline the PR and
  fix the deploy regression at the application layer.
- **Error rate spike on multiple functions calling the same
  database.** Almost certainly case 2 (downstream
  dependency). Investigate the database; decline the PRs.
- **I run chaos engineering tests that intentionally spike
  errors.** Use the exclusion table (#531 slice 2 chunk 4)
  to suppress future recommendations during the test window.
- **Per-cloud rate limit hit on the metric API.** The
  substrate's existing rate limiter throttles to 10 RPS on
  AWS / 60 RPM on GCP / 12000 RPH on Azure / 10 TPS on OCI.
  For very large fleets (10K+ functions), scans may take
  longer. Slice 2 may add per-connection rate-limit tuning.

## Per-cloud rate limits + cost surface — substrate absorbs

The substrate's rate limiters from cold-start slice 1+2 +
sampling rate slice 1 absorb the new query:

| Cloud | Limit         | Total per-resource queries per scan      |
|-------|---------------|------------------------------------------|
| AWS   | 10 RPS        | 5 (cold-start 2 + sampling 1 + error 2) |
| GCP   | 60 RPM        | 5                                        |
| Azure | 12000 RPH     | 5                                        |
| OCI   | 10 TPS        | 5                                        |

All well under per-cloud quotas for typical fleets.

**Cost surface.** Error rate adds 2 metric queries per
resource per scan, identical cost characteristics to
cold-start. Per slice 1+2 cost analysis: essentially $0 for
typical fleets. Per the no-money brief: documented BEFORE
operators enable.

## What slice 2 will add

Per §13 of the design doc:

- Compute / database / kubernetes error rate.
- Per-error-type analysis (4xx vs 5xx, exception type,
  timeout vs throw).
- Deploy-correlated error spike detection (when Squadron
  ingests deployment events).
- Cascading error analysis (caller/callee correlation).
- Per-language error fingerprinting.
- Top-level "Workload health" dashboard panel summarizing
  cold-start + sampling + error rate together.
- Time-of-day-aware error rate baselines.
- Per-deploy-event baseline reset.
- Recommendation Terraform patterns for cases (1) and (2)
  (currently only case 3 is drafted).
- Error rate by HTTP path / by trigger source (for Lambda
  with multiple event source mappings).

## Strategic frame — substrate compounds 3x

This is the THIRD diagnostic running on the cold-start
latency substrate. The architectural bet is now demonstrated
three ways:

| Slice                  | New substrate | New per-cloud metrics  | New detection branch |
|------------------------|---------------|------------------------|----------------------|
| Cold-start slice 1     | Substrate ✓   | 1 (Lambda InitDuration) | 1                    |
| Cold-start slice 2     | None          | 4 (per-cloud variants) | 0 (same branch)      |
| Sampling rate slice 1  | None          | 5 (invocation counts)  | 1                    |
| **Error rate slice 1** | **None**      | **5 (error counts)**   | **1**                |

After error rate slice 1, the substrate supports a complete
serverless health diagnostic suite:

- **Cold-start latency** — is the workload's startup
  performance regressed? (slice 1 + slice 2)
- **Sampling rate** — is enough of the traffic actually
  being observed? (slice 1)
- **Error rate** — is the workload failing at an unusual
  rate? (this arc)

Together, these answer the operator's "is this workload
healthy?" question with three independent signals. The
universal claim doesn't grow a new verb — MEASURES gains a
third sub-diagnostic.

The Tuesday LinkedIn drumbeat narrative gains another
specific answer:

> "Your Lambda's error rate jumped from 0.3% to 12% after
> yesterday's deploy. The cold-start P95 is healthy, the
> sampling rate is healthy, but the deploy regressed your
> application logic. Squadron's recommendation is to revert
> OR raise memory + concurrency to absorb the failure mode;
> pick based on the deployment diff."

The substrate has now paid for itself three times over.
Every subsequent metric-correlation diagnostic Squadron
ships gets the same compounding return.

## Interpreting Azure (Application Insights) counts

When the commercial tier sources the error rate from Application Insights
(`requests/count` + `requests/failed`), the **absolute** invocation and error
counts reflect App Insights' own ingestion sampling / `itemCount` weighting —
they can be substantially larger than the raw request volume. This is expected
App Insights behaviour, not a Squadron miscount: Squadron sums one per-bucket
timeseries (5-minute `Total` aggregation) over the window, with no dimension
split, so there is no double-counting on our side.

The detection keys off the **error rate** (`failed / count`), which is
sampling-invariant — numerator and denominator are weighted identically, so the
ratio is unaffected. The absolute count only feeds the conservative volume floor
(≥1000 invocations) that suppresses noisy low-traffic functions; sampling
inflation there only makes the floor easier to clear, and the ratio floor
(≥2.0×) still guards against false positives. Read the amber Error-rate cell as
"the rate moved," not "exactly N requests." (Observed during the v0.89.311 live
verification: 95 injected requests surfaced as ~16k counted invocations with the
26% failure ratio preserved exactly.)

## Cross-references

- [Error rate correlation slice 1 design doc](./proposals/error-rate-correlation-slice1.md) —
  the locked spec this runbook operationalizes.
- [Cold-start latency operator guide](./cold-start-latency-operator-guide.md) —
  the substrate arc this reuses.
- [Sampling rate analysis operator guide](./sampling-rate-operator-guide.md) —
  the second substrate diagnostic this composes with.
- [Span quality operator guide](./span-quality-operator-guide.md) —
  the recommendation kind prefix this reuses
  (`span-quality-error-rate-spike`).
- [Serverless tier slice 1](./proposals/serverless-tier-slice1.md) —
  the inventory rows this annotates.
- [Audit log](./audit-log.md) — full catalog of event types.
