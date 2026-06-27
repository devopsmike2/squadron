# Sampling rate analysis — operator guide

This is the operator-facing runbook for the v0.89.121 through
v0.89.125 sampling rate analysis arc. Squadron now compares
the observed span count from its traceindex against the
expected invocation count from each cloud's native metric API
to detect serverless resources whose sampler is too
aggressive.

The strategic frame: this is the SECOND diagnostic running
on the cold-start latency substrate (v0.89.113 + v0.89.118).
The architectural bet that the substrate compounds is now
proven — building the `MetricQuerier` interface as a generic
per-cloud metric query primitive was the right call. Slice 1
of sampling rate adds one new metric name per cloud, one
new detection branch, one new recommendation kind, six lines
of new UI columns, and no new substrate. That's roughly 1/4
the implementation cost of cold-start slice 1, because slice 1
paid the substrate cost.

For a first test, the walkthrough takes about 15 minutes —
most of it spent confirming the cloud connections already
have the metric read permissions from the cold-start arc.

## What this is good for

- A team running production serverless functions and
  suspecting their default 10% sampler is dropping the slow
  tail invocations that matter most.
- An SRE team that has had vague suspicions that "we're not
  seeing the timeouts in our trace dashboard" and wants
  Squadron to confirm whether sampling is the cause.
- A platform team that migrated to a new OTel SDK version
  and isn't sure whether the framework integration's default
  sampler matches the previous SDK's.
- An auditor verifying that production-critical functions
  are emitting traces at a reasonable rate.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of sampling rate analysis is
intentionally narrow:

- **Compute / database / kubernetes sampling rate is slice
  2+.** These tiers don't have a clean cloud-native
  "invocation count" metric to compare span throughput
  against. EC2 doesn't have one (the equivalent is
  application-level request count which Squadron doesn't
  see). Slice 2 may use a different per-tier approach
  (Prometheus counter correlation, etc.).
- **Span event sampling.** Span events (per-span exception
  markers, log lines) have their own sampling. Slice 2+.
- **Tail-sampling collector detection.** If the operator
  uses a tail-sampling collector in front of Squadron that
  selectively keeps spans, `observed_span_count`
  legitimately undercounts intended emission. Slice 2 may
  add a per-resource exclusion when the operator declares
  tail-sampling.
- **Per-language sampler library detection.** The
  recommendation suggests raising the sampling rate but
  does NOT detect WHICH sampler library / framework is in
  use. Slice 3+.
- **Adaptive sampling deep diagnosis.** Some SDKs (notably
  Application Insights) use adaptive sampling that
  responds to throughput. Squadron's detection treats the
  observed rate as the operator-experienced rate; if the
  SDK adapts, that's the same ratio Squadron sees. Slice 2
  may add adaptive sampling caveats in reasoning text.
- **Baseline-comparison detection.** Slice 1 fires when the
  current 24h ratio is below the absolute 5% floor. It does
  NOT detect SUDDEN drops (deploy event reduced the rate).
  Slice 2 candidate.
- **Auto-fix.** Squadron remains a recommender.

## The detection rule

For each serverless resource at scan time:

1. Squadron's traceindex reports the count of spans observed
   from this resource over the last 24 hours
   (`observed_span_count`).
2. Squadron queries the cloud-native invocation count metric
   over the same window (`expected_invocation_count`).
3. The ratio `observed_span_count / expected_invocation_count`
   is computed.
4. The recommendation fires when ALL THREE conditions hold:
   - **Ratio condition**: `ratio < 0.05` (strictly less than 5%)
   - **Minimum invocations**: `expected_invocation_count >= 1000`
   - **Not on exclusion list**: per the existing #531 slice 2
     chunk 4 affordance.

### Why 5% floor?

OTel SDK defaults vary by language + framework. Common
defaults Squadron should NOT flag:

- 100% (always-on): obviously fine.
- 10% (`TRACEIDRATIO_BASED` 0.1): the most common production
  default.
- 5% sustained: at the edge — the detection floor sits
  exactly at this value (strictly less-than), not below.
  The detection fires at 4.9%, not 5%.

Sub-5% sustained over 24h is unusual enough to warrant
operator review. Sub-1% is almost always a regression or
misconfiguration.

### Why 1000 invocation minimum?

A function invoked 50 times in 24h with 2 spans observed gives
a 4% ratio — looks aggressive but is statistical noise. 1000
invocations corresponds to roughly 40/hour sustained — a
meaningful traffic level where percentages are reliable.

### Why a single ratio threshold?

Slice 1 does NOT compare against a historical baseline
(unlike cold-start latency, which compares 24h vs 7d).
Sampling rates change at deploy time, not gradually.
Comparing to historical baseline would mask intentional
reductions; the absolute floor is the honest signal.

