# Cold-start latency analysis slice 1 — metrics correlation substrate

**Status:** design doc, locked for slice 1 implementation.
First arc on a NEW substrate dimension (metric correlation)
alongside the existing presence + correctness dimensions
Squadron already operates on. Grows the universal claim by a
fifth verb: "MEASURES."

**See also:**
[Serverless tier slice 1](./serverless-tier-slice1.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Span quality slice 1](./span-quality-slice1.md),
[Span quality slice 2](./span-quality-slice2.md).

## 1. Problem

Squadron's existing surface area operates on two dimensions:

- **Presence:** does the cloud-native primitive exist? (e.g.
  X-Ray enabled on a Lambda; Pub/Sub topic has tracingConfig;
  Service Bus namespace has diagnostic settings).
- **Correctness:** does the configured primitive actually
  preserve trace context end-to-end? (e.g. EventBridge rule
  InputPath doesn't strip headers; traceparent attribute is
  well-formed W3C).

These two dimensions catch the canonical "broken trace"
failures. They do not catch a third operationally important
class: **latency outliers**. The canonical example:

> A Lambda function with X-Ray Active tracing, with the ADOT
> layer attached, with no orphan spans, with valid traceparent
> headers — but cold-start P95 sitting at 4.2 seconds when
> the team's expectation is sub-300ms. Every request hitting
> a cold start times out at the API Gateway 30s default
> timeout. The trace looks healthy in the dashboard; the
> user-facing experience is broken.

Squadron currently sees the function as healthy. The operator
has to leave Squadron, go to CloudWatch, manually query
duration metrics, manually filter by `InitDuration`, manually
compare against expected baseline. None of that is in
Squadron's surface today.

Slice 1 introduces the metric correlation substrate that will
later support cold-start latency analysis, sampling rate
analysis (span quality slice 1 §13 deferral), and other
metric-driven diagnostics. The substrate is deliberately
narrow:

1. **Per-cloud metric query interface.** A single Go
   interface `MetricQuerier` with one method:
   `QueryAggregate(ctx, resourceARN, metricName, window, stat)`.
2. **AWS-only implementation in slice 1.** CloudWatch
   GetMetricStatistics for the AWS Lambda case. GCP / Azure /
   OCI implementations are honest slice 2 / 3 deferrals.
3. **One metric type:** Lambda `InitDuration` from
   AWS/Lambda namespace. No general-purpose metric routing in
   slice 1.
4. **Baseline storage.** A new `cold_start_observation` table
   captures the rolling 7-day P95 per Lambda function, used
   as the comparison baseline.
5. **Detection rule.** Current-window P95 > baseline P95 ×
   1.5 → fire `lambda-cold-start-baseline`.

Slice 2 extends to GCP Cloud Run cold-start, Azure Functions
cold-start, OCI Functions cold-start. Slice 3 may extend
to sampling rate analysis or error rate correlation.

## 2. Non-goals (slice 1)

- **GCP / Azure / OCI cold-start coverage.** Honest slice 2
  deferral. Each cloud's metric API has its own auth /
  pagination / aggregation shape; designing the substrate
  generically requires having all four implementations to
  generalize against. Slice 1 ships AWS-only; slice 2
  generalizes after seeing the second implementation.
- **Real-time metric streaming.** Squadron stays a discovery
  + correlation surface, not a metrics pipeline (Datadog /
  Honeycomb / Grafana Cloud space). The slice 1 substrate
  polls metrics on each scan window; no continuous
  streaming.
- **Sampling rate analysis.** Same substrate, different
  recommendation kinds and detection rules. Slice 2 / 3
  candidate; the slice 1 substrate is designed to support
  it but doesn't ship the detection logic.
- **Error rate correlation.** Same substrate. Slice 3+.
- **Per-language cold-start tuning.** Slice 1 reports the
  P95 outlier; the recommendation suggests three common
  causes (provisioned concurrency, ARM-X86 migration, init
  script optimization). It does NOT detect which cause
  applies to a specific function — slice 3+ may add per-language
  fingerprinting.
- **Provisioned concurrency cost analysis.** The
  recommendation may suggest provisioned concurrency as
  the fix; it does NOT calculate the cost implication.
  Operators check pricing themselves. (Per the no-money brief.)
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection rule

Per Lambda function in the existing serverless inventory:

1. **Query the rolling 24h window P95.** CloudWatch
   GetMetricStatistics for `AWS/Lambda InitDuration` metric,
   filtered by `FunctionName` dimension, last 24h, P95
   statistic, 5-minute period (gives ~288 data points).
2. **Query the rolling 7d baseline.** Same query for the
   prior 7 days, P95.
