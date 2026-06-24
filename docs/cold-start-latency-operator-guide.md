# Cold-start latency — operator guide

This is the operator-facing runbook for the v0.89.112 through
v0.89.116 cold-start latency analysis arc. Squadron now
correlates CloudWatch's `AWS/Lambda InitDuration` metric
against a rolling 7-day baseline to flag cold-start latency
regressions on AWS Lambda functions.

The strategic frame: Squadron previously operated on two
dimensions — **presence** (is the cloud-native primitive
enabled?) and **correctness** (does the configured primitive
preserve trace context end-to-end?). Cold-start latency
analysis introduces a third dimension: **measurement** of
latency outliers against expected baselines. Squadron's
universal claim gains a fifth verb: "MEASURES."

For a first test, the walkthrough takes about 25 minutes —
most of it spent confirming the AWS connection has the
additional CloudWatch read permission AND letting Squadron
accumulate the 7 days of baseline data before recommendations
start firing.

## What this is good for

- A team running production AWS Lambda functions and wanting
  to catch cold-start regressions before users see them.
- An SRE team that has cobbled together CloudWatch dashboards
  to track per-function P95 InitDuration and is tired of
  switching between Squadron and CloudWatch.
- A platform team migrating Lambda functions between
  architectures (x86_64 → arm64) wanting to flag any
  regressions during the migration window.
- An auditor who needs to verify "every Lambda has a documented
  cold-start baseline" and wants a single screen showing the
  current vs. baseline P95.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of cold-start latency analysis is
intentionally narrow:

- **GCP Cloud Run / Cloud Functions cold-start is slice 2+.**
  Each cloud's metric API has its own auth + pagination +
  aggregation shape; designing the substrate generically
  requires having all four implementations. Slice 1 ships
  AWS only; slice 2 generalizes after the second
  implementation.
- **Azure Functions cold-start is slice 2+.**
- **OCI Functions cold-start is slice 2+.**
- **Real-time metric streaming.** Squadron stays a discovery
  + correlation surface, not a metrics pipeline (Datadog /
  Honeycomb / Grafana Cloud space). The slice 1 substrate
  polls metrics once per scan window; there's no continuous
  streaming.
- **Sampling rate analysis.** Same substrate, different
  detection rules. Slice 2 / 3 candidate.
- **Error rate correlation.** Same substrate. Slice 3+.
- **Per-language cold-start tuning.** Slice 1 reports the P95
  outlier; the recommendation lists three common causes but
  does NOT detect which cause applies. Slice 3 may add
  per-language fingerprinting.
- **Provisioned concurrency cost analysis.** The recommendation
  suggests provisioned concurrency as one possible fix; it
  does NOT calculate the cost implication. Operators
  evaluate the pricing impact themselves before merging.
- **Auto-fix.** Squadron remains a recommender.

## The detection rule

Per Lambda function, per scan:

1. Squadron queries CloudWatch GetMetricStatistics for the
   `AWS/Lambda InitDuration` metric, filtered by the
   function's `FunctionName` dimension.
2. The query covers two windows: a 24-hour current window and
   a 168-hour (7-day) baseline window.
3. Both windows return the P95 statistic computed at
   5-minute granularity, then aggregated across the window
   by taking the maximum of the per-period P95 values.
4. The recommendation fires when ALL THREE conditions hold:
   - **Ratio condition**: `current_p95 / baseline_p95 >= 1.5`
   - **Floor condition**: `current_p95 >= 500ms` (absolute)
   - **Baseline confidence**: the 7-day baseline window has
     at least 50 sample data points

### Why 1.5x ratio?

CloudWatch's `InitDuration` carries genuine variance from the
cold-start path (init-time package loading, dependency
network reachability, runtime variance). A 1.5x ratio is the
threshold above which the variance is unlikely to be
statistical noise. The 7-day rolling baseline smooths
week-over-week trends so a Monday-vs-Sunday traffic shift
doesn't trigger false positives.

### Why the 500ms floor?