Slice 2 may add baseline comparison for detecting SUDDEN
drops (deploy event reduced the rate). For now, the
absolute floor wins on clarity.

## The five serverless surfaces — per-cloud invocation metrics

The substrate from cold-start slice 1+2 already wired per-cloud
`MetricQuerier`. Slice 1 of sampling rate adds ONE NEW METRIC
NAME per cloud:

> **⚠️ Accuracy note (v0.89.232).** The Azure invocation metric below should be
> `FunctionExecutionCount` (`FunctionInvocations` does not exist in Azure
> Monitor); the code rename is pending — see
> [detection-coverage.md](./detection-coverage.md). The OCI metric was corrected
> to `FunctionInvocationCount` in v0.89.229.

| Cloud | Surface         | Invocation metric                                          |
|-------|-----------------|------------------------------------------------------------|
| AWS   | Lambda          | `AWS/Lambda Invocations` (Sum statistic)                   |
| GCP   | Cloud Run       | `run.googleapis.com/request_count` filtered by `response_code_class != "5xx"` |
| GCP   | Cloud Functions | `cloudfunctions.googleapis.com/function/execution_count` filtered by `status = "ok"` |
| Azure | Functions       | `FunctionExecutionCount` (Total aggregation)              |
| OCI   | Functions       | `FunctionInvocationCount` (Sum)                            |

All five metrics fold into the existing rate limiter for each
cloud — the substrate adds 1 metric query per resource per
scan (alongside cold-start's 2 windows), so the overhead is
50% on top of cold-start, still well within per-cloud quotas.

## The three failure modes

Following the recurring Squadron pattern, the
`span-quality-sampling-too-aggressive` recommendation
acknowledges three possible causes.

### Case 1 — Default sampler too aggressive

The most common cause: the OTel SDK was installed with a
framework integration that defaults to a low sampling rate.

How to recognize:
- Check the SDK configuration in the deployed code —
  `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` env vars,
  or framework-level config.
- If the values are explicitly low (e.g. 0.01), this is your
  case.
- The recommendation's PR is the RIGHT fix here. Raise the
  sampler arg to 0.5 (or your operator-tuned floor).

### Case 2 — Adaptive sampling throttling

Application Insights and some OTel exporters use adaptive
sampling that throttles UNDER LOAD. The ratio Squadron sees
is the OPERATOR-EXPERIENCED rate — not the configured rate.

How to recognize:
- Check the SDK config: if the sampler is `ParentBased` with
  an Application Insights exporter, adaptive sampling is
  likely on.
- The function's invocation count may correlate with the
  ratio — high invocation count = low ratio (throttling).

What to do: the recommendation's PR (raising the rate) won't
help if the throttling is adaptive. Decline with the note
"adaptive sampling intentional under high load." The verdict
learning loop records.

### Case 3 — Tail-sampling collector

If the operator runs a tail-sampling collector in front of
Squadron's OTLP receiver, the collector selectively keeps
spans based on properties (error / slow / etc.).
`observed_span_count` legitimately undercounts the configured
emission.

How to recognize:
- Check the collector pipeline config: any `tail_sampling`
  processor in front of Squadron means this is your case.
- Sustained low ratio with NO traffic correlation often
  indicates tail-sampling.

What to do: decline the recommendation. Slice 2 may add a
per-resource "tail-sampling intentional" flag.

## Per-cloud Terraform patterns

All 5 surfaces set the same TWO env vars — `OTEL_TRACES_SAMPLER` (a ratio
sampler) AND `OTEL_TRACES_SAMPLER_ARG` (the ratio) — just with different
injection mechanisms. Setting the ARG alone is a NO-OP: the OTel SDK's default
sampler (parentbased_always_on) ignores it.

### AWS Lambda

```hcl
resource "aws_lambda_function" "<name>" {
  environment {
    variables = {
      OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"
      OTEL_TRACES_SAMPLER_ARG = "0.5"  # operator tunes
    }
  }
}
```

### GCP Cloud Run

```hcl
resource "google_cloud_run_service" "<name>" {
  template {
    spec {
      containers {
        env {
          name  = "OTEL_TRACES_SAMPLER"
          value = "parentbased_traceidratio"
        }
        env {
          name  = "OTEL_TRACES_SAMPLER_ARG"
          value = "0.5"
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
    environment_variables = {
      OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"
      OTEL_TRACES_SAMPLER_ARG = "0.5"
    }
  }
}
```

### Azure Functions

```hcl
resource "azurerm_linux_function_app" "<name>" {
  app_settings = {
    OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"
    OTEL_TRACES_SAMPLER_ARG = "0.5"
  }
}
```

### OCI Functions

```hcl
resource "oci_functions_function" "<name>" {
  config = {
    OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"
    OTEL_TRACES_SAMPLER_ARG = "0.5"
  }
}
```

The 0.5 (50%) target is the starting point — operators tune
based on cost vs. observability tradeoff. The recommendation
reasoning explicitly notes that decline path.

## The 6-column SPAN QUALITY dashboard panel

The slice 2 5-column SPAN QUALITY panel grows to 6 columns:

```
  Orphan      Missing     Attr        Malformed         Missing       Sampling
  trace       attrs       mismatch    traceparent       on child      too aggressive
   3.2%        6.3%        2.0%          0.8%            4.1%            12.5%
12 res.     18 res.     6 res.        3 res.          14 res.        4 res.
```

Each column is clickable, deep-linking to the per-provider
Recommendations tab filtered by the corresponding kind. The
new 6th column deep-links to the
`span-quality-sampling-too-aggressive` filter.

The panel hides when all 6 percentages are zero.

The Tailwind grid at the smallest viewports may compress
labels. The slice 1 chunk 3 implementation uses
`sm:grid-cols-2 md:grid-cols-3 lg:grid-cols-6` to wrap
gracefully on narrow screens.

## The per-Serverless-row Sampling rate column

Each DiscoveryX Serverless table now has a "Sampling rate
(24h)" column between "Cold-start P95 (24h)" and "Last seen":

| Column            | Source                                |
|-------------------|---------------------------------------|
| Resource Name     | function name / service name          |
| Surface           | lambda / cloudrun / etc.              |
| Runtime           | python3.11 / nodejs20.x / etc.        |
| Region            | resource region                       |
| Trace axis        | existing                              |
| OTel distro       | existing                              |
| Cold-start P95    | existing (slice 2 of cold-start)      |
| Sampling rate     | NEW — ratio as percentage; amber when below floor + above minimum |
| Last seen         | existing                              |
| Quality           | existing (AWS only, slice 1 deferral) |

Hover shows the underlying `observed_span_count /
expected_invocation_count`.

## The per-Inventory-row QualityDot tooltip extension

The per-row Quality dot from span quality slice 1 stays; the
hover tooltip extends to show all 6 percentages:

> Orphan 3.2%, Missing attrs 6.3%, Mismatch 2.0%, Malformed
> traceparent 0.8%, Missing on child 4.1%, Sampling too
> aggressive 12.5%

The dot color logic counts any of the 6 percentages > 0
toward "issues."

## The per-resource sampling endpoint

```
GET /api/v1/discovery/{provider}/inventory/serverless/{id}/sampling
```

Returns:

```json
{
  "resource_arn": "...",
  "window_hours": 24,
  "observed_span_count": 142,
  "expected_invocation_count": 3500,
  "sampling_ratio": 0.0406,
  "exceeds_floor": true,
  "exceeds_minimum_invocations": true,
  "would_fire_recommendation": true,
  "observed_at": "..."
}
```

The two underlying gate flags (`exceeds_floor` and
`exceeds_minimum_invocations`) surface separately so consumers
can distinguish "below floor but above minimum (fires)" from
"below floor AND below minimum (statistical noise — does NOT
fire)."

