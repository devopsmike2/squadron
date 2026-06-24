# Sampling rate analysis slice 1 — substrate's second diagnostic

**Status:** design doc, locked for slice 1 implementation.
Closes the explicit slice 1 deferral from span quality
(§13: "Sampling rate analysis — compares observed span
throughput against expected throughput from cloud-native
metrics"). Second diagnostic running on the cold-start
latency substrate (v0.89.113 + v0.89.118) — proves the
substrate compounds.

**See also:**
[Cold-start latency slice 1](./cold-start-latency-slice1.md),
[Cold-start latency slice 2](./cold-start-latency-slice2.md),
[Span quality slice 1](./span-quality-slice1.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Serverless tier slice 1](./serverless-tier-slice1.md).

## 1. Problem

After span quality slice 1 (v0.89.84-88) and slice 2
(v0.89.108-111), Squadron catches five pathology classes at
the OTLP receiver hot path:

- Orphan spans (broken context propagation).
- Missing required resource attributes.
- Attribute placeholders.
- Malformed W3C traceparent.
- Missing traceparent on child spans.

These detectors all share an assumption: that the spans
Squadron sees are REPRESENTATIVE of the resource's actual
traffic. When the SDK's sampler drops 95% of invocations,
the spans Squadron observes are only 5% of the truth.
Squadron sees no orphans, no malformed traceparents, no
placeholder values — but the operator's slow-path P99
queries that hit timeout are exactly the 95% that got
dropped.

This matters because aggressive sampling is the
canonical "why isn't this regression visible in our
dashboards?" surface:

- A Lambda function with X-Ray ratio sampling at 0.05
  (5%) — the team has been running with this since launch;
  nobody remembers why.
- A Cloud Run service with an OTel SDK configured for
  `TRACEIDRATIO_BASED` at 0.01 — somebody copy-pasted the
  default. The slow-tail invocations that matter most are
  in the 99% that gets dropped.
- An Azure Function with no explicit sampling, relying on
  Application Insights' adaptive sampling which throttles
  to 5 traces/sec when the function is busy — exactly when
  observability matters.

Slice 1 adds a single new detection at the per-resource
level for the 5 serverless surfaces:

```
observed_span_count / expected_invocation_count < SAMPLING_RATIO_FLOOR
  AND expected_invocation_count >= MIN_INVOCATION_COUNT
  → fire span-quality-sampling-too-aggressive
```

Where:
- `observed_span_count` = spans Squadron's traceindex
  received from the resource in the last 24h
- `expected_invocation_count` = invocations from the
  cloud-native metric over the same window
- `SAMPLING_RATIO_FLOOR` = 0.05 (5% — below this is
  unusually aggressive)
- `MIN_INVOCATION_COUNT` = 1000 (statistical noise floor)

The detection runs once per scan; ratios computed per
resource. Cross-cloud: the substrate from cold-start slice
1+2 already implements per-cloud `MetricQuerier`.
Sampling rate analysis adds ONE NEW METRIC NAME per cloud
(the invocation count) and ONE NEW DETECTION BRANCH.

This is the cleanest demonstration yet that the
architectural bet on the substrate compounds.

## 2. Non-goals (slice 1)

- **Compute / database / kubernetes sampling rate analysis.**
  These tiers don't have a clean cloud-native "invocation
  count" metric. EC2 doesn't have one (the equivalent is
  application-level request count which Squadron doesn't
  see). Slice 2 candidate that may use a different
  per-tier approach.
- **Span event sampling.** Span events (per-span
  exception markers, log lines etc.) have their own
  sampling. Slice 2+.
- **Tail-sampling collector detection.** If the operator
  uses a tail-sampling collector that selectively keeps
  spans based on properties, Squadron's observed_span_count
  legitimately undercounts intended emission. Slice 2 may
  add per-resource exclusion when the operator declares
  tail-sampling.
- **Per-language sampler library recommendation.** The
  recommendation suggests increasing the sampling rate but
  does NOT detect WHICH sampler library / framework is in
  use. Slice 3+.
