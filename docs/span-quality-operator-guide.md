# Span quality — operator guide

This is the operator-facing runbook for the v0.89.84 through
v0.89.88 span quality slice 1 arc. Squadron's traceindex now
inspects every incoming OTLP span on the hot path and flags
three classes of pathology: orphan spans (broken context
propagation), spans missing required resource attributes, and
spans with placeholder values in required attributes.

The strategic frame: trace integration slice 1 shipped
**visibility** (Discovery dashboard panel + Last seen column).
Slice 2 shipped **action** for case (a) — SDK not deployed.
Span quality slice 1 ships **action** for cases (b) — exporter
misconfigured — and (c) — attribute mismatch. After this arc,
Squadron can detect all three failure modes the trace-emission
recommendations reasoning text described, and draft the IaC
PR for each.

For a first test, the walkthrough takes about 15 minutes —
most of it spent confirming your OTel collectors are emitting
spans that Squadron receives. For a production deployment with
high-volume OTel coverage already in place, the SPAN QUALITY
panel populates in the rolling 1-hour window after deployment.

## What this is good for

- A team that has OTel collectors deployed and emitting, but
  suspects the spans aren't as healthy as the volume suggests
  (lots of `host.name=localhost` from misconfigured detectors,
  orphan spans from broken propagation, etc.).
- A team running multi-cloud OTel and wanting one panel that
  reads quality across AWS, GCP, Azure, and Oracle Cloud
  uniformly.
- An auditor who needs to correlate "Squadron received N spans
  from this cluster" with "M% of them had broken propagation"
  in the same audit trail.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of span quality is intentionally
narrow:

- **Squadron does NOT store span content beyond in-memory
  counters.** The QualityCounters are an in-memory rolling
  window (1h per resource). The OTel batch contents still
  flow to the existing DuckDB telemetry store; the slice 1
  detection works on counter increments, not on stored
  content.
- **W3C trace context header parsing is slice 2.** Slice 1
  detects orphan spans by checking the parent_span_id against
  spans seen in a 5-minute window. It does NOT parse the
  traceparent header to detect "you fell back to a new
  trace because the upstream header was malformed."
- **Per-language semantic convention validation is slice 2+.**
  Slice 1 checks a small fixed set of required attributes per
  tier. It does NOT validate that your `http.request.method`
  matches the semantic-convention casing for your language's
  SDK.
- **Sampling rate analysis is slice 2.** Detecting "sampling
  is too aggressive; you're missing the long tail" requires
  windowed throughput analysis comparing observed span rate
  to expected throughput from cloud-native metrics.
- **Cross-trace correlation is slice 4+.** A span chain that
  crosses Lambda → SQS → Cloud Function carries context
  across cloud boundaries; that's a separate arc.
- **Span event content quality is slice 3+.** Span events
  ("exception thrown at X") and their stack traces / PII
  surface are their own threat model.
- **Auto-fix.** Slice 1 surfaces gaps + drafts PRs. Squadron
  remains a recommender, not an actor.

## The three pathologies

### Orphan spans (case b — exporter misconfigured)

A span is **orphan** when its `parent_span_id` is non-zero but
no span with that span_id has been observed in the same trace
within the last 5 minutes. Common causes:

- **Broken HTTP context propagation.** The calling service
  emitted a span. The called service's library didn't read
  the W3C traceparent header. The called service's span has
  a parent_span_id pointing at the caller's span — but the
  called service is in a fresh trace_id, so the parent never
  resolves.
- **Queue header stripping.** SQS without
  MessageAttributesNames="All", Kafka without OpenTelemetry
  headers turned on, Redis pub/sub: the propagation chain
  breaks at the queue.
- **Exporter mid-flush restart.** The collector restarted
  mid-batch, dropped the parent span, kept the child.

A resource with > 10% of its spans orphaned in the last hour
fires a `span-quality-orphan-trace` recommendation.

### Missing required attributes (case c — attribute mismatch)

Each tier has a fixed set of required attributes the
recommendation logic depends on:

| Tier       | Required                                                            |
|------------|---------------------------------------------------------------------|
| Compute    | `service.name`, `cloud.provider`, `cloud.account.id`, `cloud.region`, AND one of `host.id` / `host.name` / `cloud.resource_id` |
| Database   | `service.name`, `cloud.provider`, `cloud.account.id`, `db.system`, `db.name` |
| Kubernetes | `service.name`, `cloud.provider`, `cloud.account.id`, `k8s.cluster.name`, `k8s.namespace.name`, `k8s.pod.name` |

