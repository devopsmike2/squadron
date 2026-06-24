# Error rate correlation slice 1 — substrate's third diagnostic

**Status:** design doc, locked for slice 1 implementation.
Third diagnostic running on the cold-start latency substrate
(v0.89.113 + v0.89.118). Completes the natural serverless
diagnostic suite: **latency** (cold-start) + **throughput**
(sampling rate) + **error rate** (this arc).

**See also:**
[Cold-start latency slice 1](./cold-start-latency-slice1.md),
[Cold-start latency slice 2](./cold-start-latency-slice2.md),
[Sampling rate analysis slice 1](./sampling-rate-analysis-slice1.md),
[Span quality slice 1](./span-quality-slice1.md),
[Serverless tier slice 1](./serverless-tier-slice1.md).

## 1. Problem

After cold-start latency slices 1+2 and sampling rate slice 1,
Squadron measures two of the three classic serverless health
dimensions:

- **Cold-start latency** — is the workload's startup
  performance regressed?
- **Sampling rate** — is enough of the traffic actually being
  observed?

The third dimension is **error rate**. Operators care about
this independently of the other two:

> A Lambda function with healthy cold-start P95, healthy
> sampling rate, healthy traces — but the error rate jumped
> from 0.3% to 12% after yesterday's deploy. The traces show
> the expected mix of success and failure spans but the
> failure share is wrong. The dashboard doesn't surface
> this; operators have to manually chart Errors/Invocations
> in CloudWatch.

Squadron's existing surface area doesn't catch this:
- The traceindex sees spans but doesn't distinguish success
  from failure (the span status attribute is read but not
  correlated to cloud-native error rates).
- Span quality slice 1 + slice 2 detect span pathologies but
  not error count drift.
- Cold-start latency catches init regressions; sampling rate
  catches coverage drops. Neither catches "the function
  silently started failing at 12% after a deploy."

Slice 1 adds error rate correlation per resource for the 5
serverless surfaces:

```
current_error_rate = current_error_count / current_invocation_count
baseline_error_rate = baseline_error_count / baseline_invocation_count

fire when:
  current_error_rate > baseline_error_rate * RATE_RATIO_FLOOR (2.0)
  AND current_invocation_count >= MIN_INVOCATION_COUNT (1000)
  AND current_error_count >= MIN_ERROR_COUNT (50)
  AND not on exclusion list
```

The substrate from cold-start slice 1+2 already implements
per-cloud `MetricQuerier`. Error rate correlation adds one
new metric name per cloud (the error count) — `MetricQuerier`
stays stable. The invocation count metric from sampling rate
slice 1 (v0.89.122) is reused.

This is the THIRD substrate use. The architectural bet that
the substrate compounds is now demonstrated twice over.

## 2. Non-goals (slice 1)

- **Compute / database / kubernetes error rate.** These tiers
  don't have a clean cloud-native error metric that
  correlates cleanly to per-resource ownership. Slice 2
  candidate.
- **Per-error-type analysis.** Slice 1 reports total error
  count. It does NOT break down by error category (4xx vs
  5xx, exception type, timeout vs throw). Slice 2.
- **Error span content inspection.** Slice 1 reads
  cloud-native error counters. It does NOT inspect
  individual span events or exception messages. Slice 3+
  (PII concerns).
- **Deploy-correlated error spike detection.** If errors
  spike at deploy time, Squadron's baseline (7d window)
  smooths over it slowly. Slice 2 may add deploy-event
  correlation when Squadron ingests deployment events.
- **Cascading error analysis.** When function A's errors
  cause function B to error, slice 1 sees both
  independently. Slice 2+ may add caller/callee
  correlation.
- **Per-language error fingerprinting.** Same as cold-start
  / sampling rate — operator picks the failure mode in
  the 3-failure-mode reasoning.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection rule

For each serverless resource at scan time:

1. **Query current 24h error count.** Via `MetricQuerier`
   with per-cloud error metric (§4).
2. **Query current 24h invocation count.** Reuses sampling
   rate slice 1's invocation metric.
3. **Query baseline 168h (7d) error count.** Same metric,
   different window.
4. **Query baseline 168h invocation count.**
5. **Compute rates:**
   - `current_error_rate = current_error_count / current_invocation_count`
   - `baseline_error_rate = baseline_error_count / baseline_invocation_count`
6. **Fire when ALL conditions hold:**
   - `current_error_rate > baseline_error_rate * 2.0`
   - `current_invocation_count >= 1000` (noise filter)
   - `current_error_count >= 50` (absolute floor — avoid
     firing on 1-2 errors that happen to be 2x baseline of
     0.5 errors)
   - Not on exclusion list