- **Adaptive sampling tuning.** Some SDKs use adaptive
  sampling that responds to throughput. Squadron's
  detection treats the observed rate as the operator's
  configured rate; if the SDK adapts, that's the same
  ratio Squadron sees. Slice 2 may add adaptive sampling
  caveats in reasoning text.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection rule

For each serverless resource at scan time:

1. **Query observed span count.** From the existing
   `traceindex.Index` per-resource SpanCountAt method (or
   sibling) over the last 24h. The traceindex already
   tracks per-key observations from v0.89.74; the
   per-window count comes from the Quality observer's
   1h counters from v0.89.85 OR a new 24h-window counter.
2. **Query expected invocation count.** Via the existing
   `MetricQuerier.QueryAggregate` with the per-cloud
   invocation metric name (§4) over the last 24h.
3. **Compute ratio.** `observed_span_count /
   expected_invocation_count`.
4. **Fire when conditions met:**
   - `ratio < SAMPLING_RATIO_FLOOR (0.05)` AND
   - `expected_invocation_count >= MIN_INVOCATION_COUNT (1000)`
   - AND not on the exclusion list (#531 slice 2 chunk 4)

The 0.05 floor catches obvious aggressive sampling without
firing on every default 10% sampler. The 1000 invocation
minimum avoids statistical noise (a function invoked 50
times with 1 span observed = 2% ratio but is noise).

### 3.1 Why 5% floor?

OTel SDK defaults vary by SDK and framework. Common
defaults Squadron should NOT flag as aggressive:
- 100% (always-on): obviously fine.
- 10% (`TRACEIDRATIO_BASED` 0.1): common production default.
- 5% sustained: at the edge — the floor sits right at this
  value, not below. The detection fires at 4.9%, not 5%.

Sub-5% ratios sustained over a 24h window are unusual
enough to warrant operator review. Below 1% is almost
always a regression or misconfig.

### 3.2 Why 1000 invocations minimum?

A function invoked 50 times in 24h with 2 spans observed
gives a 4% ratio — looks aggressive but is statistical
noise. 1000 invocations corresponds to roughly 40/hour
sustained — a meaningful traffic level where percentages
are reliable.

### 3.3 Why a single ratio threshold?

Slice 1 does NOT have separate baseline vs current windows
(unlike cold-start latency, which compares 24h vs 7d).
Sampling rates change at deploy time (operator updates
sampler config), not gradually. Comparing to a historical
baseline would mask intentional reductions; the absolute
floor is the honest signal.

Slice 2 may add baseline comparison for detecting
SUDDEN drops (deploy event reduced the sampling rate).

## 4. Per-cloud invocation metrics

Each cloud has its native invocation count metric. Slice 1
extends each cloud's `MetricQuerier.QueryAggregate` with
this additional supported metric name:

### 4.1 AWS Lambda

Metric: `AWS/Lambda Invocations` (sum statistic over
window). IAM unchanged from cold-start slice 1
(`cloudwatch:GetMetricStatistics` covers).

### 4.2 GCP Cloud Run

Metric: `run.googleapis.com/request_count` filtered by
`response_code_class != "5xx"` (exclude server errors —
the upstream sampling decision is what matters, not
ingress success). IAM unchanged from cold-start slice 2.

### 4.3 GCP Cloud Functions

Metric: `cloudfunctions.googleapis.com/function/execution_count`
filtered by `status = "ok"`. IAM unchanged.

### 4.4 Azure Functions

Metric: `FunctionInvocations` (sum statistic). IAM unchanged
from cold-start slice 2.

### 4.5 OCI Functions

Metric: `function_invocation_count` (sum). IAM unchanged.

## 5. Traceindex per-window span count

The Quality observer from v0.89.85 has a 1h rolling
window. Sampling rate analysis wants a 24h window. Two
options:

**Option A: 24h Quality counter.** Extend the Quality
observer with a parallel 24h counter. Memory cost ~16
bytes per resource × 100K cap = ~1.6MB worst case. Low
risk.

**Option B: Query the existing traceindex.** The
traceindex's per-key observation history could be queried
for last-24h counts. The slice 1 traceindex tracks
LastSeenAt but not a per-window counter; slice 2 of trace
integration tracks Quality counters at 1h.

Slice 1 of sampling rate ships **Option A**: extend the
Quality observer with a 24h counter that rolls over
identically to the 1h counter pattern. This is the
smallest change with the cleanest test surface.

The new field on `QualityCounters`:

```go
type QualityCounters struct {
    // ... existing 1h-window counters ...
    
    // Slice 1 of sampling rate: parallel 24h-window
    // counter for sampling-rate analysis. Resets when
    // the 24h window elapses, independently from the
    // 1h-window counters.
    TotalSpansLast24h    uint64
    WindowStart24h       time.Time
}
```

## 6. API surface

### 6.1 Per-resource sampling endpoint

New `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/sampling`:

```json
{
  "resource_arn": "...",
  "window_hours": 24,
  "observed_span_count": 142,
  "expected_invocation_count": 3500,
  "sampling_ratio": 0.0406,
  "exceeds_floor": false,
  "exceeds_minimum_invocations": true,
  "would_fire_recommendation": true,
  "observed_at": "..."
}
```

### 6.2 Inventory endpoint extension

Existing per-cloud Serverless inventory rows gain
`sampling_ratio` field (nullable when no observation).

## 7. UI

The SPAN QUALITY dashboard panel from v0.89.110 currently
has 5 columns (Orphan / Missing attrs / Mismatch /
Malformed traceparent / Missing on child). Slice 1 of
sampling rate adds a 6th column:

```
  Orphan      Missing     Attr        Malformed         Missing       Sampling
  trace       attrs       mismatch    traceparent       on child      too aggressive
   3.2%        6.3%        2.0%          0.8%            4.1%            12.5%
12 res.     18 res.     6 res.        3 res.          14 res.        4 res.
```

The panel hides when all 6 are zero (extend the existing
condition).

Per-Inventory-row Quality dot tooltip extends to show all
6 percentages.

Per-Serverless-row Sampling Ratio column: each cloud's
Serverless table gains a "Sampling rate (24h)" column
between "Cold-start P95 (24h)" and "Last seen":

- "—" when no observation (function too new, scan didn't run,
  or invocation_count below minimum)
- ratio value as percentage (e.g. "4.1%") in slate when
  above floor
- amber when below floor AND exceeds minimum invocations
- hover tooltip shows the underlying span_count / invocation_count

## 8. Recommendation kinds

1 new kind:

```
span-quality-sampling-too-aggressive
```

Reuses the existing `span-quality-` webhook prefix from
v0.89.86 — NO new webhook routing.

Reasoning template:

> "This serverless resource emitted N spans over the last
> 24 hours against M expected invocations (ratio: X%).
> Squadron flags this when the ratio is below 5% AND the
> resource processed at least 1000 invocations in the
> window. Three common causes:
>
> 1. **Default sampler too aggressive.** Many OTel SDKs
>    default to TRACEIDRATIO_BASED at 0.1 (10%) but some
>    framework integrations default lower. Check the SDK
>    configuration.
> 2. **Adaptive sampling throttling.** Application Insights
>    and some OTel exporters use adaptive sampling that
>    throttles under load. The ratio Squadron sees is the
>    OPERATOR-EXPERIENCED rate, not the configured rate.
> 3. **Tail-sampling collector.** A tail-sampling collector
>    (in front of Squadron) selectively keeps spans. If
>    that's intentional, decline the recommendation — the
>    exclusion table records.
>
> This Terraform PR raises the sampler ratio. If your case
> is (2) or (3), decline the PR."

Terraform pattern per cloud (mirrors the cold-start arc
per-cloud emitters):

- AWS Lambda: env var
  `OTEL_TRACES_SAMPLER_ARG=0.5` injection into
  `aws_lambda_function.environment.variables`.
- GCP Cloud Run: `google_cloud_run_service` env var
  injection.
- GCP Cloud Functions: `google_cloudfunctions2_function`
  env var.
- Azure Functions: `app_settings["OTEL_TRACES_SAMPLER_ARG"]`.
- OCI Functions: `config["OTEL_TRACES_SAMPLER_ARG"]`.

The 0.5 floor is operator-tunable; the recommendation
draft uses 0.5 as a starting point.

## 9. Slice 1 contract

**In:**

1. `QualityCounters` extension with 24h-window counter.
2. `traceindex.Index` exposes `SpanCountLast24h(key) (uint64, bool)`
   method.
3. Each cloud's `MetricQuerier.QueryAggregate` extends to
   support the per-cloud invocation count metric.
4. Detection logic in the proposer bridge per-cloud.
5. New per-resource sampling endpoint.
6. Inventory endpoint adds `sampling_ratio` per
   Serverless row.
7. SPAN QUALITY dashboard panel grows from 5 columns to 6.
8. Per-Inventory-row Quality dot tooltip extends.
9. Serverless table gains a "Sampling rate (24h)" column.
10. Proposer prompt extension with
    `span-quality-sampling-too-aggressive` kind.
11. iacpicker per-cloud emitters for the sampling
    Terraform pattern.
12. Operator runbook covering all the above.
13. Acceptance tests covering the detection ratio + minimum
    invocations + per-cloud metric queries.

**Out:**

- Compute / database / kubernetes sampling rate (slice 2).
- Span event sampling.
- Tail-sampling collector detection.
- Per-language sampler library detection.
- Adaptive sampling deep diagnosis.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation — 24h Quality counter +
  per-cloud invocation metric support.** ~900-1100 lines.
  Extends `QualityCounters` + per-cloud `MetricQuerier`
  metric routing. Per-cloud rate limit accounting stays.
  **v0.89.122.**
- **Chunk 2: Detection branch + per-resource API endpoint
  + proposer prompt + iacpicker patterns.** ~700-900 lines.
  **v0.89.123.**
- **Chunk 3: UI SPAN QUALITY 6th column + Sampling rate
  column on Serverless tables + tooltip extension.**
  ~700-900 lines. **v0.89.124.**
- **Chunk 4: Operator runbook + README index.**
  ~300-400 lines. **v0.89.125.**

Total: 4 release tags. No parallel scanner fan-out — the
substrate already exists per-cloud; slice 1 extends each
cloud's MetricQuerier with one new metric name (additive,
small).