A span is "missing attrs" if any required attribute for its
tier is absent (empty string).

The most common causes:

- The OTel resource detector ran with insufficient IAM
  permissions and silently failed to populate the cloud.*
  attributes.
- The resource detector ran before the cloud metadata
  service was reachable (race on startup).
- The workload was deployed without explicit
  `OTEL_RESOURCE_ATTRIBUTES` env vars, and the SDK doesn't
  have a host detector for the platform (common on bare-metal
  / Docker-on-Mac dev setups).

A resource with > 25% of its spans missing required attributes
in the last hour fires a `span-quality-missing-resource-attrs`
recommendation.

### Attribute placeholder/mismatch (case c — different flavor)

A static list of known placeholder values per attribute:

| Attribute            | Placeholder values                          |
|----------------------|---------------------------------------------|
| `host.name`          | `localhost`, `127.0.0.1`, `unknown_host`    |
| `cloud.account.id`   | `000000000000`, `123456789012`, `unknown`   |
| `service.name`       | `unknown_service`, `default-service`        |
| `cloud.region`       | `unknown_region`                            |
| `cloud.provider`     | (must be aws/gcp/azure/oci; otherwise mismatch) |

A span "matches" if ANY required attribute has one of the
listed placeholder values. The most common causes:

- The SDK fell back to defaults when the resource detector
  failed silently.
- The deployment was templated from a different cluster's
  config and the values were never overridden.
- Some test fixture or canary deployment leaked into the
  production stream.

A resource with > 5% of its spans matching a placeholder in
the last hour fires a `span-quality-attribute-mismatch`
recommendation. The 5% threshold is intentionally low — even
small fractions of placeholders indicate the SDK is doing
something wrong.

## The minimum sample size

A recommendation does NOT fire unless the resource has emitted
at least 100 spans in the current 1-hour window. This avoids
noisy recommendations on low-traffic resources (the resource
that emitted 4 spans this hour, all orphan, would otherwise
trigger a recommendation despite being statistical noise).

Slice 2 may tune this threshold per tier — a database tier
sees fewer spans than a compute tier, so the floor may need
to be lower for db.

## The three new recommendation kinds

```
span-quality-orphan-trace
span-quality-missing-resource-attrs
span-quality-attribute-mismatch
```

Each kind is drafted by the proposer when its threshold is
exceeded AND the row isn't on the exclusion list (#531 slice 2
chunk 4). The recommendation's Reasoning field cites the
specific percentages observed; the Terraform field carries a
per-cloud pattern the operator can merge:

### span-quality-orphan-trace patterns

- **ADOT on EC2:** edit the collector config to add
  `propagators: [tracecontext, baggage]` to the OTLP
  receivers.
- **K8s with OpenTelemetry Operator:** edit the
  Instrumentation CRD's propagators field.
- **Inline SDK:** add `OTEL_PROPAGATORS=tracecontext,baggage`
  to the Deployment env block (K8s) or task definition (ECS).
- **Lambda:** add the OTEL_PROPAGATORS env var to the
  function's environment_variables.

### span-quality-missing-resource-attrs patterns

- **AWS EC2:** add `ec2:DescribeInstances` to the instance
  profile (the SDK resource detector needs it to populate
  cloud.account.id).
- **AWS EKS:** ensure the pod's service account has the
  IRSA annotation pointing at a role with
  `eks:DescribeCluster`.
- **GCP:** ensure the workload's service account has
  `roles/compute.viewer` (resource detector reaches the
  compute metadata server).
- **Azure:** ensure managed identity is enabled on the VM /
  AKS / App Service host.
- **OCI:** ensure instance principal auth is configured
  (dynamic group → policy).

### span-quality-attribute-mismatch patterns

- **EC2:** write the correct values to the instance's
  user-data via Terraform metadata so the SDK reads them at
  startup.
- **ECS:** add `OTEL_RESOURCE_ATTRIBUTES=cloud.account.id=NNN,host.id=EEE,cloud.region=us-east-1`
  to the task definition's environment block.
- **K8s:** add the same env var to the Deployment's pod
  spec via `kubernetes_manifest` Terraform.
- **GCE / Azure VM / OCI Instance:** corresponding
  user-data / cloud-init / extension patterns per cloud.