### 3.1 Why 2.0x ratio?

Error rates are inherently noisier than latency metrics — a
healthy function may have a baseline rate of 0.5% that
occasionally spikes to 1% during deploys or backend issues
without being a regression. A 2.0x ratio catches genuine
sustained drift (1% → 2%, 0.3% → 0.7%) while filtering
day-to-day noise.

The cold-start arc used 1.5x because latency is more stable.
Error rate gets the looser threshold.

### 3.2 Why 50 absolute error count minimum?

A function with baseline of 1 error/day that has 3 errors
today shows a 3x ratio — but the absolute count is so low
that the ratio is statistical noise. The 50-error floor
ensures we only fire when the error count is large enough to
be operationally meaningful.

50 errors in 24h corresponds to roughly 2/hour sustained —
a real signal worth surfacing.

### 3.3 Why current_error_rate, not current_error_count alone?

A function whose error count grew 10x but whose invocation
count also grew 10x has the same RATE. That's traffic
growth, not regression. Comparing rates filters legitimate
scaling.

### 3.4 Why baseline 7d window?

Mirrors cold-start latency for consistency. Long enough to
smooth over weekly cycles (Monday vs Saturday); short
enough that gradual rollouts can shift the baseline within
a quarter.

## 4. Per-cloud error metrics

Each cloud has its native error count or error rate metric.
Slice 1 extends each cloud's `MetricQuerier.QueryAggregate`
with one new supported metric name.

### 4.1 AWS Lambda

Metric: `AWS/Lambda Errors` (Sum statistic). IAM unchanged
from cold-start/sampling rate (`cloudwatch:GetMetricStatistics`
covers).

### 4.2 GCP Cloud Run

Metric: `run.googleapis.com/request_count` filtered by
`response_code_class = "5xx"`. The same metric as sampling
rate uses for the denominator; the dimension filter
inverts. IAM unchanged.

### 4.3 GCP Cloud Functions

Metric: `cloudfunctions.googleapis.com/function/execution_count`
filtered by `status != "ok"`. Sibling of sampling rate's
`status = "ok"` filter. IAM unchanged.

### 4.4 Azure Functions

Metric: `FunctionErrors` (Sum aggregation). IAM unchanged
from cold-start slice 2.

### 4.5 OCI Functions

Metric: `function_invocation_count` filtered by
`result = "error"`. Same metric as sampling rate's
denominator; tag-filtered to error invocations. IAM
unchanged.

## 5. Storage

NO new schema migration. The existing
`cold_start_observation` table from slice 1 has the right
shape; slice 1 of error rate could reuse it. But for
clarity, slice 1 of error rate adds rows to a new
`error_rate_observation` table mirrored from
`cold_start_observation`.

Schema v14 → v15:

```sql
CREATE TABLE IF NOT EXISTS error_rate_observation (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    surface TEXT NOT NULL,
    account_id TEXT NOT NULL,
    region TEXT NOT NULL,
    resource_arn TEXT NOT NULL,
    observed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    window_hours INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    invocation_count INTEGER NOT NULL,
    error_rate REAL NOT NULL,
    snapshot_json TEXT NOT NULL,
    UNIQUE (connection_id, resource_arn, observed_at, window_hours)
);
CREATE INDEX IF NOT EXISTS idx_errorrate_resource ON error_rate_observation(resource_arn);
CREATE INDEX IF NOT EXISTS idx_errorrate_observed ON error_rate_observation(observed_at);
```

Migration idempotent.

## 6. API surface

### 6.1 Per-resource error rate endpoint

New `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/error_rate`:

```json
{
  "resource_arn": "...",
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
  "exceeds_rate_ratio_floor": true,
  "exceeds_minimum_invocations": true,
  "exceeds_minimum_errors": true,
  "would_fire_recommendation": true
}
```

### 6.2 Inventory endpoint extension

Existing per-cloud Serverless inventory rows gain
`current_error_rate` field (nullable when no observation).

## 7. UI

The SPAN QUALITY dashboard panel from v0.89.124 currently
has 6 columns. Adding error rate would push it to 7. The
brief's 1500-line soft cap means the UI work has tradeoffs.

Slice 1 of error rate: do NOT add error rate to the SPAN
QUALITY panel — error rate isn't a span-quality issue per
se, it's a workload-health issue. Instead:

- Add "Error rate (24h)" column on each cloud's Serverless
  inventory table, between "Sampling rate (24h)" and
  "Last seen". Amber when exceeds_rate_ratio_floor + minimums
  met.
- The per-Inventory-row QualityDot stays at 6 percentages
  (sampling, traceparent, etc.). Error rate stays on the
  per-row column.