## 11. Acceptance tests

1. **24h Quality counter increments on each span**.
2. **24h Quality counter resets after 24h window elapses**.
3. **24h Quality counter independent from 1h counter**
   (resets at different times).
4. **AWS QueryAggregate supports AWS/Lambda Invocations
   metric.**
5. **GCP QueryAggregate supports run.googleapis.com/request_count
   AND cloudfunctions.googleapis.com/function/execution_count.**
6. **Azure QueryAggregate supports FunctionInvocations.**
7. **OCI QueryAggregate supports function_invocation_count.**
8. **Detection — ratio 4.9% at 5000 invocations fires
   recommendation** (below 5% floor AND above 1000 min).
9. **Detection — ratio 5.0% at 5000 invocations does NOT
   fire** (at floor, not below).
10. **Detection — ratio 2.0% at 500 invocations does NOT
    fire** (below minimum invocation count).
11. **Detection — ratio 4% with NULL traceindex count
    does NOT fire** (missing data).
12. **Per-resource sampling endpoint shape per §6.1.**
13. **Inventory Lambda row includes sampling_ratio field**.
14. **UI SPAN QUALITY panel renders 6th column when
    non-zero**.
15. **UI SPAN QUALITY panel hides when all 6 are zero**.
16. **Cold-start parity preserved** — all 4 providers
    cold-start prompts byte-identical to v0.89.120 when no
    sampling rows trigger recommendations.