## The Discovery dashboard SPAN QUALITY panel

Below the existing TRACE COVERAGE panel, slice 1 adds a SPAN
QUALITY panel — a 3-column health grid showing:

```
  Orphan trace      Missing attrs    Attribute mismatch
       3.2%              6.3%             2.0%
   12 resources      18 resources       6 resources
```

The panel hides entirely when all three percentages are zero.
Clicking any column deep-links to the per-provider
Recommendations tab filtered to the corresponding kind.

## The per-Inventory-row Quality column

Each Inventory row across the existing 12 surfaces (4 clouds ×
3 tiers) gains a small Quality dot column next to the existing
"Last seen" column:

- **Green dot:** no quality issues in the last hour.
- **Yellow dot:** 1 issue class triggering (orphan OR missing
  attrs OR mismatch).
- **Red dot:** 2+ issue classes triggering.
- **Gray dot:** no spans observed in the window — not enough
  data to evaluate.

Hovering shows a tooltip with the specific percentages:

> Orphan 3.2%, Missing attrs 6.3%, Mismatch 2.0%

The dot color thresholds are NOT the same as the recommendation
firing thresholds — a 4% orphan rate would render a yellow dot
even though it doesn't fire a recommendation (the threshold for
a recommendation is 10%). The dot is your "early warning";
the recommendation is your "fix it" signal.

## The Recommendations filter chip

The slice 2 chunk 3 filter chip "Show only trace-emission"
gains a sibling on the AWS Recommendations tab:

> [ Show only span-quality ]

Same toggle behavior — yellow when active, slate otherwise.
Clicking filters the recommendations list to only
`span-quality-*` kinds.

The GCP / Azure / OCI Recommendations tabs are stubs awaiting
their own chunk-5 follow-on (same deferral as the slice 2
trace-emission chip).

## Workflow — first span-quality recommendation

1. Open the Discovery dashboard at `/discovery`. Confirm
   both the TRACE COVERAGE panel and the new SPAN QUALITY
   panel render. If SPAN QUALITY shows non-zero
   percentages, you have at least one resource that will
   trigger a span-quality recommendation.
2. Click one of the SPAN QUALITY columns (Orphan / Missing /
   Mismatch). You're deep-linked to the AWS Recommendations
   tab with the span-quality filter chip pre-active.
3. Review each draft recommendation. The Reasoning text
   names the specific pathology + the observed percentages.
   The Terraform field carries the suggested fix.
4. For the SDK-config case: click Open PR. Webhook listener
   records the open. When you merge, the
   `recommendation.pr_merged` audit fires; the verdict
   learning loop records the merge as positive signal.
5. For false positives (a workload that legitimately uses
   `host.name=localhost` for dev / test): click Don't
   propose this again. The exclusion table records the
   suppression scoped to (resource, kind).
6. Wait 1h+ and re-load the dashboard. If your fix worked,
   the relevant SPAN QUALITY percentage drops; the
   per-row Quality dot may shift green.

## Reading the audit

Slice 1 adds one new audit event type:

- `span_quality.requested` — emitted on a cache miss when
  the dashboard or proposer pulls fresh data from the
  Quality index. Payload includes the cache age, the
  per-provider counts. Audit-only; no side effects.

Threshold breaches that turn into recommendations fire the
existing `recommendation.created` event with the new kind
value. SIEM consumers can filter on the prefix:

```
recommendation_kind ~= "^span-quality-"
```

PR open / merge / close fires the existing webhook events
with the corresponding kind values. The webhook router
extends the kind-prefix detection to route `span-quality-*`
to the correct provider — since the kind doesn't carry a
provider segment, the router falls back to looking up the
resource the recommendation targets, then extracting the
provider from there.

## Troubleshooting

- **The SPAN QUALITY panel is empty but I know I have OTel
  collectors deployed.** Check that the collectors are
  pointed at Squadron's OTLP receiver (port 4318 HTTP /
  4317 gRPC). The traceindex shows received span counts
  in the `traceindex.background_flushed` audit event;
  if that count is zero, your collectors aren't reaching
  Squadron.
- **The SPAN QUALITY panel shows non-zero counts but no
  recommendation drafts appear.** Check the per-resource
  span counts — recommendations only fire after 100 spans
  in the window. Run a load test against the resource to
  push it over the threshold, OR wait for natural traffic.