Slice 2 may add a top-level "Workload health" panel
summarizing cold-start + sampling + error rate together.

## 8. Recommendation kinds

1 new kind:

```
span-quality-error-rate-spike
```

Reuses the existing `span-quality-` webhook prefix from
v0.89.86 — NO new webhook routing.

Reasoning template:

> "This serverless resource's 24-hour error rate is X% over
> N error events in M invocations. The 7-day baseline error
> rate is Y% (Z ratio). Squadron flags this when the ratio
> exceeds 2.0x AND current invocations >= 1000 AND current
> errors >= 50.
>
> Three common causes:
>
> 1. **Recent deploy regression.** Check the function's
>    deployment timeline. If errors started after a deploy,
>    revert or fix the regression at the application layer.
>    This Terraform PR does NOT fix application bugs;
>    decline if your cause is (1).
> 2. **Downstream dependency failure.** If the function
>    calls a database / API / queue that's failing, errors
>    propagate. Investigate the downstream first. Decline
>    if (2).
> 3. **Resource exhaustion under load.** Throttling, memory
>    pressure, connection pool exhaustion. The Terraform PR
>    raises memory + concurrency limits to give the function
>    headroom. Merge if (3).
>
> If your cause is (1) or (2), decline this PR — the verdict
> learning loop records."

Terraform pattern per cloud (slice 1 ships case (3) — the
resource-exhaustion mitigation):

- AWS Lambda: raise `memory_size` from current to current * 1.5,
  bump `reserved_concurrent_executions`.
- GCP Cloud Run: raise `resources.limits.memory` and
  `containerConcurrency`.
- GCP Cloud Functions: raise `service_config.available_memory`.
- Azure Functions: bump Premium Plan tier (EP1 → EP2).
- OCI Functions: raise `memory_in_mbs`.

The reasoning text explicitly notes that cases (1) + (2)
are the more common causes and should be declined.

## 9. Slice 1 contract

**In:**

1. Storage schema v14 → v15 with `error_rate_observation`
   table.
2. Each cloud's `MetricQuerier.QueryAggregate` extends to
   support the per-cloud error metric.
3. Detection logic per §3 in the proposer bridge per-cloud.
4. Per-resource error rate API endpoint.
5. Inventory endpoint extension with `current_error_rate`
   field per Serverless row.
6. Per-Serverless-row "Error rate (24h)" column on each
   cloud's table.
7. Proposer prompt extension with
   `span-quality-error-rate-spike` kind.
8. iacpicker per-cloud emitters for the resource-exhaustion
   Terraform pattern.
9. Operator runbook covering all the above.
10. Acceptance tests.

**Out:**

- Compute / database / kubernetes error rate.
- Per-error-type analysis (4xx vs 5xx, exception type).
- Error span content inspection.
- Deploy-correlated error spike detection.
- Cascading error analysis.
- Per-language error fingerprinting.
- Top-level "Workload health" dashboard panel.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation — storage migration + per-cloud
  error metric support.** ~900-1100 lines. v14→v15
  migration, store CRUD, per-cloud error metric routing in
  `MetricQuerier`. **v0.89.127.**
- **Chunk 2: Detection branch + per-resource endpoint +
  proposer prompt + iacpicker.** ~800-1000 lines.
  **v0.89.128.**
- **Chunk 3: UI per-Serverless-row Error rate column on all
  4 provider tables + inventory annotation.** ~600-800
  lines. **v0.89.129.**
- **Chunk 4: Operator runbook + README index.**
  ~300-400 lines. **v0.89.130.**

Total: 4 release tags. No parallel scanner fan-out —
substrate already exists per-cloud.

## 11. Acceptance tests

1. **AWS QueryAggregate supports AWS/Lambda Errors metric.**
2. **GCP QueryAggregate supports request_count{5xx}.**
3. **GCP QueryAggregate supports execution_count{status!=ok}.**
4. **Azure QueryAggregate supports FunctionErrors.**
5. **OCI QueryAggregate supports function_invocation_count{result=error}.**
6. **Detection — 3x ratio at 3000 invocations + 90 errors
   fires recommendation.**
7. **Detection — 1.9x ratio at 3000 invocations does NOT
   fire** (below ratio threshold).
8. **Detection — 3x ratio at 500 invocations does NOT fire**
   (below minimum invocations).
9. **Detection — 3x ratio at 3000 invocations + 30 errors
   does NOT fire** (below absolute error minimum).
10. **Storage migration v14 → v15 idempotent.**
11. **error_rate_observation rows persist + retrieve.**
12. **Per-resource error_rate endpoint returns shape per §6.1.**
13. **Inventory endpoint includes current_error_rate field
    on Serverless rows for all 4 providers.**
