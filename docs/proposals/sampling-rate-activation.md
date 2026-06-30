# Sampling-rate detection activation (#295)

**Status:** design — implementation in slices. Arc kickoff after the #300
metric-detection wiring landed (v0.89.330–335).

## The dormancy

Sampling-rate analysis flags serverless functions whose **observed OTLP span
count** is far below their **cloud-native invocation count** over 24h — i.e. the
OTel SDK's trace sampler is dropping so aggressively that the function is
effectively unobserved. The rule (proposer/sampling_rate.go): `ratio =
spans / invocations`; fire `span-quality-sampling-too-aggressive` when
`ratio < 0.05` **and** `invocations >= 1000` (the second gate filters
statistical noise).

Everything downstream is already built and tested:

- `proposer.DetectSamplingRate(querier, qual, arn, surface, key)` — the detector.
- Per-cloud invocation metric routing (all 5 surfaces) through `QueryAggregate`.
- `GET /…/serverless/{id}/sampling` endpoint (`DiscoveryServerlessSamplingHandlers`).
- `AnnotateServerlessWithSampling` — populates `ServerlessInstanceSnapshot.
  SamplingRatio` + `.SamplingExceedsFloor` on the inventory rows (UI).
- Proposer checks (5 per-cloud variants) + iacpicker Terraform (OTEL_TRACES_SAMPLER
  env injection, all 5 clouds).

**What's missing:** a wired *producer*. No concrete `SamplingAnnotator` /
`SamplingDetector` is constructed in `main.go`/`server.go`, so both consumers
degrade to no-op / 404. This is the exact shape the cold-start + error-rate
detectors were in before #300 — and it's unblocked by the same fix: sampling
reads the invocation-count metric, which now has a wired per-cloud metric client
behind `serverless_metric_detection.enabled`.

## Architecture: a handler-level cross-layer join

Sampling is unique among the serverless detectors: it joins **two** data sources
that live in **different layers**:

1. **Cloud invocation count** — scanner-side, via the scanner's
   `QueryAggregate` (the metric client #300 wired).
2. **Observed span count** — server-side, from the OTLP receiver's
   `traceindex.Quality.SpanCountLast24h(key)` (already wired into the discovery
   handlers as the `qualityIndex`, v0.89.326).

Neither the scanner (no OTLP) nor the receiver (no cloud creds) has both. The
**handler** is the only layer that does — which is why the designed seam
(`AnnotateServerlessWithSampling`, the `SamplingAnnotator`/`SamplingDetector`
interfaces) is handler-level and does a **live** detection at scan-response time
rather than reading a persisted observation (unlike cold-start/error-rate).

### The join key

`traceindex.ComputeResourceKey` tier 1 keys verbatim on `cloud.resource_id`. A
properly-instrumented serverless function emits `cloud.resource_id` = its ARN /
resource name / OCID — the same identifier the scanner stores as
`ServerlessInstanceSnapshot.ResourceARN`. So the `SamplingKeyResolver` for
serverless is trivial: **key = ResourceARN**. When a function doesn't emit
`cloud.resource_id` (weaker instrumentation), the span lookup returns
`ok=false` → 0 observed spans → the annotator leaves the row's pointers nil
(renders "—"), which is the correct insufficient-data posture.

### The concrete adapter (slice 1)

A single small handler-package type implements **both** interfaces by
dispatching to the existing detector:

```go
type samplingDetector struct {
    querier proposer.SamplingRateMetricQuerier // a scanner's QueryAggregate
    quality proposer.SamplingRateSpanCounter   // *traceindex.Quality
}
func (d samplingDetector) DetectSampling(ctx, arn, surface, key) (…)  { return proposer.DetectSamplingRate(ctx, d.querier, d.quality, arn, surface, key) }
func (d samplingDetector) AnnotateSampling(ctx, arn, surface, key) (…) { return proposer.DetectSamplingRate(ctx, d.querier, d.quality, arn, surface, key) }
```

Plus a trivial `samplingKeyResolver` returning the ARN.

### Per-cloud wiring (slice 2+)

In each per-cloud scan handler, **after** `scanner.Scan()` returns and after the
cold-start/error-rate annotations, if (a) the scan-returned scanner
type-asserts to `SamplingRateMetricQuerier` (QueryAggregate is promoted from the
embedded `*Scanner`), and (b) `qualityIndex != nil`, build the `samplingDetector`
and call `AnnotateServerlessWithSampling(detector, arnResolver, result.Serverless)`.

This rides `serverless_metric_detection.enabled` implicitly: with the flag off,
the scanner has no metric client, `QueryAggregate` returns
`ErrMetricNotImplemented`, the annotator logs+continues, and the rows stay "—" —
zero behavior change, zero metric reads. With the flag on, the annotation runs.

**Known wrinkle — AWS per-region binding.** AWS builds `cwClient` per-region
during the scan walk and leaves `s.cwClient` bound to the *last* region. A
post-scan sampling query for a function in a *different* region would hit the
wrong region's CloudWatch. Resolution options (decide in the AWS slice): (i)
rebuild per-function-region in the annotator (mirror cloudWatchForRegion), or
(ii) group serverless rows by region. GCP/OCI have no per-region client, so they
are unaffected.

### Per-resource endpoint (later slice)

`GET /…/sampling` has no live scanner. Options: build a metric querier on-demand
from the connection (heavier), or have the scan-response annotation persist a
small sampling observation that the endpoint reads (mirrors cold-start). Deferred
— the inventory annotation (UI + recs) is the higher-value consumer and ships
first.

## Slice plan

1. **Concrete `samplingDetector` adapter + ARN key resolver + tests** (this
   slice — self-contained, no per-cloud wiring).
2. **GCP + OCI annotation wiring** (no region wrinkle) + handler tests.
3. **AWS annotation wiring** (resolve the per-region binding) + tests.
4. **Azure annotation wiring** (commercial App Insights path) — Azure's metric
   client is the App Insights component path; confirm `QueryAggregate` reaches it.
5. **Per-resource `/sampling` endpoint** wiring + docs reconciliation
   (detection-coverage.md sampling rows) + full gate.

## Non-goals

- No new config flag — rides `serverless_metric_detection.enabled`.
- No persisted sampling-observation store in slices 1–4 (the annotation is live).
- No change to the detection math, the recommendation kind, or the Terraform
  patterns — all already shipped.
