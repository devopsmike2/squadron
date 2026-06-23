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

## What slice 2 will add

Per §12 of the design doc:

- W3C trace context header parsing — catches "parent_span_id
  is zero but should not be" by inspecting traceparent on
  inbound spans.
- Sampling rate analysis — compares observed span throughput
  against expected throughput from cloud-native metrics;
  detects "you set the sampler to 1% but you should be at
  5%."
- Per-language semantic convention validation — checks
  incoming spans against the OpenTelemetry semantic
  conventions YAML for the tier + language.
- Span event content quality — PII redaction policy, stack
  trace truncation policy. Higher threat surface; deferred
  to its own slice.
- Span quality history table — durable trend analysis,
  diffing two windows over a 7-day range.
- Auto-fix for low-risk pathologies — env var injection that
  doesn't change application semantics may eventually be
  safe to apply automatically; slice 1 explicitly does not
  do this.

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