14. **UI Error rate column renders amber when exceeds
    threshold.**
15. **Cold-start parity preserved** — all 4 providers
    cold-start prompts byte-identical to v0.89.125 when no
    error rate rows trigger recommendations.

## 12. Threat model

**No new external surface.** Slice 1 extends each cloud's
existing `MetricQuerier` with one additional supported
metric name. Per-cloud IAM unchanged from
cold-start/sampling rate.

**Per-cloud rate limits absorb the new query.** Error rate
detection adds 2 metric queries per resource per scan
(current error count + baseline error count). Cold-start
substrate already runs 2 queries per resource (24h + 7d
cold-start). Sampling rate adds 1 query (invocation count
reused for both arcs).

Total per-resource queries per scan for the substrate:
- Cold-start: 2 (24h + 7d cold-start P95)
- Sampling rate: 1 (24h invocation count)
- Error rate: 2 (24h + 7d error count)
- **Total: 5 queries per resource per scan**

Per-cloud rate limits (AWS 10 RPS, GCP 60 RPM, Azure
12000 RPH, OCI 10 TPS) absorb 5 queries per resource per
scan comfortably for typical fleets.

For a 1000-function fleet scanned every 24h:
- AWS: 5000 queries / 24h = ~0.06 RPS — trivial
- GCP: 5000 queries / 24h = ~0.06 RPM — trivial
- Azure / OCI: similar

**Cost surface.** Error rate adds 2 metric queries per
resource per scan, identical cost characteristics to
cold-start latency. Cloud-native metric APIs are free for
typical fleets per slice 1+2 cost analysis.

**False positives on intentional error spikes.** Operators
running a chaos engineering test may intentionally spike
errors. Exclusion table + verdict learning loop handle.

**False positives on baseline-too-low cases.** A function
whose 7d baseline error rate is essentially 0 (no errors
in window) gets a divide-by-zero risk. The slice 1
implementation guards: if `baseline_error_rate <
1e-6`, use a floor value (0.0001 = 0.01%) as the comparison
baseline to avoid spurious large ratios on tiny absolute
counts.

**No span content logging.** Slice 1 logs aggregate values
(error count, invocation count, rate, ratio). Individual
error messages are NOT stored. PII surface stays at zero.

## 13. Slice 2 candidates

- Compute / database / kubernetes error rate.
- Per-error-type analysis (4xx vs 5xx, exception type
  breakdowns).
- Deploy-correlated error spike detection (when Squadron
  ingests deployment events).
- Cascading error analysis (caller/callee correlation).
- Per-language error fingerprinting.
- Top-level "Workload health" dashboard panel summarizing
  cold-start + sampling + error rate together.
- Time-of-day-aware error rate baselines.
- Per-deploy-event baseline reset.
- Recommendation Terraform patterns for cases (1) and (2)
  (currently only case (3) is drafted).
- Error rate by HTTP path / by trigger source (for Lambda
  with multiple event source mappings).

---

**Strategic frame:**

This is the THIRD diagnostic running on the cold-start
latency substrate. The architectural bet is now
demonstrated three ways:

| Slice              | New substrate | New per-cloud metrics  | New detection branch |
|--------------------|---------------|------------------------|----------------------|
| Cold-start slice 1 | Substrate ✓   | 1 (Lambda InitDuration) | 1                    |
| Cold-start slice 2 | None          | 4 (per-cloud variants) | 0 (same branch)      |
| Sampling rate s1   | None          | 5 (invocation counts)  | 1                    |
| Error rate s1      | None          | 5 (error counts)       | 1                    |

After error rate slice 1, the substrate supports a
complete serverless health diagnostic suite:

- **Cold-start latency** — is the workload's startup
  performance regressed? (slice 1 + slice 2)
- **Sampling rate** — is enough of the traffic actually
  being observed? (slice 1)
- **Error rate** — is the workload failing at an unusual
  rate? (this arc)

Together, these answer the operator's "is this workload
healthy?" question with three independent signals. The
universal claim doesn't grow a new verb — MEASURES gains
a third sub-diagnostic.

The Tuesday LinkedIn drumbeat narrative gains another
specific answer: "your Lambda's error rate jumped from
0.3% to 12% after yesterday's deploy. The cold-start P95
is healthy, the sampling rate is healthy, but the
deploy regressed your application logic. Squadron's
recommendation is to revert OR raise memory + concurrency
to absorb the failure mode; pick based on the deployment
diff."

After slice 1 of error rate, the substrate has paid for
itself three times over. The architectural bet pays out
on every subsequent metric-correlation diagnostic
Squadron ships.