- **A `span-quality-orphan-trace` recommendation fires but
  I know my context propagation is fine.** Check whether
  your trace_id sampling is dropping the parent span before
  Squadron sees it. The Quality observer can't distinguish
  "parent never existed" from "parent sampled out." Decline
  the recommendation; the verdict learning loop records
  the decline.
- **A `span-quality-attribute-mismatch` fires on a workload
  that intentionally uses placeholder values (test fixture,
  canary, dev sandbox).** Click Don't propose this again.
  The exclusion is scoped to (resource, kind); other
  workloads still get the recommendation.
- **Per-row Quality dots all render gray even though spans
  are flowing.** Per-row span_quality data lands in chunk
  4 follow-on (the inventory scanners need to call the
  per-resource span_quality endpoint to populate the
  field). Until then, gray dots are expected on rows that
  haven't been individually queried. Click into a
  resource's detail panel; the dot color refreshes from
  there.

## Slice 2 SHIPPED in v0.89.108-v0.89.111

Slice 2 closes the explicit slice 1 deferral above
(W3C trace context header parsing). The full design doc is
at [proposals/span-quality-slice2.md](./proposals/span-quality-slice2.md).

# Slice 2 — W3C trace context parsing (v0.89.108-v0.89.111)

Slice 1 shipped three pathology detectors at the OTLP receiver
hot path: orphan spans, missing required resource attributes,
attribute placeholders. Slice 2 adds two more at the same hot
path: malformed traceparent and missing traceparent on child.

## The two new pathologies

### Malformed traceparent

Defined as: a span carries a `traceparent` attribute, but its
value doesn't conform to the W3C trace context spec format
`00-{32hex}-{16hex}-{2hex}`.

Detection rule:
- length 55
- hyphens at positions 2, 35, 52
- version segment "00" (slice 2 only; future versions like
  "01" or "ff" are rejected per spec reserved values)
- 32-char trace_id segment: hex (lowercase 0-9 a-f), non-zero
- 16-char parent_id segment: hex, non-zero
- 2-char trace_flags segment: hex

Threshold: > 1% of spans with a traceparent attribute. The 1%
is intentionally low — ANY malformed traceparent is unusual.
A correctly-instrumented fleet either has 0% malformed or
~100% malformed depending on whether the upstream SDK is
broken.

The denominator is `spans_with_traceparent`, NOT `total_spans`.
A span with no traceparent attribute can't be malformed.

### Missing traceparent on child

Defined as: a span has a non-zero `parent_span_id` (it's a
child span) AND no `traceparent` attribute. This suggests the
SDK either didn't extract the W3C context propagator on the
inbound request, OR the resource is a worker/background-job
context where child spans are intra-process and never
received an inbound traceparent.

Threshold: > 5% of CHILD spans. The 5% is between the slice 1
orphan (10%) and the slice 2 malformed (1%) thresholds — SDK
propagation is mostly all-or-nothing but some legitimate cases
(pure internal spans) lack traceparent.

The denominator is `child_spans`, NOT `total_spans`. A root
span (zero parent_span_id) can't be missing-on-child.

## The three failure modes per kind

Just like slice 1's trace-emission and slice 1's first three
pathologies, the slice 2 kinds acknowledge three possible
causes. The PR drafts target the most common cause; declining
the PR records the actual cause for the verdict learning loop.

### span-quality-traceparent-missing

1. **SDK middleware not enabled.** The OTel SDK is installed
   but the W3C context propagator middleware isn't enabled in
   the HTTP server config. Squadron's PR drafts add
   `OTEL_PROPAGATORS=tracecontext,baggage` env var injection
   (same pattern as the slice 1 orphan-trace recommendation).
2. **Custom middleware consumes the header first.** Some
   teams have custom logging or rate-limiting middleware in
   front of the SDK that pops the traceparent header before
   the SDK reads it. The PR's env var won't help; the team
   needs to reorder middleware.
3. **Worker pod with no inbound HTTP.** A pod that processes
   queue messages or runs cron jobs has child spans that are
   intra-process, not from external HTTP. Squadron's PR will
   produce no behavior change. Decline.

### span-quality-traceparent-malformed

1. **Upstream emits custom-format trace ID.** Some legacy SDKs
   emit non-W3C trace IDs (e.g. 64-bit DataDog IDs in the W3C
   slot). Squadron's PR drafts pin the upstream SDK to a
   W3C-compliant version.