3. **Compare.** If 24h_p95 > 7d_baseline_p95 × 1.5 AND
   24h_p95 > 500ms (absolute floor), fire
   `lambda-cold-start-baseline`.

The absolute floor (500ms) avoids fingering well-tuned
functions with naturally low cold-start that happen to hit a
1.6x ratio (e.g. baseline=200ms, current=320ms — that's a
ratio jump but not actually slow).

The detection runs per-scan (which is how Squadron picks up
all other tier signals). It does NOT run continuously.
Operators see the recommendation on the next discovery scan
after the cold-start regression starts.

### 3.1 Why 1.5x ratio?

CloudWatch's `InitDuration` carries genuine variance from
cold-start path (init-time package loading, network reachability
to dependencies, etc.). A 1.5x ratio is the threshold above
which the variance is unlikely to be statistical noise. The
7-day baseline window smooths week-over-week trends.

Operators can tune this — slice 1 ships the threshold as a
single Go constant. Slice 2 may make it per-recommendation
configurable.

### 3.2 Why P95 and not P99 or P50?

P99 is too noisy at typical Lambda throughputs (a single
slow init dominates). P50 misses the operator-facing problem
(the long tail is what causes user-visible timeouts). P95
is the standard SRE compromise.

## 4. Storage schema

New `cold_start_observation` table. Schema migration v13 → v14.

```sql
CREATE TABLE IF NOT EXISTS cold_start_observation (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    provider TEXT NOT NULL, -- "aws" in slice 1
    surface TEXT NOT NULL, -- "lambda" in slice 1
    account_id TEXT NOT NULL,
    region TEXT NOT NULL,
    resource_arn TEXT NOT NULL,
    observed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    window_hours INTEGER NOT NULL, -- 24 or 168 (7d)
    p95_ms REAL NOT NULL,
    sample_count INTEGER NOT NULL,
    snapshot_json TEXT NOT NULL,
    UNIQUE (connection_id, resource_arn, observed_at, window_hours)
);
CREATE INDEX IF NOT EXISTS idx_coldstart_resource ON cold_start_observation(resource_arn);
CREATE INDEX IF NOT EXISTS idx_coldstart_observed ON cold_start_observation(observed_at);
```

Schema bumps to v14. Migration idempotent.

The table grows linearly with (resource count × scans per
day). For a fleet of 1000 Lambda functions scanned every 24h,
that's 1000 rows per day (2 windows × 500 functions average
that had cold starts) ≈ 365K rows per year. Squadron's
discovery store already handles larger tables (scan_run
history). Slice 2 may add a retention policy.

## 5. Scanner contract

New `internal/discovery/scanner/metrics.go` introduces the
`MetricQuerier` interface:

```go
package scanner

type MetricStatistic string

const (
    StatisticP95     MetricStatistic = "p95"
    StatisticP99     MetricStatistic = "p99"
    StatisticAverage MetricStatistic = "average"
    StatisticSum     MetricStatistic = "sum"
)

type AggregateMetricResult struct {
    ResourceARN string
    MetricName  string
    Window      time.Duration
    Statistic   MetricStatistic
    Value       float64
    Unit        string
    SampleCount int
    ObservedAt  time.Time
}

// MetricQuerier returns aggregate metric values for a
// specific resource. Per-cloud implementations live in
// scanner/{aws,gcp,azure,oci}/metrics.go.
//
// Slice 1 ships only the AWS implementation for the Lambda
// InitDuration metric. Future slices add more clouds + metric
// types; the interface stays stable.
type MetricQuerier interface {
    QueryAggregate(ctx context.Context, resourceARN, metricName string, window time.Duration, stat MetricStatistic) (AggregateMetricResult, error)
}
```

The AWS implementation `internal/discovery/aws/metrics.go`
wraps `cloudwatch.GetMetricStatistics`. It MUST handle:

- Authentication via the existing AWS scanner credentials.
- Pagination (CloudWatch caps at 1440 data points per call;
  5-minute period over 24h = 288 points, well under).
- Empty result sets (function never invoked → return
  Value=0, SampleCount=0, no error).
- API throttling (CloudWatch GetMetricStatistics has a
  shared per-account TPS limit — slice 1 rate-limits to
  10 RPS per account; slice 2 may make this configurable).

## 6. API surface

### 6.1 Per-resource cold-start endpoint

New `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/cold_start`:

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

### 6.2 Inventory endpoint extension

The existing `GET /api/v1/discovery/aws/inventory` response
gains a `cold_start_p95_ms` field per Lambda row in the
serverless array (when observed) — sourced from the latest
observation in cold_start_observation. NULL when no
observation exists yet.