## Per-cloud rate limits + cost surface

The substrate's existing rate limiters from cold-start slice 1+2
absorb the new query:

- AWS: 10 RPS; sampling adds 1 query/resource (cold-start has 2)
- GCP: 60 RPM
- Azure: 12000 RPH (200 RPM)
- OCI: 10 TPS

For a 4-cloud serverless fleet of 1000 functions per cloud,
sampling adds 4000 queries/day across all clouds — negligible
relative to per-cloud quotas.

Cost: covered by the same free tiers as cold-start. No
incremental cost per the no-money brief.

## Workflow — first sampling rate scan

1. Open the AWS Discovery page (`/discovery/aws`). Note the
   existing connection.
2. **No IAM upgrade required** — the cold-start arc's
   permissions already cover the new metrics.
3. Click "Run scan". The scan walks serverless functions,
   queries the cloud-native invocation count alongside the
   cold-start metrics, computes the ratio.
4. Open the Discovery dashboard. The SPAN QUALITY panel's
   new 6th column "Sampling too aggressive" shows the
   per-resource exceedance percentage.
5. Click into a per-provider page. The Serverless table's
   "Sampling rate (24h)" column shows the per-resource
   ratio. Amber cells need attention.
6. Click into a function with an amber cell → opens the
   per-resource sampling endpoint detail OR opens the
   Recommendations tab filtered to `span-quality-sampling-too-aggressive`.
7. Review the 3-failure-mode reasoning. Pick yours. Merge
   the PR (case 1) or decline with note (case 2 or 3).

## Reading the audit

Slice 1 reuses the existing audit event types — no new
constants. The recommendation lifecycle
(`recommendation.created` / `pr_opened` / `pr_merged` /
`pr_closed`) carries the new
`span-quality-sampling-too-aggressive` kind.

SIEM consumers can filter:

```
recommendation_kind = "span-quality-sampling-too-aggressive"
```