2. **SDK version mismatch.** The upstream emits a "next-version"
   (01) traceparent because it shipped against a future spec
   revision; the downstream rejects it because slice 2 only
   accepts version 00. Pin to a matched SDK version pair.
3. **Proxy / load balancer rewriting.** Some ALBs and CDNs
   rewrite the X-Amzn-Trace-Id header in transit; downstream
   sees a malformed value. Check the AWS X-Ray header
   handling config on the ALB.

## The denominator semantics

This catches operators reading the dashboard percentages.

The slice 1 percentages (orphan / missing attrs / mismatch)
use `total_spans` as denominator — every span is eligible.

The slice 2 percentages use HONEST denominators:

| Pathology                       | Denominator             |
|---------------------------------|-------------------------|
| Orphan spans                    | total_spans (slice 1)   |
| Missing required attrs          | total_spans (slice 1)   |
| Attribute placeholders          | total_spans (slice 1)   |
| Malformed traceparent           | spans_with_traceparent  |
| Missing traceparent on child    | child_spans             |

A resource with 1000 spans, 200 of which carry traceparent,
8 of which are malformed: malformed_pct = 4% (8/200), NOT
0.8% (8/1000). The honest framing matters because the 1%
threshold would otherwise misfire.

The per-resource detail endpoint exposes the denominators
(`spans_with_traceparent` and `child_spans`) so consumers can
re-derive if needed.

## The 5-column SPAN QUALITY dashboard panel

The dashboard panel grows from 3 columns to 5:

```
  Orphan trace      Missing attrs    Attribute mismatch    Malformed traceparent    Missing on child
       3.2%              6.3%             2.0%                 0.8%                       4.1%
   12 resources      18 resources       6 resources         3 resources                14 resources
```

The panel hides when all 5 percentages are zero (extended
condition from slice 1).

Each column is clickable, deep-linking to the per-provider
Recommendations tab filtered by the corresponding kind. The
two new columns deep-link to the
`span-quality-traceparent-{missing,malformed}` filter.

## Per-Inventory-row Quality dot tooltip

The per-row Quality dot from slice 1 stays. The hover tooltip
extends to show all 5 percentages:

> Orphan 3.2%, Missing attrs 6.3%, Mismatch 2.0%, Malformed
> traceparent 0.8%, Missing on child 4.1%

The dot color logic stays the same (green if all zero, yellow
if one issue class, red if 2+).

## The minimum-sample-size guards

In addition to the slice 1 minimum (100 total spans / hour),
slice 2 adds two more:

- **`span-quality-traceparent-malformed`** requires at least
  50 spans with a traceparent attribute in the window. Below
  that, the percentage is statistically meaningless.
- **`span-quality-traceparent-missing`** requires at least 50
  child spans. Same rationale.

A resource with only 30 spans-with-traceparent (and 8
malformed → 26.7% malformed_pct) does NOT fire the
recommendation. Wait for more traffic OR run a load test to
push the sample size over the threshold.

## Workflow — first traceparent recommendation

1. Open the Discovery dashboard at `/discovery`.
2. Look at the SPAN QUALITY panel — it now has 5 columns.
3. If "Malformed traceparent" or "Missing on child" shows a
   non-zero percentage, click the column.
4. You're deep-linked to the AWS Recommendations tab with the
   span-quality filter chip active.
5. Review the recommendation. The Reasoning text names the
   three failure modes; pick yours.
6. For the SDK-side fix: click Open PR. The webhook listener
   records the open. When you merge, the verdict learning
   loop records.
7. For false positives (worker pod with intra-process child
   spans, etc.): click Don't propose this again.
8. Wait 1h+ and reload. If your fix worked, the relevant
   percentage drops.

## Reading the audit

No new audit event types. The recommendation lifecycle
(recommendation.created / pr_opened / pr_merged / pr_closed)
carries the new kind values.

SIEM consumers can filter:

```
recommendation_kind ~= "^span-quality-traceparent-"
```

## Slice 2 troubleshooting

- **Malformed traceparent shows 100% but my SDK should be
  W3C-compliant.** Check the SDK version — some 2022-era OTel
  SDK releases ship a non-W3C compliant traceparent format on
  certain HTTP frameworks. Pin to the latest SDK release.
- **Missing traceparent on child shows 100% on a worker pod
  that processes Pub/Sub messages.** The pod's spans are
  intra-process; there's no inbound HTTP carrying a
  traceparent header. Decline the recommendation. Slice 3
  may add per-resource-type detection to suppress this
  automatically.