A function with baseline_p95 = 200ms that suddenly shows
current_p95 = 320ms hits the 1.6x ratio. But 320ms is still
fast cold-start — fingering the operator about a 120ms
absolute increase wastes attention. The 500ms floor filters
out naturally-low cold-start functions hitting ratio
thresholds on small absolute numbers.

### Why 50 sample minimum?

A function that's been live for less than 1 day doesn't have
a meaningful 7-day baseline. Slice 1 skips detection rather
than firing noisy recommendations against new functions.
Wait for a week of traffic before expecting the
recommendation to evaluate.

### Why P95 and not P99 or P50?

P99 is too noisy at typical Lambda throughputs — a single
slow init dominates the percentile. P50 misses the
operator-facing problem because user-visible timeouts come
from the long tail, not the median. P95 is the standard SRE
compromise.

## The three failure modes

Following the recurring Squadron pattern, the
`lambda-cold-start-baseline` recommendation acknowledges three
possible causes. The PR drafts the most common (provisioned
concurrency); the operator's review decides whether to merge
or decline.

### Case 1 — Init script regression

The most common cause for a sudden cold-start regression: a
recent deployment added heavy imports, eager-init connections,
or other startup work that wasn't there before.

How to recognize:
- Look at the function's deployment timeline (CodeDeploy /
  Terraform plan history). If the regression onset roughly
  aligns with a deployment, this is your case.
- The recommendation's PR is the WRONG fix here.

What to do: revert the deployment, identify the heavy import
or eager-init code path, fix the regression at the application
layer. Decline the Squadron recommendation with the note
"init script regression, fix in app layer." The verdict
learning loop records the decline.

### Case 2 — Cold-start frequency increase

A reduction in invocation rate means more invocations hit the
cold path. The function's intrinsic cold-start hasn't
regressed; the operator just sees more cold starts because
fewer warm executions are happening.

How to recognize:
- Compare the function's invocation rate over the same 24h /
  7d window. If invocations dropped significantly, this is
  your case.
- The function's cold-start P95 itself may be unchanged; the
  weighted average ratio just shifted because more invocations
  are in the cold-path bucket.

What to do: the recommendation's PR (provisioned concurrency)
is the RIGHT fix here. Tune the value based on expected
traffic — start with 1 and observe.

### Case 3 — Architecture change

A migration between architectures (x86_64 → arm64) or a
runtime update can shift cold-start behavior in either
direction. Some functions get faster on arm64; some get
slower depending on dependencies.

How to recognize:
- Check the function's current architecture vs. its previous
  scan history. If the architecture changed and the
  cold-start regressed, this is your case.

What to do: the recommendation's PR doesn't address the
underlying cause. Either accept the new baseline (decline
the recommendation and let the 7-day baseline catch up to
the new architecture) OR revert the architecture change.

## The Cold-start P95 column

The DiscoveryAWS Serverless inventory now has a new column
"Cold-start P95 (24h)" between "OTel distro" and "Last seen":

| Column            | Source                                |
|-------------------|---------------------------------------|
| Resource Name     | function name                         |
| Surface           | lambda                                |
| Runtime           | python3.11 / nodejs20.x / etc.        |
| Region            | function's region                     |
| Trace axis        | existing                              |
| OTel distro       | existing                              |
| Cold-start P95    | NEW — 24h P95 in ms; "—" if no data   |
| Last seen         | existing                              |
| Quality           | existing                              |

The cell renders in amber when `exceeds_threshold` is true
(both ratio + floor conditions met). Hover shows the baseline
value + ratio for context.

DiscoveryGCP / DiscoveryAzure / DiscoveryOCI Serverless tables
render the column as "—" everywhere — slice 1 has no data for
those clouds.

## The per-resource cold_start endpoint

```
GET /api/v1/discovery/aws/inventory/serverless/{id}/cold_start
```

Returns:

```json
{
  "resource_arn": "arn:aws:lambda:us-east-1:123456789012:function:order-processor",
  "current_window": {
    "window_hours": 24,
    "p95_ms": 4230,
    "sample_count": 142,
    "observed_at": "2026-06-25T14:00:00Z"
  },
  "baseline_window": {
    "window_hours": 168,
    "p95_ms": 2820,
    "sample_count": 1086,
    "observed_at": "2026-06-25T14:00:00Z"
  },
  "ratio": 1.5,
  "exceeds_threshold": true,
  "exceeds_floor_ms": true
}
```

Returns 404 when no cold_start_observation rows exist for the
resource (function never scanned, or function is too new).

## The cost surface

This catches first-time operators.

CloudWatch GetMetricStatistics is charged per request after
the first 1 million requests/month. For a fleet of 1000
Lambda functions scanned every 24 hours with 2 windows per
function (24h + 7d), the math is:

```
1000 functions × 2 windows × 1 scan/day × 30 days
  = 60,000 requests/month
```

That's well under the 1M free tier. For larger fleets:

```
10,000 functions × 2 × 30 = 600K requests/month — still free
50,000 functions × 2 × 30 = 3M requests/month
  - first 1M: free
  - remaining 2M: 2M × $0.01/1K = $20.00/month
```

Operators with very large fleets may want to disable the
substrate via the configuration toggle (slice 2 candidate).
Per the no-money brief: **Squadron does NOT make any
purchase decisions on the operator's behalf.** The operator
chooses whether to enable the substrate based on this cost
analysis.

## The seasonal traffic shift caveat

A function whose baseline is 200ms during business hours and
800ms overnight (cold-starts happen during the low-traffic
window) will have a 4x ratio at 14:00 vs. 03:00 baseline.

Slice 1's 7-day rolling baseline smooths weekly cycles (Monday
vs. Saturday) but NOT within-day cycles (14:00 vs. 03:00).
Operators may see false positives during overnight traffic
dips.

What to do: click "Don't propose this again" on the
recommendation. The exclusion table from #531 slice 2 chunk 4
records the suppression. The verdict learning loop logs the
decline reason for future scans.

Slice 2 may add time-of-day-aware baseline windows.

## Workflow — first cold-start scan

1. Open the AWS Discovery page (`/discovery/aws`). Note the
   existing connection.