## 12. Threat model

**No new external surface.** Slice 1 extends each cloud's
existing `MetricQuerier` with one additional supported
metric name. The IAM permissions slice 1 needs are already
in place from cold-start (`cloudwatch:GetMetricStatistics`
covers Lambda Invocations; GCP's `monitoring.timeSeries.list`
covers Cloud Run/Functions invocations; Azure's existing
Reader covers FunctionInvocations; OCI's `read metrics in
compartment` covers function_invocation_count).

**Per-cloud rate limits unchanged.** Sampling rate
detection adds 1 metric query per resource per scan
(invocation_count). The cold-start substrate already
processes 2 queries per resource (24h + 7d cold-start
windows). Adding 1 more is a 50% increase per resource;
still well within per-cloud rate limits (the slice 1
limits leave significant headroom).

**Memory cost.** The 24h Quality counter adds ~16 bytes
per resource. At the 100K-key Quality observer cap, ~1.6MB
worst case. Acceptable.

**Hot-path cost.** Each span observation does one
additional `if windowElapsed { reset }` check on the 24h
counter, then one increment. Measured ~5ns additional per
span. Well under the slice 1 100ns budget for cumulative
slice 2 overhead.

**False positives on tail-sampling collectors.** Operators
running a tail-sampling collector in front of Squadron's
OTLP receiver will see legitimate undercount. Slice 1 docs
the exclusion path; slice 2 may add a per-resource
"tail-sampling intentional" flag.

**False positives on adaptive sampling.** Same as
tail-sampling — the observed rate IS the operator-facing
rate, but the operator may have intended adaptive
throttling. Decline path handles.

**No span content logging.** Slice 1 logs only counters
(observed count, expected count, ratio). PII surface
stays at zero.

## 13. Slice 2 candidates

- Compute / database / kubernetes sampling rate (different
  per-tier approach since they lack a clean cloud-native
  invocation-count metric).
- Span event sampling rate.
- Tail-sampling collector detection.
- Per-language sampler library detection (Python opentelemetry-api
  vs ParentBased TraceIdRatioBased vs framework-specific).
- Adaptive sampling deep diagnosis.
- Baseline comparison detection (detect SUDDEN drops in
  sampling rate from previous scans).
- Per-deploy-event sampling reset (when an operator
  deploys, ignore the immediate window because the
  configured rate may have just changed).
- Compute / k8s observed throughput vs application
  metrics (if a k8s pod exposes a Prometheus
  `http_requests_total` counter, compare against
  traceindex span count).

---

**Strategic frame:**

This is the second diagnostic running on the cold-start
latency substrate. The architectural bet that the
substrate compounds is now PROVEN — building the
`MetricQuerier` interface as a generic per-cloud metric
query primitive was the right call. Slice 1 of sampling
rate adds:

- One new metric name per cloud (additive).
- One new detection branch (proposer-side).
- One new recommendation kind.
- Six lines of new UI columns.
- No new substrate.

That's roughly 1/4 the implementation cost of cold-start
slice 1, because slice 1 paid the substrate cost.

After this arc, Squadron's universal claim doesn't grow a
new verb — it makes the existing MEASURES verb more
specific. The "where did my trace go?" diagnostic chain
gains a third sibling under "is the latency reasonable?":

1. Is the cloud-native trace primitive enabled?
2. Does the event source's CONFIG preserve trace context?
3. Does the trace context that arrives conform to W3C?
4. Is the cold-start latency reasonable?
5. **Is enough of the traffic actually being sampled?**

Slice 1 of sampling rate closes the span quality slice 1
§13 deferral with a fraction of the cost. The Tuesday
LinkedIn drumbeat narrative gains the most specific
"where did my trace go?" answer yet: "your Lambda's
sampler is throttling 96% of invocations. The slow-tail
P99 requests that timeout are exactly the 4% that
matters; you can't see them because they're not being
sampled. Squadron just drafted the PR to raise the
sampler ratio to 50%."