- **The dashboard panel suddenly shows "Missing on child" at
  85%.** Likely cause: the upstream service was deployed
  without the W3C propagator middleware. Check the upstream
  deployment's env vars; the slice 1 trace-emission arc may
  have already drafted a `*-otel-*` recommendation for the
  upstream resource.
- **Malformed traceparent percentage is high but the
  recommendation doesn't fire.** Check the `spans_with_traceparent`
  count in the per-resource detail endpoint — if it's < 50,
  the minimum-sample-size guard suppresses the recommendation.
- **My team uses B3 propagation (not W3C).** Slice 2 currently
  treats B3-only fleets as 100% missing-traceparent-on-child.
  Click Don't propose this again on the recommendations;
  slice 3 may add format-agnostic detection.
- **A specific kind shows up in the recommendation list but
  the SPAN QUALITY dashboard shows 0% for that kind.** Check
  the cache — the dashboard endpoint has a 30s cache; the
  recommendation may have fired from a more recent in-memory
  observation. Refresh the dashboard.

## What slice 3 will add (slice 2 deferrals)

Per §13 of the slice 2 design doc:

- tracestate parsing (per-vendor metadata validation).
- Sampling decision propagation analysis.
- B3 / Jaeger / DataDog / Zipkin context detection.
- Per-language SDK fingerprinting.
- HTTP header inspection (when SDK doesn't attach to
  attributes).
- Per-vendor traceparent format extensions (some SDKs use
  non-canonical 32-char trace_id).
- Auto-fix for trivial cases (SDK version pin update).

## Slice 2 strategic frame

Slice 2 doesn't grow Squadron's universal claim — it makes
the existing span quality claim more rigorous. After this
arc, the "where did my trace go?" diagnostic surface has
three layers:

1. **Is the cloud-native trace primitive enabled at the
   event source?** (event source slice 1)
2. **Does the event source's CONFIG preserve trace context
   end-to-end?** (event source slice 2)
3. **Does the trace context that DOES arrive at Squadron's
   OTLP receiver conform to the W3C spec?** (span quality
   slice 2 — this arc)

These three diagnostic layers cover the full "request →
orchestration → execution" chain. An operator who sees
orphan spans on the consumer side can now walk the diagnostic
chain:

- Check event source slice 1: was the trace primitive on?
- Check event source slice 2: was the propagation config
  preserved?
- Check span quality slice 2: did the traceparent arrive
  malformed or absent?
- Check span quality slice 1: any other resource attribute
  pathologies?

Each step has its own recommendation kind and IaC PR. The
verdict learning loop teaches the proposer from every decline.

## What slice 3+ may add (slice 1 deferrals re-stated)

The slice 1 deferral list above stays accurate — slice 2
covered W3C trace context parsing. The remaining deferrals
include sampling rate analysis, per-language semantic
convention validation, span event content quality, span
quality history table, and auto-fix patterns.

## The universal claim grows a fourth verb

After span quality slice 1, Squadron's positioning reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps,
> verifies telemetry is actually flowing, validates the spans
> Squadron receives are healthy, AND drafts the IaC PRs that
> close the gaps it finds.

Four verbs. One control plane. The recommendation surface is
no longer about turning on the primitive — it's about
turning on the primitive, watching whether telemetry flows,
inspecting the telemetry that does flow for quality, and
drafting the PR that fixes whichever layer broke. The
reconciliation loop the Tuesday LinkedIn drumbeat narrative
promised compounds another iteration.

## Cross-references

- [Span quality slice 1 design doc](./proposals/span-quality-slice1.md) —
  the locked spec this runbook operationalizes.
- [Trace integration slice 1](./proposals/trace-integration-slice1.md) —
  the foundation that shipped the traceindex package + OTLP
  receiver wiring.
- [Trace integration slice 2](./proposals/trace-integration-slice2.md) —
  the slice that shipped the 12 trace-emission-* kinds + the
  iacpicker package this arc reuses for span-quality kinds.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the runbook for trace integration slices 1+2.
- [Unified Discovery dashboard slice 1](./proposals/unified-discovery-dashboard-slice1.md) —
  the dashboard the SPAN QUALITY panel sits on.
- [Audit log](./audit-log.md) — full catalog of event types
  including `span_quality.requested`.