2. **IAM upgrade**: if the connection was created before
   v0.89.114, the IAM policy needs
   `cloudwatch:GetMetricStatistics` added. The in-product
   IAM upgrade flow (#590) shows the diff.
3. Click "Run scan". The default tier list runs all 6 tiers
   plus the new cold-start substrate. The scan walks Lambda
   functions, queries CloudWatch per function (10 RPS rate
   limited), persists observations to the
   cold_start_observation table.
4. **Wait 7 days.** The recommendation logic requires a 7-day
   baseline. New functions and new Squadron deployments
   won't see recommendations until the baseline accumulates.
5. After 7 days, click the Serverless inventory section.
   Functions with Cold-start P95 in amber are exceeding the
   threshold.
6. Click the function name → opens the per-resource cold_start
   endpoint detail.
7. Open the Recommendations tab. Any function exceeding the
   threshold has a `lambda-cold-start-baseline` recommendation.
8. Review the 3-failure-mode reasoning. Pick yours. Merge
   the PR (case 2) or decline with note (case 1 or 3).

## Reading the audit

Slice 1 reuses the existing audit event types — no new
constants. The discovery scan emits the existing
`discovery.aws.scan_completed` event with a `cold_start_count`
field included in the payload showing how many Lambda
functions had cold-start observations recorded.

The recommendation lifecycle carries the new
`lambda-cold-start-baseline` kind through the existing
`recommendation.*` events.

## Troubleshooting

- **Cold-start P95 shows "—" for all my Lambda functions.**
  Check the IAM policy — `cloudwatch:GetMetricStatistics` is
  required. The scan audit may show a partial reason
  indicating CloudWatch was unreachable. If the IAM is
  correct, the substrate may be disabled by configuration
  (slice 2 candidate); check the audit for explicit disable.
- **Cold-start P95 shows a value but no recommendation
  fires.** Three possible reasons:
  1. The baseline P95 is below 50 samples (function too new).
  2. The current P95 doesn't exceed 500ms (below the floor).
  3. The current P95 doesn't exceed 1.5x baseline.
  The per-resource cold_start endpoint shows all three values
  so you can verify which condition is missing.
- **Recommendation fires but I deployed yesterday and the
  regression is from my deployment.** This is case 1 from
  the three failure modes. Decline the recommendation with
  "init script regression" and address the regression at
  the application layer. The 7-day baseline will eventually
  catch up to the new baseline (or you'll revert the
  deployment).
- **Cold-start observations are stored but the Cold-start P95
  column shows "—".** The inventory handler queries the
  cold_start_observation table at request time for the
  latest 24h observation. If the most recent observation
  is older than 1 day, the field may be stale. Re-run the
  scan to refresh.
- **CloudWatch billing alert triggered.** Check the request
  count math in §"The cost surface" above. For very large
  fleets you may want to disable the substrate. Slice 2 will
  add a per-connection configuration toggle.
- **Function with provisioned concurrency configured still
  shows high cold-start P95.** Provisioned concurrency only
  warms a configured floor of concurrent executions; if
  invocations exceed the floor, the excess hits cold path.
  Tune the floor based on your actual peak concurrency.

## What slice 2 will add

Per §13 of the design doc:

- GCP Cloud Run cold-start latency (Cloud Monitoring V3 +
  request_latencies metric).
- GCP Cloud Functions cold-start latency.
- Azure Functions cold-start (Application Insights Live
  Metrics + customMetrics).
- OCI Functions cold-start (OCI Monitoring + functions
  metric namespace).
- Sampling rate analysis using the same MetricQuerier
  substrate (closes the span quality slice 1 §13 sampling
  rate deferral).
- Error rate correlation.
- Retention policy for cold_start_observation table.
- Per-language fingerprinting (Python vs Node.js vs Java
  cold-start typical ranges).
- Per-deployment-event baseline reset (when a function is
  re-deployed, reset the 7-day baseline window).
- Time-of-day-aware baseline windows (suppress overnight
  false positives).
- Per-connection configuration toggle for the substrate
  (operators with cost concerns can disable).

## The universal claim grows a fifth verb

After cold-start latency slice 1, Squadron's positioning
reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, MEASURES cold-start latency
> against expected baselines, AND drafts the IaC PRs that
> close the gaps it finds.

**Five verbs.** Four clouds. Six tiers. One control plane.

The honest qualification: MEASURES is 1-cloud (AWS Lambda)
in slice 1; grows to 4-cloud through slice 2 and slice 3 as
the substrate generalizes. The substrate work — the
`MetricQuerier` interface, the rate limiter, the
cold_start_observation storage — is what makes future arcs
cheap. Once the substrate is in place, sampling rate analysis
(span quality slice 1 §13 deferral) becomes a small
detection-logic arc rather than substrate rebuilding.

The Tuesday LinkedIn drumbeat narrative gains a new
diagnostic dimension. Previous arcs answered "is the trace
present?" and "is the trace correct?" Cold-start latency
analysis adds: "is the latency reasonable?" An operator who
has spent the last hour digging through CloudWatch
dashboards to find a regressed Lambda can now see the
regression in Squadron's Serverless section on the next scan,
with the recommendation pre-drafted.

## Cross-references

- [Cold-start latency slice 1 design doc](./proposals/cold-start-latency-slice1.md) —
  the locked spec this runbook operationalizes.
- [Serverless tier slice 1](./proposals/serverless-tier-slice1.md) —
  the tier whose Lambda inventory rows this arc extends.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the trace integration arc this composes with.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  the span quality arc whose §13 sampling-rate deferral
  will reuse this substrate.
- [Audit log](./audit-log.md) — full catalog of event types.