### 6.3 Trace coverage endpoint extension

No new fields. Cold-start is a separate diagnostic dimension
from coverage; lives in the new endpoint above.

## 7. UI

The existing Serverless sub-tab on DiscoveryAWS gains a new
column "Cold-start P95 (24h)" between "OTel distro" and
"Last seen":

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

The cell color is amber when exceeds_threshold is true.
Hover shows the baseline value + ratio.

No new top-level dashboard panel in slice 1. Slice 2 may
add a "LATENCY OUTLIERS" panel once cross-cloud coverage
exists.

## 8. Recommendation kinds

1 new kind:

```
lambda-cold-start-baseline
```

Reuses the existing `lambda-` webhook prefix from v0.89.92
(serverless tier chunk 5) — NO new webhook routing.

Reasoning template:

> "This Lambda function's 24-hour P95 cold-start duration is
> {value}ms, {ratio}x its 7-day baseline of {baseline}ms.
> Squadron flags this when the ratio exceeds 1.5x AND the
> absolute value exceeds 500ms. Three common causes:
>
> 1. **Init script regression.** A recent deployment added
>    new heavy imports / startup work. Compare the function's
>    deployment timeline to the regression onset.
> 2. **Cold-start frequency increase.** A reduction in invocation
>    rate means more invocations hit cold path. Consider
>    provisioned concurrency for predictable traffic.
> 3. **Architecture change.** Migration between architectures
>    (e.g. x86_64 → arm64) or runtime updates can shift
>    cold-start behavior.
>
> This Terraform PR drafts a baseline provisioned concurrency
> configuration. If your case is (1), decline and trace the
> regression in your deployment history; if (3), decline and
> evaluate the architecture intentionally."

The Terraform pattern:

```hcl
resource "aws_lambda_provisioned_concurrency_config" "<name>" {
  function_name                     = aws_lambda_function.<name>.function_name
  provisioned_concurrent_executions = 1  # operator tunes
  qualifier                         = aws_lambda_function.<name>.version
}
```

## 9. Slice 1 contract

**In:**

1. Storage schema v13 → v14 with `cold_start_observation` table.
2. `MetricQuerier` interface in scanner package.
3. AWS `MetricQuerier` implementation wrapping
   CloudWatch GetMetricStatistics.
4. CloudWatch rate-limited to 10 RPS per AWS account.
5. Cold-start observation flow: scan polls 24h + 7d windows
   per Lambda inventory row.
6. Per-resource cold_start API endpoint.
7. Inventory endpoint extension with cold_start_p95_ms field.
8. UI Serverless sub-tab Cold-start P95 column with hover
   tooltip.
9. Proposer prompt extension with `lambda-cold-start-baseline`
   kind.
10. Operator runbook covering all the above.
11. Acceptance tests covering the detection ratio + floor
    edges, the API shape, the UI column, cold-start parity.

**Out:**

- GCP / Azure / OCI cold-start (slice 2).
- Sampling rate analysis (slice 2 / 3).
- Error rate correlation (slice 3+).
- Per-language cold-start fingerprinting.
- Provisioned concurrency cost analysis.
- Real-time metric streaming.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Substrate foundation + storage + MetricQuerier
  interface.** ~900-1100 lines. Storage migration v13→v14,
  MetricQuerier interface in scanner package, scanner
  package tests. AWS impl SKELETON ONLY (returns
  not-implemented). **v0.89.113.**
- **Chunk 2: AWS CloudWatch implementation + Lambda
  cold-start detection branch.** ~900-1100 lines. AWS
  CloudWatch GetMetricStatistics wiring, rate limiter,
  scanner extension to populate cold_start_observation,
  per-resource endpoint. **v0.89.114.**
- **Chunk 3: Proposer prompt + UI Cold-start P95 column +
  inventory field extension.** ~800-1000 lines.
  **v0.89.115.**
- **Chunk 4: Operator runbook + README index.**
  ~300-400 lines. **v0.89.116.**

Total: 4 release tags. Slightly bigger than recent tier work
because the substrate is new ground; no parallel scanner
fan-out (slice 1 is AWS-only).

## 11. Acceptance tests

1. **MetricQuerier interface — slice 1 AWS impl returns
   AggregateMetricResult for InitDuration**.
2. **Empty CloudWatch response → Value=0, SampleCount=0,
   no error**.
3. **CloudWatch API throttle → backoff respected, request
   eventually succeeds**.
4. **Rate limiter caps at 10 RPS per AWS account** — pin
   with a test that issues 50 requests in 1 second and
   measures the time taken (should be ~5 seconds).
5. **Detection ratio at 1.5x exactly and floor at 500ms
   triggers recommendation** — boundary case.