## Troubleshooting

- **Sampling rate cell shows "—" for all my functions.** The
  function must have been observed by Squadron's traceindex
  (at least 1 span flowed in the 24h window) AND the
  invocation count must be queryable. Check the
  per-resource sampling endpoint for the underlying counts;
  if `observed_span_count = 0`, the function may not be
  emitting traces at all (separate problem — check the
  trace-emission recommendations).
- **Sampling rate shows 0% with high invocation count.** The
  function is emitting NO spans despite high traffic. This
  is likely a misconfigured exporter — check the OTel SDK
  exporter destination. Squadron's trace-emission
  recommendations may already be firing for this resource.
- **Sampling rate shows 4% but no recommendation fires.**
  Check the `exceeds_minimum_invocations` flag in the
  per-resource endpoint. If false, the function has fewer
  than 1000 invocations in the window — too noisy to
  trust the percentage.
- **Recommendation fires on a function that's intentionally
  sampled at 1%.** This is case 3 (cost-conscious intentional
  sampling) or case 2 (adaptive). Decline the recommendation;
  the verdict learning loop records the decline reason.
- **The dashboard SPAN QUALITY 6th column shows high
  percentage but the per-provider Recommendations tab is
  empty.** The aggregation uses honest denominators — only
  resources above the 1000-invocation minimum count toward
  the denominator. The per-resource detail may show a high
  ratio but be below minimum invocations. Check the per-resource
  sampling endpoint.
- **Sampling rate column shows different value than the
  per-resource sampling endpoint.** The inventory annotation
  uses the latest observation; if the latest scan is older
  than the per-resource endpoint's freshly computed value,
  they'll differ. Re-run the scan to refresh.

## What slice 2 will add

Per §13 of the design doc:

- Compute / database / kubernetes sampling rate (different
  per-tier approach since they lack a clean cloud-native
  invocation-count metric — possibly Prometheus counter
  correlation for k8s workloads).
- Span event sampling rate.
- Tail-sampling collector detection.
- Per-language sampler library detection (Python
  opentelemetry-api vs ParentBased TraceIdRatioBased vs
  framework-specific defaults).
- Adaptive sampling deep diagnosis with explicit caveat
  text in the reasoning.
- Baseline comparison detection (detect SUDDEN drops in
  sampling rate from previous scans).
- Per-deploy-event sampling reset (when an operator
  deploys, ignore the immediate window because the
  configured rate may have just changed).
- Per-resource "tail-sampling intentional" flag.
- Compute / k8s observed throughput vs application metrics
  (if a k8s pod exposes a Prometheus `http_requests_total`
  counter, compare against traceindex span count).

## Strategic frame — the substrate compounds

This is the second diagnostic running on the cold-start
latency substrate. The architectural bet that the substrate
compounds is PROVEN. Slice 1 of sampling rate added:

- One new metric name per cloud (additive)
- One new detection branch (proposer-side)
- One new recommendation kind
- Six lines of new UI columns
- NO new substrate

That's roughly 1/4 the implementation cost of cold-start
slice 1, because slice 1 paid the substrate cost.

Squadron's universal claim doesn't grow a new verb — slice
1 of sampling rate makes the existing MEASURES verb more
specific. The "where did my trace go?" diagnostic chain
gains a fifth sibling:

1. Is the cloud-native trace primitive enabled at the event
   source? (event source slice 1)
2. Does the event source's CONFIG preserve trace context
   end-to-end? (event source slice 2)
3. Does the trace context that arrives conform to W3C?
   (span quality slice 2)
4. Is the cold-start latency reasonable? (cold-start arc)
5. **Is enough of the traffic actually being sampled?**
   (this arc)

These five diagnostic layers cover the full "request →
execution" chain at the substrate level. An operator who
sees orphan spans on the consumer side can now walk the
chain step-by-step, with each step having its own
recommendation kind and IaC PR.

The Tuesday LinkedIn drumbeat narrative gains the most
specific "where did my trace go?" answer yet: "your Lambda's
sampler is throttling 96% of invocations. The slow-tail
P99 requests that timeout are exactly the 4% that matters;
you can't see them because they're not being sampled.
Squadron just drafted the PR to raise the sampler ratio
to 50%."

## Cross-references

- [Sampling rate analysis slice 1 design doc](./proposals/sampling-rate-analysis-slice1.md) —
  the locked spec this runbook operationalizes.
- [Cold-start latency operator guide](./cold-start-latency-operator-guide.md) —
  the substrate arc that this reuses.
- [Span quality operator guide](./span-quality-operator-guide.md) —
  the SPAN QUALITY panel this extends from 5 columns to 6.
- [Serverless tier slice 1](./proposals/serverless-tier-slice1.md) —
  the inventory rows this annotates.
- [Audit log](./audit-log.md) — full catalog of event types.