6. **Detection ratio at 1.4x does NOT trigger** — below
   ratio threshold.
7. **Detection floor at 499ms does NOT trigger** — below
   absolute floor.
8. **Detection without baseline (new function < 7 days
   old)** — skips the comparison; no recommendation.
9. **Storage migration v13 → v14 idempotent**.
10. **cold_start_observation rows persist + retrieve**.
11. **Per-resource cold_start endpoint returns shape per
    §6.1**.
12. **Inventory endpoint includes cold_start_p95_ms field
    on Lambda rows**.
13. **UI Cold-start P95 column renders amber when
    exceeds_threshold**.
14. **UI Cold-start P95 column renders '—' when no data**.
15. **Cold-start parity preserved** — all 4 providers
    cold-start prompts byte-identical to v0.89.111 when no
    cold-start observations exist.

## 12. Threat model

**Wider IAM permissions.** Slice 1 adds
`cloudwatch:GetMetricStatistics` to the AWS scanner IAM
template. Read-only; sits on the existing IAM upgrade flow
(#590).

**CloudWatch GetMetricStatistics rate limits.** CloudWatch
GetMetricStatistics is rate-limited per AWS account at ~50
RPS (varies by account size). Slice 1's 10 RPS rate limit
keeps Squadron well under, even with multiple Squadron
instances scanning the same account. Slice 2 may add per-AWS-account
coordination if multi-tenant deployments emerge.

**Cost surface.** CloudWatch GetMetricStatistics is charged
per request after the first 1M/month. For a fleet of 1000
Lambda functions scanned 24x/day with 2 windows = 48K
requests/day = 1.44M requests/month. Charged at ~$0.01 per
1K requests = ~$14.40/month. Slice 1 documents this in the
runbook so operators know the cost impact before enabling.
Per the no-money brief: Squadron does NOT make any
purchase decisions; the operator chooses to enable.

**Cold-start observation table growth.** ~365K rows/year for
a 1000-function fleet. Slice 2 may add a retention policy
(e.g. delete observations older than 30 days). For slice 1,
the table grows monotonically; operators can manually clean
up via SQL if needed.

**False positives during seasonal traffic shifts.** A
function whose baseline is 200ms during business hours and
800ms overnight (because cold-starts happen at night) will
have a 4x ratio at 14:00 vs 03:00 baseline window. Slice 1
uses 7-day rolling baseline which smooths weekly cycles but
not within-day cycles. Operators see false positives
during overnight traffic dips. The exclusion table from
#531 slice 2 chunk 4 handles; verdict learning loop records.

**No span/metric content logging.** The substrate logs only
aggregate values (P95 ms, sample count). Individual
invocation timestamps are not stored. PII surface stays at
zero.

## 13. Slice 2 candidates

- GCP Cloud Run cold-start latency (Cloud Monitoring V3 +
  request_latencies metric).
- GCP Cloud Functions cold-start latency.
- Azure Functions cold-start (Application Insights
  Live Metrics + customMetrics).
- OCI Functions cold-start (OCI Monitoring + functions
  metric namespace).
- Sampling rate analysis using the same MetricQuerier
  substrate.
- Error rate correlation.
- Retention policy for cold_start_observation table.
- Per-language fingerprinting (Python vs Node.js vs Java
  cold-start typical ranges).
- Per-deployment-event baseline reset (when a function is
  re-deployed, reset the 7-day baseline window).
- Provisioned concurrency cost-aware recommendation tiers.

---

**Strategic frame:**

Squadron's universal claim grows a fifth verb:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, MEASURES cold-start latency
> against expected baselines, AND drafts the IaC PRs that
> close the gaps it finds.

Five verbs. Four clouds. Six tiers. One control plane.

The honest qualification: MEASURES is 1-cloud (AWS Lambda)
in slice 1, grows to 4-cloud through slice 2 and slice 3.
The substrate work is what makes the future arcs cheap:
once `MetricQuerier` is in place, sampling rate analysis
(span quality slice 1 §13 deferral) becomes a small arc
that adds detection logic on top of the substrate rather
than re-building it. Cold-start latency was picked first
because it has the cleanest signal-to-noise ratio for slice 1
substrate validation.

The Tuesday LinkedIn drumbeat narrative gains a new
diagnostic dimension. The existing arcs answered "is the
trace present?" and "is the trace correct?" Cold-start
latency analysis adds: "is the latency reasonable?" An
operator who has spent the last hour digging through
CloudWatch dashboards to find a regressed Lambda can now
see the regression in Squadron's Serverless sub-tab on
the next scan, with the recommendation pre-drafted.
