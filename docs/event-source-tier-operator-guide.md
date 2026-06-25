# Event source tier — operator guide

This is the operator-facing runbook for the v0.89.99 through
v0.89.103 event source tier slice 1 arc. Squadron now scans
four event source surfaces across all four clouds — AWS
EventBridge, GCP Pub/Sub, Azure Service Bus, OCI Streaming —
for the observability primitives that determine whether trace
context propagates through the inbound layer of your
architecture.

The strategic frame: Squadron previously covered five tiers
(compute / database / kubernetes / serverless / orchestration)
across four clouds. Event sources are the sixth tier — the
root of trace continuity. The "request → orchestration →
execution" chain Squadron scans now starts at the request
entry point. A Pub/Sub topic without `tracingConfig.samplingRatio`
above zero, or an EventBridge bus without any logging target,
means the trace ID never gets created (or never reaches the
downstream consumer), and every span Squadron's traceindex
receives downstream looks orphaned.

For a first test, the walkthrough takes about 20 minutes —
most of it spent confirming your cloud connections have the
additional read permissions for the event source APIs.

## What this is good for

- A team running EventBridge for event orchestration and
  wanting to confirm at least one rule targets a log
  destination — Squadron's traceindex needs spans from the
  downstream consumer to land somewhere it can read.
- A GCP team with Pub/Sub topics that publish to Cloud Run /
  Cloud Functions and seeing orphan spans on the consumer
  side — the cause is almost always `tracingConfig.samplingRatio`
  not being set above 0.
- An Azure team running Service Bus + Functions where some
  namespaces have diagnostic settings and some don't, and
  the consumer Functions emit traces but the trace IDs are
  fresh (no parent context arrived).
- An OCI team running Streams + Functions wanting to verify
  every stream has OCI Logging coverage so cross-stream
  correlation is possible.

## What this is NOT (slice 1)

Read this carefully. Slice 1 of event source tier is
intentionally narrow:

- **Per-message propagation analysis is slice 2+.** Slice 1
  checks whether the event source has the trace primitive
  enabled at the SOURCE level. It does NOT inspect whether
  every Pub/Sub message has the `googclient_OpenTelemetryTraceparent`
  attribute populated, or every EventBridge event has the
  X-Ray trace header in its detail field. That requires
  per-message inspection at the consumer side, which is its
  own substrate.
- **Cross-cloud event flows are slice 3+.** A message
  published to AWS EventBridge that flows out via SNS to a
  GCP Pub/Sub topic — that's a real architecture but the
  trace correlation across cloud boundaries is its own arc.
- **Per-target trace propagation on EventBridge rules is
  slice 2+.** Rules can have multiple targets; each target
  may or may not be configured to receive the X-Ray trace
  header.
- **Schema registry inspection is slice 2+.** EventBridge
  Schema Registry and Schema Registry for Pub/Sub define
  event payload schemas. Squadron does not inspect schema
  versions for traceparent fields in slice 1.
- **Dead-letter queue trace continuity is slice 2+.**
- **Auto-fix.** Squadron remains a recommender. Slice 1
  surfaces gaps + drafts PRs.

## The four event source surfaces

| Cloud | Surface         | Trace axis                                                  | Log axis                                          |
|-------|-----------------|-------------------------------------------------------------|---------------------------------------------------|
| AWS   | EventBridge     | bus has any rule with a CloudWatch Logs target (slice 1 proxy) | same proxy                                     |
| GCP   | Pub/Sub         | topic `tracingConfig.samplingRatio > 0`                     | (implicit via topic creation)                     |
| Azure | Service Bus     | namespace diagnostic settings → App Insights OR Log Analytics workspace | namespace has any diagnostic setting (incl. Event Hub destination) |
| OCI   | Streaming       | stream has Logging service log group (slice 1 proxy)        | same proxy                                        |

Each surface contributes one row per discovered event source
to the new Event sources Inventory sub-tab on the per-provider
Discovery page.

## The EventBridge Schemas detection softness

This catches first-time operators. EventBridge has two
mechanisms that look like "trace primitives":

1. **Schemas Discoverer** — a service that auto-discovers
   event schemas as they flow through the bus. Conceptually
   close to "trace this bus" because it implies the bus is
   observable.
2. **CloudWatch Logs target rules** — a rule that routes events
   to a CloudWatch Logs log group, making event content
   inspectable.

Slice 1 uses the LOG-TARGET path as the proxy for trace axis
because the Schemas Discoverer API lives in a separate AWS
SDK package and wiring it would have pushed chunk 1 past
budget. The slice 1 detection is: "does this bus have at
least one rule whose target is a CloudWatch Logs log group?"
If yes, HasTraceAxis = true AND HasLogAxis = true (both axes
share the proxy in slice 1).

This means: an EventBridge bus that has Schemas Discoverer
ENABLED but no log-target rules gets `has_trace_axis = false`
from Squadron. The `eventbridge-xray-enable` recommendation
that fires is a soft positive — the operator may already
have observability via Schemas. Decline the recommendation
if your trace strategy doesn't rely on the log-target path;
the verdict learning loop records.

Slice 2 will detect the Schemas Discoverer directly and the
recommendation logic will distinguish properly.

## The Pub/Sub samplingRatio=0 ambiguity

A GCP Pub/Sub topic with no `tracingConfig` field at all is
indistinguishable from a topic with explicit
`tracingConfig.samplingRatio = 0`. Both produce
`has_trace_axis = false` in slice 1. The
`pubsub-trace-enable` recommendation drafts a PR that sets
`tracing_config { sampling_ratio = 1.0 }` (full sampling) —
operators who deliberately set 0 for cost reasons should
decline and either reduce the sampling rate in the PR or
explicitly opt the topic out.

## The Service Bus Event Hub destination disjunction

Azure Service Bus diagnostic settings can route to several
destinations: App Insights, Log Analytics workspace, Event
Hub, or a Storage Account. Slice 1's `has_trace_axis`
detection requires either App Insights OR Log Analytics
workspace destination. Event Hub and Storage Account are
treated as logging-only destinations:

- Namespace with diagnostic setting routing to **App Insights**:
  `has_trace_axis = true`, `has_log_axis = true`.
- Namespace with diagnostic setting routing to **Log Analytics workspace**:
  `has_trace_axis = true`, `has_log_axis = true`.
- Namespace with diagnostic setting routing to **Event Hub
  only**: `has_trace_axis = false`, `has_log_axis = true`.
  Squadron treats this as "you're logging the data flowing
  through this namespace but you're not setting up Squadron's
  traceindex to consume it directly."
- Namespace with diagnostic setting routing to **Storage Account
  only**: same as Event Hub.
- Namespace with NO diagnostic setting at all:
  `has_trace_axis = false`, `has_log_axis = false`.

If you've deliberately chosen Event Hub or Storage Account as
your observability destination (e.g. you have a custom
processor pulling from Event Hub and forwarding to your own
stack), decline the `servicebus-diagnostics-enable`
recommendation. The verdict learning loop records the
decline.

## The OCI Streaming logging proxy

OCI Streaming doesn't have direct OTel integration. Slice 1
treats OCI Logging coverage as the proxy for trace axis. If
your team uses a non-Logging observability destination
(e.g. Streams flowing to a custom processor that emits to its
own observability stack), decline the
`streaming-logging-enable` recommendation. Slice 2 may add
more granular detection when OCI exposes APM integration for
Streams directly.

## The 7 new recommendation kinds

```
eventbridge-xray-enable        pubsub-trace-enable          servicebus-diagnostics-enable
eventbridge-schemas-discover   pubsub-schema-attach         streaming-logging-enable
eventbridge-logging-enable
```

## Per-cloud Terraform patterns

### AWS EventBridge

- **`eventbridge-xray-enable`** / **`eventbridge-schemas-discover`** —
  in slice 1 both target the same Terraform pattern (slice 2
  will distinguish): `aws_schemas_discoverer description = "Squadron-recommended discoverer for bus X" source_arn = aws_cloudwatch_event_bus.<name>.arn`
- **`eventbridge-logging-enable`** —
  `aws_cloudwatch_event_target target_id = "logs" arn = aws_cloudwatch_log_group.bus.arn rule = aws_cloudwatch_event_rule.<name>.name event_bus_name = aws_cloudwatch_event_bus.<name>.name`

### GCP Pub/Sub

- **`pubsub-trace-enable`** — `google_pubsub_topic tracing_config { sampling_ratio = 1.0 }`
  (or operator-tuned floor)
- **`pubsub-schema-attach`** —
  `google_pubsub_topic schema_settings { schema = google_pubsub_schema.<name>.id encoding = "JSON" }`
  (requires existing `google_pubsub_schema` resource; the PR
  body includes the dependency note)

### Azure Service Bus

- **`servicebus-diagnostics-enable`** —
  `azurerm_monitor_diagnostic_setting target_resource_id = azurerm_servicebus_namespace.<name>.id`
  with either `workspace_id = azurerm_log_analytics_workspace.<name>.id`
  OR `application_insights_id = azurerm_application_insights.<name>.id`

### OCI Streaming

- **`streaming-logging-enable`** —
  `oci_logging_log` resource with
  `configuration { source { resource = oci_streaming_stream.<name>.id service = "streaming" category = "all" } }`

## The Event sources Inventory sub-tab

Each per-provider Discovery page (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure / DiscoveryOCI) now has a sixth Inventory
sub-tab. Unlike Orchestration (which OCI hid in slice 1), all
four providers render the Event sources sub-tab.

The Event sources table shows:

| Column        | Source                                |
|---------------|---------------------------------------|
| Resource Name | event bus / topic / namespace / stream name |
| Surface       | eventbridge / pubsub / servicebus / streaming |
| Type          | bus / topic / namespace / stream      |
| Region        | resource region                       |
| Trace axis    | ✓ if `has_trace_axis` else ✗          |
| Log axis      | ✓ if `has_log_axis` else ✗            |
| Last seen     | relative time (per v0.89.77)          |
| Quality       | dot indicator (AWS only; per established slice 1 pattern) |

QualityDot ships on AWS only — same slice 1 constraint as
v0.89.92 and v0.89.97. Slice 2 unifies across all 4 providers.

Implementation note: DiscoveryAWS uses a collapsible section
pattern (mirroring the existing Orchestration / Serverless
sections); DiscoveryGCP/Azure/OCI use the sub-tab pattern.
Same rationale as v0.89.97 — match each page's existing
architecture.

## Dashboard surfaces

### Discovery summary endpoint extension

`GET /api/v1/discovery/summary` per-provider response gains
`event_source_count`. All 4 providers populate (unlike
orchestration which left OCI at 0 in slice 1).

### Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` per-provider response
gains `event_source_pct` — % of inventoried event sources
emitting a span within 24h. All 4 providers populate.

The Discovery dashboard TRACE COVERAGE chip breakdown adds
an EVT column:

```
COMPUTE 67% | DB 42% | K8S 89% | SERVERLESS 33% | ORCH 12% | EVT 8%
```

When `event_source_pct` is zero across all 4 providers, the
EVT column hides — same pattern as the SERVERLESS and ORCH
columns.

## Webhook routing

The kind-prefix detection in the IaC webhook handler extends
with 4 new cases:

```
eventbridge-   → aws
pubsub-        → gcp
servicebus-    → azure
streaming-     → oci
```

All 7 new recommendation kinds route to the correct provider's
audit scope. SIEM consumers can filter on:

```
recommendation_kind ~= "^(eventbridge-|pubsub-|servicebus-|streaming-)"
```

## Workflow — first event source scan

1. Open the per-provider Discovery page (e.g.
   `/discovery/aws`). Note your existing connection.
2. If the connection was created before v0.89.100, you may
   need to upgrade the IAM policy / SA permissions / RBAC
   role to include the new event source API actions. The
   in-product IAM upgrade flow (#590) shows the diff.
3. Click "Run scan" — the default tier list now includes
   `event_source`. The scan walks event buses / topics /
   namespaces / streams in addition to the existing five
   tiers.
4. Click the Event sources Inventory sub-tab. Each event
   source row shows the two axes + source type + Last seen.
5. Click the Recommendations tab. Any event source missing
   the trace axis fires the corresponding recommendation.
6. Review the Terraform PR. The Reasoning text names the
   specific axis missing.
7. After merge + apply + first event flow, wait ~5 minutes.
   Re-load the Event sources sub-tab; the Last seen column
   populates for the event source.

## Reading the audit

Slice 1 reuses the existing audit event types — no new
constants. The discovery scan emits the existing
`discovery.{provider}.scan_completed` event with the
`event_source_count` field included in the payload.

The recommendation lifecycle carries the new kind values.

## Troubleshooting

- **Event buses don't appear in the Event sources sub-tab.**
  Check the IAM policy — `events:ListEventBuses`,
  `events:ListRules`, `events:ListTargetsByRule` are required.
  The in-product IAM upgrade documentation shows the diff.
- **A Pub/Sub topic with samplingRatio = 0.05 shows
  `has_trace_axis = true`.** This is correct — any value
  > 0 satisfies. The recommendation doesn't fire for this
  topic.
- **An EventBridge bus with Schemas Discoverer enabled but
  no log-target rules shows `has_trace_axis = false`.** This
  is the slice 1 detection softness. Slice 2 will detect
  Schemas directly. For now, decline the recommendation if
  your trace strategy doesn't rely on the log-target path.
- **A Service Bus namespace with diagnostic settings routing
  to Event Hub only shows `has_trace_axis = false`.** This
  is the documented disjunction — Event Hub is a logging-only
  destination in slice 1. If you have a custom processor
  pulling from Event Hub, decline the recommendation.
- **An OCI Stream with OCI Notifications configured but no
  Logging shows `has_log_axis = false`.** OCI Notifications
  isn't recognized as a Logging destination in slice 1. If
  you use Notifications for observability, decline the
  recommendation.
- **The Last seen column populates for some event sources
  but not others.** Squadron's traceindex correlates spans
  by resource identity (per the trace integration slice 1
  6-tier matching chain). Event sources that publish through
  cloud-native paths (X-Ray-aware EventBridge rules, Pub/Sub
  with tracingConfig) carry the trace context end-to-end and
  appear in the index; sources that don't emit OTel-native
  spans don't.

# Slice 2 — per-message propagation (v0.89.104 through v0.89.107)

Slice 1 surfaced whether the cloud-native trace primitive is
on at the SOURCE level: does this EventBridge bus / Pub/Sub
topic / Service Bus namespace / OCI Stream have the
observability axis enabled? Slice 2 goes one level deeper:
even when the primitive is on, the per-message control-plane
config can strip trace context before the downstream consumer
receives it. Slice 2 detects those config gaps and drafts the
PR that closes them.

The strategic frame: orphan spans on the consumer side of an
event-driven architecture are almost always a propagation
break somewhere upstream — and operators rarely have the
tooling to find which boundary breaks them. Slice 1 ruled
out "the primitive is off." Slice 2 rules out the next layer
of gaps:

- An EventBridge rule with `InputPath = "$.detail"` strips
  the X-Ray trace header (which lives outside `detail`)
  before the event reaches the target.
- A Pub/Sub topic with a schema attached but no
  `traceparent` field in the schema drops the publisher's
  traceparent at validation.
- A Service Bus namespace whose authorization rules
  restrict `ApplicationProperties` via a property-allowlist
  RBAC role blocks publishers from attaching traceparent.
- An OCI Stream with `retentionInHours < 24` may truncate
  Kafka headers (where traceparent flows on the Kafka
  protocol) in some OCI Streaming versions.

After slice 2, the Tuesday LinkedIn drumbeat narrative gains
its sharpest concrete answer to the orphan-span question yet:

> "Your EventBridge rule has X-Ray on, but it's using
> InputPath '$.detail' which strips the X-Ray trace header
> before the event reaches the Lambda. Your Lambda's spans
> look orphaned because the parent context never arrived.
> Squadron just drafted the PR to remove the InputPath and
> let the full event through."

## The per-message propagation detection

Each per-cloud scanner extended in slice 2 (chunks 1-4,
v0.89.105-v0.89.106) sets two new fields on every
EventSourceInstanceSnapshot:

- `has_propagation_config: bool` — true when the source's
  control-plane config preserves trace context end-to-end;
  false when at least one config gap would drop it.
- `propagation_notes: []string` — human-readable per-issue
  strings explaining each gap. Empty when
  `has_propagation_config` is true.

The snapshot blob carries both fields; the storage schema
stays at v13 (no migration in slice 2 — both fields live in
the `snapshot_json` JSON column the slice 1 row already
uses).

The per-cloud detection logic:

### AWS EventBridge — per-rule propagation

For each rule on the bus:

- Rule with no `InputPath` and no `InputTransformer`:
  PROPAGATION PRESERVED (full event flows through including
  the X-Ray trace header in top-level metadata).
- Rule with `InputPath = "$"`: same as no path; PRESERVED.
- Rule with `InputPath = "$.detail"` (or similar narrow
  path): PROPAGATION BROKEN. The X-Ray trace header lives
  outside `detail`; narrowing the input strips it.
- Rule with `InputTransformer` whose template includes the
  `x-amzn-trace-id` or `traceparent` literal: PRESERVED via
  heuristic match.
- Rule with `InputTransformer` template omitting the trace
  header literal: PROPAGATION BROKEN.

The bus's `has_propagation_config` is true when ALL its
rules preserve propagation (or there are no rules). A single
broken rule fails the bus axis.

### GCP Pub/Sub — schema + subscription

- Topic with no `schemaSettings`: PRESERVED (publisher owns
  attribute presence).
- Topic with `schemaSettings.schema` set: fetch the schema.
  PRESERVED if the schema includes a field named
  `traceparent` / `googclient_OpenTelemetryTraceparent` /
  `trace_context` (case-insensitive substring); BROKEN
  otherwise. Recommendation kind:
  `pubsub-schema-includes-traceparent`.
- For each subscription on the topic: PRESERVED unless the
  subscription has push delivery AND an attribute filter
  excluding the traceparent attribute key. Recommendation
  kind: `pubsub-subscription-preserves-attrs`.

### Azure Service Bus — namespace authorization rules

- Namespace with at least one `Listen + Send` rule and no
  property-restricting RBAC role at the namespace scope:
  PRESERVED.
- Otherwise (rules restricted, or a property-allowlist role
  is in place): BROKEN. Recommendation kind:
  `servicebus-policy-preserves-traceparent`.

### OCI Streaming — retention threshold

- Stream with `retentionInHours >= 24`: PRESERVED.
- Stream with `retentionInHours < 24`: BROKEN (some OCI
  Streaming versions truncate Kafka headers under short
  retention). Recommendation kind:
  `streaming-config-preserves-headers`.

## The 5 new recommendation kinds

Slice 2 adds 5 propagation kinds — one per cloud × surface
plus an extra for Pub/Sub subscriptions:

```
eventbridge-rule-preserves-trace          pubsub-schema-includes-traceparent
pubsub-subscription-preserves-attrs       servicebus-policy-preserves-traceparent
streaming-config-preserves-headers
```

These reuse the slice 1 webhook prefixes
(`eventbridge-` / `pubsub-` / `servicebus-` / `streaming-`)
so the IaC webhook routing in
`internal/api/handlers/iac_github_webhook.go` did not need
new prefix matchers in chunk 5. The
`TestWebhook_PropagationKinds_RouteToCorrectProviders`
table-driven test pins the routing for all 5 new kinds.

## The Propagation column

The Event sources sub-tab from v0.89.102 gains a new column
"Propagation" between "Log axis" and "Last seen" on every
provider's Discovery page (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure / DiscoveryOCI):

- ✓ when `has_propagation_config` is true (emerald, matching
  the slice 1 trace/log axis palette).
- ✗ when false. Rendered as an amber clickable button. The
  tooltip shows the first `propagation_notes` entry; clicking
  opens a side panel listing every note for the row.
- — (em dash) when `has_propagation_config` is undefined
  (no rules / no schema / no subscriptions to evaluate, or
  a surface the slice 2 scanner cannot inspect yet).

The amber palette on ✗ matches the slice 1 "primitive on,
config gap" convention — green ✓ on the trace/log axes
means "the primitive is on," amber ✗ on Propagation means
"the primitive is on but the config gap drops trace
context."

The notes side panel is the operator's path from "Squadron
flagged this bus" to "here's the specific rule name and
why." Each note is a single line, e.g.
`rule 'order-events' has InputPath '$.detail' that strips
trace header` or
`topic schema 'shipping-events-v3' missing traceparent
field`.

## The dashboard EVT chip propagation suffix

The Discovery dashboard TRACE COVERAGE chip breakdown EVT
column gains a `(prop N%)` suffix when the fleet-wide event
source count is non-zero. The suffix surfaces the
cross-provider weighted average of `propagation_pct` —
the per-provider count of `has_propagation_config = true`
divided by total event source count, weighted by emitting
count per provider (same aggregation as the existing tier
chips). Example:

```
EVT 80% (prop 45%)
```

This reads as: 80% of inventoried event sources have a
recent span observed, but only 45% of inventoried event
sources have the propagation config that preserves trace
context end-to-end. The gap (35 percentage points) is the
slice 2 surface — sources where the primitive is on but
trace context drops at the per-message control-plane
boundary.

The suffix hides when the fleet-wide `event_source_count`
is zero (no inventory to evaluate). Operators on a cold
install or with no event source connections see no
suffix; the EVT chip itself is already hidden on cold
install by the existing `event_source_pct = 0` rule from
slice 1.

## Reading the slice 2 outputs

The recommended path for an operator working through a
slice 2 finding:

1. Open the per-provider Discovery page Event sources
   sub-tab. Sort by the Propagation column descending; the
   amber ✗ rows surface at the top.
2. Click the ✗ button on a row. Read the side panel notes
   — each line names the specific rule / schema /
   subscription / config that breaks propagation.
3. Open the Recommendations tab. The matching
   recommendation kind (one of the 5 slice 2 kinds) is
   already drafted with Terraform that closes the gap.
4. Review the PR. The Reasoning text names the specific
   propagation gap and how the Terraform patch closes it.
5. Merge + apply. Re-scan after the first event flow; the
   row's Propagation column flips to ✓ within ~5 minutes.

## Troubleshooting

- **An EventBridge bus with mixed rules shows
  `has_propagation_config = false` even though most rules
  are fine.** This is correct — a single broken rule fails
  the bus axis. The `propagation_notes` lists every
  offending rule by name; the operator can address them
  in a single PR or one at a time. The Terraform Squadron
  drafts covers the bus axis, not per-rule.
- **A Pub/Sub topic with a schema that includes
  `trace_id` (but not `traceparent`) shows
  `has_propagation_config = false`.** The slice 2 substring
  match is `traceparent` / `googclient_OpenTelemetryTraceparent`
  / `trace_context`. `trace_id` alone is not the W3C
  traceparent header — it's the span ID, not the propagated
  context. Decline the recommendation if your schema uses a
  custom propagation convention; verdict learning records.
- **An OCI Stream with `retentionInHours = 12` deliberately
  set for cost reasons fires `streaming-config-preserves-headers`.**
  Slice 2 uses a single threshold heuristic. If you've
  deliberately set short retention, decline the
  recommendation; verdict learning records and the
  exclusion table suppresses repeat drafts.
- **A Service Bus namespace with all `Listen + Send` rules
  but a custom property-restricting RBAC role at a parent
  resource group still fires
  `servicebus-policy-preserves-traceparent`.** Slice 2
  walks the namespace scope only in chunk 3. If your
  property restriction lives at the resource group or
  subscription scope, slice 2 may miss the negative case
  (false negative) OR over-detect (false positive)
  depending on how the role binds. Slice 3 may walk parent
  scopes; until then, the exclusion table handles.
- **The Propagation column shows ✓ but downstream
  consumer spans are still orphaned.** Slice 2 detects the
  CONFIG gap, not per-message gap. A per-event-instance
  trace correlation surface is slice 3 candidate. The
  remaining orphan spans likely come from a consumer-side
  SDK that doesn't read the W3C traceparent / X-Ray
  trace header; check the consumer's OTel SDK config.

## What slice 3 will add

Per §13 of the design doc:

- Message content inspection (consumer-side substrate).
- Per-event-instance trace correlation.
- Cross-cloud event flows.
- EventBridge Pipes coverage.
- OCI Streaming consumer group config.
- Schema versioning analysis (Pub/Sub schema evolution).
- Service Bus per-entity (queue / topic / subscription)
  policy detection at the entity scope.
- Per-language SDK customization for trace context
  emission.
- Direct Schemas Discoverer detection on EventBridge
  (slice 1 uses log-target proxy).
- QualityDot column unification across all 4 providers.

## The universal claim grows a sixth tier

After event source slice 1, Squadron's positioning reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, AND drafts the IaC PRs
> that close the gaps it finds.

Four clouds. Six tiers. Four verbs. One control plane.
Twenty-four scanner surfaces (4 clouds × 5 prior tiers + 4
new event source surfaces; orchestration is still 3-cloud
pending OCI orchestration slice 2 which is honestly
qualified).

Event sources are the root of trace continuity. The
"request → orchestration → execution" chain Squadron scans
now starts at the request entry point. The Tuesday LinkedIn
drumbeat narrative gains another concrete answer to "where
did the trace go?": "your EventBridge rule didn't pass the
X-Ray sampling decision; your Lambda's spans look orphaned
because the parent context never arrived." Slice 1 ships
the visibility (does the event source have a trace primitive
on); **slice 2 (v0.89.104-v0.89.107) ships the per-message
propagation diagnosis** — does the source's config preserve
trace context end-to-end? After slice 2, an operator running
Squadron's Discovery scan gets an honest answer at TWO
levels for every inbound event source surface.

## Slice 3 SHIPPED in v0.89.137-v0.89.139 — AWS SNS

Slice 3 starts the widening pass on the event source tier
by adding AWS SNS as a second AWS surface alongside
EventBridge. Subsequent slices (4-7) will add the
corresponding second surfaces per cloud.

Honest scope: ONE new surface per arc keeps the
verification gate quality high. A 6-surface arc would
push past the soft cap multiple times.

### The new AWS surface — SNS

| Cloud | Surface | Trace axis                                                | Log axis                                          |
|-------|---------|-----------------------------------------------------------|----------------------------------------------------|
| AWS   | SNS     | `SubscriptionsConfirmed > 0` (has active downstream consumers; orphan-topic detection) | Per-protocol delivery feedback role ARN configured (http/sqs/lambda/application/firehose) |

Like the EventBridge log-target proxy from slice 1, SNS
doesn't have a direct OTel integration — Squadron uses the
per-protocol delivery feedback role attachment as the
canonical "is delivery being audited?" signal.

### The 2 new recommendation kinds

```
sns-subscriptions-attach           sns-delivery-logging-enable
```

Webhook routing: `sns- → aws`.

### sns-subscriptions-attach (audit-only)

Fires on SNS topics with zero confirmed subscriptions —
messages published get dropped on the floor. This is an
audit-only recommendation; there's NO Terraform pattern
because the operator decides:

1. **Delete the topic** if it's a leftover from a refactor
2. **Add a subscription** if a downstream consumer should
   exist but hasn't been wired up

If you intentionally keep a zero-sub topic as a placeholder,
decline. Slice 4 may add a per-resource "intentional dead
topic" flag.

### sns-delivery-logging-enable

Fires on SNS topics with active subscriptions but NO
per-protocol delivery feedback role configured. The
Terraform pattern configures all 5 protocols
(http/sqs/lambda/application/firehose) — prune the protocols
you don't use.

If you use a non-CloudWatch destination for delivery audit
(custom Lambda processor, SNS-to-Datadog integration, etc.),
decline.

### The Terraform pattern

Verbatim from §8 of the design doc — IAM role +
assume_role_policy + AmazonSNSRole policy attachment +
per-protocol feedback role ARN attachments on the
`aws_sns_topic` resource:

```hcl
data "aws_iam_policy_document" "sns_delivery_logging_<name>_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["sns.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "sns_delivery_logging_<name>" {
  name               = "sns-delivery-logging-${aws_sns_topic.<name>.name}"
  assume_role_policy = data.aws_iam_policy_document.sns_delivery_logging_<name>_assume.json
}

resource "aws_iam_role_policy_attachment" "sns_delivery_logging_<name>" {
  role       = aws_iam_role.sns_delivery_logging_<name>.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonSNSRole"
}

resource "aws_sns_topic" "<name>" {
  # ... existing fields ...

  http_success_feedback_role_arn        = aws_iam_role.sns_delivery_logging_<name>.arn
  http_failure_feedback_role_arn        = aws_iam_role.sns_delivery_logging_<name>.arn
  sqs_success_feedback_role_arn         = aws_iam_role.sns_delivery_logging_<name>.arn
  sqs_failure_feedback_role_arn         = aws_iam_role.sns_delivery_logging_<name>.arn
  lambda_success_feedback_role_arn      = aws_iam_role.sns_delivery_logging_<name>.arn
  lambda_failure_feedback_role_arn      = aws_iam_role.sns_delivery_logging_<name>.arn
  application_success_feedback_role_arn = aws_iam_role.sns_delivery_logging_<name>.arn
  application_failure_feedback_role_arn = aws_iam_role.sns_delivery_logging_<name>.arn
  firehose_success_feedback_role_arn    = aws_iam_role.sns_delivery_logging_<name>.arn
  firehose_failure_feedback_role_arn    = aws_iam_role.sns_delivery_logging_<name>.arn
}
```

### Dispatcher partial-scan posture

Slice 3 extends `ScanEventSources` to fan out across
EventBridge + SNS. If EventBridge fails (IAM lag from a
connection that predates v0.89.100) AND SNS succeeds
(slice 3 IAM update applied), the operator still sees SNS
topics in the inventory. The dispatcher returns partial
results with an honest error sum.

The IAM template extends with two new actions:

```
sns:ListTopics
sns:GetTopicAttributes
```

### Cost surface

SNS API queries are free for read operations. No new
operator-facing cost decisions per the no-money brief.

### Scan duration impact

SNS rate limits at ~30 TPS per region per account.
Squadron's existing AWS substrate rate limiter absorbs.
For a fleet of 1000 topics in one region:

- ~33 seconds added to scan duration (1000 topics × 1
  GetTopicAttributes per topic / 30 TPS)

### Slice 4+ deferrals

Per §13 of the design doc:

- **Slice 4: AWS SQS** — third AWS event source surface
- **Slice 5: GCP Cloud Tasks** — second GCP surface
- **Slice 6: Azure Event Grid + Event Hubs** — second + third
  Azure surfaces
- **Slice 7: OCI Notification Service** — second OCI surface
- **Slice 8+: subscription-level propagation analysis**,
  message filter inspection, multi-account fan-out
  coordination

## Slice 4 SHIPPED in v0.89.140-v0.89.142 — AWS SQS

Slice 4 continues the widening pass on the event source tier
by adding AWS SQS as the third AWS surface alongside
EventBridge + SNS. After slice 4, AWS has the most-complete
event source coverage of any cloud (3 surfaces); other clouds
catch up in slices 5-7.

Honest scope: ONE new surface per arc keeps the verification
gate quality high. SQS completes the canonical AWS pub/sub
fan-out architecture: `EventBridge | SNS → SQS → consumer`.

### The new AWS surface — SQS

| Cloud | Surface | Trace axis                                                | Log axis                                          |
|-------|---------|-----------------------------------------------------------|----------------------------------------------------|
| AWS   | SQS     | `RedrivePolicy` attribute set with `deadLetterTargetArn` (operational signal proxy) | DLQ ARN resolves to a queue in the same account+region (two-pass walk) |

Like SNS, SQS doesn't have a direct OTel integration. Squadron
uses the redrive policy + DLQ reachability as the canonical
"is failed-message capture configured?" signal.

### The 2 new recommendation kinds

```
sqs-redrive-policy-enable          sqs-deadletter-queue-attach
```

Webhook routing: `sqs- → aws`.

### sqs-redrive-policy-enable

Fires on SQS queues with NO RedrivePolicy configured. The
Terraform PR creates a dead-letter queue + redrive policy
targeting it with `maxReceiveCount = 5` (operator tunes).

This catches the **single most common AWS messaging
production failure**: a queue without DLQ silently drops
messages after consumer failure exhausts retries within the
queue's retention window.

Decline if your team uses a custom retry coordinator (Step
Functions retry handler, EventBridge Pipes with error
handling, etc.). The verdict learning loop records.

### sqs-deadletter-queue-attach (audit-only)

Fires on SQS queues with a RedrivePolicy set but the
`deadLetterTargetArn` doesn't resolve to a queue Squadron can
see in the same account+region. Two possibilities:

1. **Cross-account/region DLQ** — your DLQ is in a different
   account or region. Verify the source queue's IAM policy
   permits send to the DLQ ARN; declare the intent by
   declining this recommendation.
2. **Dangling reference** — the DLQ was deleted but the
   source queue's redrive policy wasn't updated. Recreate the
   DLQ OR update the redrive policy.

NO Terraform pattern — the operator confirms intent.

### The Terraform pattern (case 1: missing RedrivePolicy)

Verbatim from §8 of the design doc — `aws_sqs_queue` DLQ
resource + `redrive_policy` jsonencode block:

```hcl
resource "aws_sqs_queue" "<name>_dlq" {
  name                       = "${aws_sqs_queue.<name>.name}-dlq"
  message_retention_seconds  = 1209600  # 14 days
  kms_master_key_id          = "alias/aws/sqs"
}

resource "aws_sqs_queue" "<name>" {
  # ... existing fields ...

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.<name>_dlq.arn
    maxReceiveCount     = 5  # operator tunes
  })
}
```

### Three-way dispatcher partial-scan posture

Slice 4 extends `ScanEventSources` to fan out across
EventBridge + SNS + SQS. If two of the three surfaces fail
(IAM lag from connections that predate v0.89.100 /
v0.89.138 / v0.89.141), the third still surfaces.

The IAM template extends with two new actions:

```
sqs:ListQueues
sqs:GetQueueAttributes
```

### Cost surface

SQS API queries are free for read operations. No new
operator-facing cost decisions per the no-money brief.

### Scan duration impact

SQS rate limits at ~30 TPS per region per account.
Squadron's existing AWS substrate rate limiter absorbs.
For a fleet of 1000 queues in one region:
- ~33 seconds added to scan duration (1 GetQueueAttributes
  per queue + the two-pass DLQ resolution walk in-memory)

### The canonical pub/sub failure chain — fully visible

After slice 4, the AWS pub/sub failure chain is fully covered:

1. **SNS topic** without delivery logging (slice 3
   `sns-delivery-logging-enable`) — operator can't see
   per-message fan-out success/failure
2. **SQS queue** without redrive policy (slice 4
   `sqs-redrive-policy-enable`) — failed messages vanish
   silently
3. **Lambda consumer** without trace primitive (serverless
   tier) — even if traces flow, the consumer doesn't emit
4. **Lambda cold-start regression / error rate spike**
   (substrate's three diagnostics) — workload-health view
   shows where it broke

Four layers. One control plane. Each layer gets its own
recommendation kind + IaC PR.

### Slice 5+ deferrals

Per §13 of the design doc:
- **Slice 5: GCP Cloud Tasks** — second GCP surface
- **Slice 6: Azure Event Grid + Event Hubs** — second + third
  Azure surfaces
- **Slice 7: OCI Notification Service** — second OCI surface
- **Slice 8+: subscription-level propagation analysis**,
  per-queue depth anomaly detection using the MetricQuerier
  substrate, message filter inspection, multi-account fan-out
  coordination

## Slice 5 SHIPPED in v0.89.143-v0.89.145 — GCP Cloud Tasks

Slice 5 continues the widening pass by adding GCP Cloud Tasks
as the second GCP event source surface alongside Pub/Sub.
After slice 5, GCP comes into architectural parity with AWS
on the event source tier — both have a fan-out primitive
(EventBridge/SNS, Pub/Sub) and a queue-based primitive (SQS,
Cloud Tasks).

The canonical GCP pub/sub-with-retry architecture is
`Pub/Sub → Cloud Tasks → HTTP target`: a Pub/Sub topic fans
out to many subscribers; each subscriber adds work items to a
Cloud Tasks queue; the queue drives an HTTP endpoint with
retry-on-failure semantics.

### The new GCP surface — Cloud Tasks

| Cloud | Surface     | Trace axis                                                    | Log axis                                          |
|-------|-------------|---------------------------------------------------------------|----------------------------------------------------|
| GCP   | Cloud Tasks | `retryConfig.maxAttempts > 0` OR `-1` (unlimited)             | `stackdriverLoggingConfig.samplingRatio > 0`      |

Like SNS/SQS, Cloud Tasks doesn't have a direct OTel
integration. Squadron uses the operational signals (retry
config presence + Stackdriver Logging sampling ratio) as the
canonical "is task delivery being audited?" signal.

### The 2 new recommendation kinds

```
cloudtasks-retry-policy-enable          cloudtasks-logging-enable
```

Webhook routing: `cloudtasks- → gcp`.

### cloudtasks-retry-policy-enable

Fires on Cloud Tasks queues with `retryConfig.maxAttempts = 0`
(or retry config unset entirely). The Terraform PR configures
retry with exponential backoff (max_attempts = 5, doubling
backoff from 10s to 300s).

This catches the **canonical Cloud Tasks production failure**:
a queue without retry policy silently drops tasks when the
HTTP target returns non-2xx. Equivalent to SQS without a
redrive policy.

Decline if your team intentionally wants single-attempt
fire-and-forget semantics. The verdict learning loop records.

### cloudtasks-logging-enable

Fires on Cloud Tasks queues with
`stackdriverLoggingConfig.samplingRatio = 0` (or unset).
Configures full sampling (1.0); operators tune for
high-throughput queues.

Decline if your team uses a non-Stackdriver destination for
task audit (custom HTTP logger sidecar, etc.).

### The maxAttempts = -1 sentinel

Cloud Tasks returns `maxAttempts = -1` for unlimited retry
semantics. Slice 5 treats this as CONFIGURED retry
(HasTraceAxis = true). The recommendation doesn't fire.

If you'd rather see a bounded retry count, decline the
recommendation (the per-resource exclusion table records
the preference).

### Two-way dispatcher partial-scan posture

Slice 5 extends `ScanEventSources` to fan out across Pub/Sub +
Cloud Tasks. If Pub/Sub fails (IAM lag from connections that
predate v0.89.46) AND Cloud Tasks succeeds (slice 5 IAM
update applied), the operator still sees Cloud Tasks queues.
Same in the other direction.

The IAM template extends with two new permissions:

```
cloudtasks.queues.list
cloudtasks.queues.get
```

### Cost surface

Cloud Tasks API queries are free for read operations. No new
operator-facing cost decisions per the no-money brief.

### The canonical GCP queue-based failure chain — fully visible

After slice 5, the GCP queue-based failure chain is fully
covered:

1. **Pub/Sub topic** without delivery integration (slice 1
   `pubsub-trace-enable`)
2. **Cloud Tasks queue** without retry config (this slice
   `cloudtasks-retry-policy-enable`)
3. **Cloud Tasks queue** without Stackdriver Logging (this
   slice `cloudtasks-logging-enable`)
4. **HTTP target / Cloud Run / Cloud Functions** without
   trace primitive (serverless tier)
5. **Cloud Run / Cloud Functions cold-start regression**
   (substrate's three diagnostics)

Five layers. One control plane.

### Slice 6+ deferrals

Per §13 of the design doc:
- **Slice 6: Azure Event Grid + Event Hubs** — second + third
  Azure surfaces
- **Slice 7: OCI Notification Service** — second OCI surface
- **Slice 8+: GCP Eventarc**, per-queue depth anomaly
  detection via MetricQuerier substrate, per-task execution-
  time analysis, multi-project fan-out coordination

## Slice 6 SHIPPED in v0.89.146-v0.89.148 — Azure Event Grid

Slice 6 continues the widening pass by adding Azure Event Grid
as the second Azure event source surface alongside Service
Bus. After slice 6, Azure has 2 event source surfaces matching
GCP's count (Pub/Sub + Cloud Tasks).

Event Grid is Azure's fan-out distribution layer for cloud
events (CloudEvents 1.0 schema). The canonical Azure event
architecture is `Event Grid → Service Bus / Functions / Logic
Apps`: an Event Grid Topic publishes events; subscribers
(Service Bus queues, Functions, Logic Apps, custom webhooks)
consume them via filter rules.

### The new Azure surface — Event Grid

| Cloud | Surface    | Trace axis                                                  | Log axis                                          |
|-------|------------|-------------------------------------------------------------|----------------------------------------------------|
| Azure | Event Grid | `properties.inputSchema == "CloudEventSchemaV1_0"`          | diagnostic settings → App Insights OR Log Analytics workspace |

The trace axis uses CloudEvents 1.0 schema enforcement as the
proxy — CloudEvents 1.0 includes the distributed tracing
extension (traceparent in event extensions), while the
proprietary EventGridSchema and CustomEventSchema don't.

The log axis mirrors the slice 1 Service Bus pattern verbatim
— same Microsoft.Insights/diagnosticSettings child resource +
same destination check.

### The 2 new recommendation kinds

```
eventgrid-diagnostics-enable          eventgrid-cloudevent-schema-enforce
```

Webhook routing: `eventgrid- → azure`.

### eventgrid-diagnostics-enable

Fires on Event Grid Topics without diagnostic settings.
Terraform: `azurerm_monitor_diagnostic_setting` with 4
enabled_log categories (PublishFailures, PublishSuccess,
DeliveryFailures, DeliverySuccess) + AllMetrics, routing to
a Log Analytics workspace (operator provides workspace ID
via variable).

Decline if your team uses a non-Insights destination (custom
webhook capture, etc.).

### eventgrid-cloudevent-schema-enforce — BREAKING CHANGE

Fires on Event Grid Topics with `inputSchema = "EventGridSchema"`
or `"CustomEventSchema"`. The recommendation drafts a PR
changing to `"CloudEventSchemaV1_0"`.

⚠ **This is a BREAKING CHANGE for existing subscribers.** The
wire format changes — subscribers configured to consume the
proprietary EventGridSchema or CustomEventSchema will fail to
parse CloudEvents-formatted events.

The reasoning text emphasizes coordination with subscribers
before merging. Squadron drafts the PR; the operator's review
must catch the breakage risk. The recommended workflow:

1. Audit all subscribers consuming from the topic
2. Update each subscriber to consume CloudEvents 1.0 format
3. Deploy subscriber updates BEFORE merging the topic schema
   change
4. OR migrate to a new topic with CloudEventSchemaV1_0 and
   point subscribers at the new topic in lockstep

If your team has standardized on the proprietary Azure schema
for ecosystem reasons, decline. The verdict learning loop
records.

### The CloudEvents trace context extension

CloudEvents 1.0 carries the `traceparent` (and optionally
`tracestate`) extension in the event envelope. Subscribers
consuming CloudEvents-formatted events from an Event Grid
Topic with CloudEventSchemaV1_0 get W3C-standard trace
context for free — no per-event extraction code needed.

Combined with the trace coverage diagnostic from the existing
trace integration arc, this means CloudEvents-formatted Event
Grid → Service Bus → Functions flows propagate trace context
end-to-end without operator instrumentation.

### Two-way dispatcher partial-scan posture

Slice 6 extends `ScanEventSources` to fan out across Service
Bus + Event Grid. If Service Bus fails (RBAC propagation lag
on a connection that predates v0.89.101) AND Event Grid
succeeds (slice 6's existing read-only RBAC covers), the
operator still sees Event Grid topics. Same in the other
direction.

NO IAM extension required — the existing Azure Reader role
covers `Microsoft.EventGrid/topics/read` + the diagnostic
settings child read.

### Cost surface

Azure ARM read operations are free. No new operator-facing
cost decisions per the no-money brief.

### The canonical Azure event distribution chain — fully visible

After slice 6, the Azure event distribution chain is fully
covered:

1. **Event Grid topic** without diagnostic settings (this
   slice `eventgrid-diagnostics-enable`) — operator has no
   per-event delivery audit
2. **Event Grid topic** with proprietary schema (this slice
   `eventgrid-cloudevent-schema-enforce`) — events lose
   cross-vendor interoperability + W3C trace context
3. **Service Bus namespace** without diagnostic settings
   (slice 1 `servicebus-diagnostics-enable`) — downstream
   queue has no audit
4. **Azure Functions / Logic Apps** without trace primitive
   (serverless + orchestration tiers)
5. **Azure Functions cold-start regression** (substrate's
   three diagnostics) — workload-health view

Five layers. One control plane.

### Slice 7+ deferrals

Per §13 of the design doc:
- **Slice 7: Azure Event Hubs** — third Azure surface
- **Slice 7: OCI Notification Service** — second OCI surface
- **Slice 8+: Event Grid Domains**, Event Grid System Topics,
  per-subscription filter rule inspection, per-event
  CloudEvents payload validation, private endpoint
  configuration validation

## Slice 7 SHIPPED in v0.89.149-v0.89.151 — OCI Notification Service

Slice 7 closes the cross-cloud event source widening pass by
adding OCI Notification Service (ONS) as the second OCI
event source surface alongside Streaming. ONS serves the
pub/sub fan-out pattern — the analog of AWS SNS + GCP Pub/Sub
on the alert distribution side.

After slice 7, the cross-cloud surface count lands at
**3-2-2-2**:

- AWS: EventBridge + SNS + SQS (3 surfaces)
- GCP: Pub/Sub + Cloud Tasks (2 surfaces)
- Azure: Service Bus + Event Grid (2 surfaces)
- OCI: Streaming + Notification Service (2 surfaces)

Total: **9 event source surfaces across 4 clouds.** The
widening pass closes here.

### New recommendation kind

1 new kind in slice 7:

- `ons-logging-enable` — ONS Topic has no OCI Logging
  configuration. Without a log group capturing topic
  delivery events, the operator has no audit trail for
  which alarms / notifications were delivered to which
  subscribers — the first question in any incident
  postmortem where the operator needs to confirm "did the
  page actually get sent?".

The Logging axis mirrors the slice 1 Streaming
`streaming-logging-enable` pattern exactly. The
`listLogsForTopic` helper is structurally identical to
`listLogsForStream` from slice 1 chunk 4 — same OCI Logging
`/logs` endpoint, same `searchTerm=<ocid>` convention, same
defensive `Source.Resource` side-check.

### Terraform Squadron emits

For an `ons-logging-enable` recommendation, Squadron's IaC PR
configures an `oci_logging_log` routing the topic's delivery
events to a log group. The log group destination is parameterized
via `var.default_log_group_id` so operators with an existing
log group can reuse it without Squadron prescribing the
destination.

```hcl
resource "oci_logging_log" "<name>_delivery_log" {
  display_name = "${oci_ons_notification_topic.<name>.name}-delivery-log"
  log_group_id = var.default_log_group_id  # operator provides
  log_type     = "SERVICE"

  configuration {
    source {
      category    = "all"
      resource    = oci_ons_notification_topic.<name>.id
      service     = "notification"
      source_type = "OCISERVICE"
    }
    compartment_id = oci_ons_notification_topic.<name>.compartment_id
  }

  is_enabled         = true
  retention_duration = 30  # operator may tune
}
```

### Decline path

Decline `ons-logging-enable` if your team routes ONS audit
through a non-OCI-Logging destination — Cloud Guard custom
recipes, OCI Streaming capture, third-party SIEM connectors
all count. The verdict learning loop records the decline and
quiets the recommendation on future scans matching the same
fingerprint.

### Two-way dispatcher posture

Slice 7 extends `ScanEventSources` to fan out across
Streaming + Notifications with partial-scan posture mirroring
the slice 6 Azure two-way dispatcher:

- Streaming success + ONS success → returns union of snapshots
- One-side failure → returns the other side's snapshots
- Both-side failure → returns terminal error mentioning both
  surfaces

The compartment list call is shared between both surface
walks. When `/compartments` fails, both surfaces fail with
the same root cause (substrate-shared); the dispatcher
surfaces the error mentioning both surfaces so the audit
consumer sees the cross-surface fault domain explicitly.
Pinned by `TestScanEventSources_BothFail_MentionsBothSurfaces`.

### IAM extension

Slice 7 adds one new OCI policy statement:

```
Allow group SquadronReaders to read ons-topics in compartment <name>
```

The existing Logging read policy from slice 1 (`read log-content`
+ `read log-groups`) covers the per-topic Logging detection
call without extension.

### The OCI alerting + integration chain Squadron now sees

After slice 7, the canonical OCI workload-health chain is
fully visible end-to-end:

1. **Monitoring alarm** firing on a workload metric
   (substrate's three diagnostics: cold-start P95, sampling
   rate, error rate)
2. **ONS topic** without Logging configured (this slice
   `ons-logging-enable`) — operator has no audit of which
   subscribers got the page
3. **ONS subscription** (slice 8+ — protocol enforcement,
   retry policy tuning)
4. **Functions / OKE** without trace primitive (serverless +
   kubernetes tiers)
5. **Workload health view** — workload-health dashboard
   panel from v0.89.131-133

Five layers. One control plane. Four clouds. Fully widened
on the event source tier.

### Slice 8+ deferrals

Per §13 of the slice 7 design doc:
- **Azure Event Hubs** — third Azure surface (deferred from
  slice 7 honest deferral; design doc §1 picks ONS first
  for closing the OCI pass)
- **OCI Queue Service** — third OCI surface (transactional
  message queues, distinct primitive)
- **Per-subscription protocol enforcement** — HTTP → HTTPS
  recommendation at subscription scope
- **Per-subscription retry policy tuning** — extending
  `deliveryPolicy.maxRetryDuration` on subscriptions with
  short default retries
- **ONS Subscription confirmation lag detection** — flag
  PENDING subscriptions older than 24h
- **CMEK / vault key rotation validation** — deeper
  encryption posture
- **Per-delivery audit reconstruction** — assemble per-event
  delivery timelines from the Logging stream

## Slice 8 SHIPPED in v0.89.152-v0.89.154 — Azure Event Hubs

Slice 8 brings Azure to parity with AWS on the event source
tier at 3 surfaces. Event Hubs is Azure's big-data event
ingestion primitive — a partitioned log analogous to Kafka,
distinct from the messaging primitives (Service Bus = queue,
Event Grid = fan-out topic).

After slice 8, the cross-cloud surface count lands at
**3-2-3-2**:

- AWS: EventBridge + SNS + SQS (3 surfaces)
- GCP: Pub/Sub + Cloud Tasks (2 surfaces)
- Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
- OCI: Streaming + Notification Service (2 surfaces)

Total: **10 event source surfaces across 4 clouds.**

### New recommendation kinds

2 new kinds in slice 8:

- `eventhubs-diagnostics-enable` — Event Hubs Namespace has
  no diagnostic settings configured. Without diagnostic
  settings routing to App Insights OR a Log Analytics
  workspace, the operator has no visibility into per-namespace
  delivery health, capture status, or throughput unit
  utilization.

- `eventhubs-capture-enable` — Event Hubs Namespace has NO
  event hub with Capture enabled. Without Capture, events
  expire after the namespace's retention window (1 day
  default; 7 days max on Standard; 90 days max on Premium).
  The operator has no event-content audit trail beyond the
  retention window for incident postmortems.

The diagnostic settings axis mirrors the slice 1 Service
Bus + slice 6 Event Grid patterns exactly — same
`azurerm_monitor_diagnostic_setting` resource shape, same
log analytics workspace destination.

The Capture axis is Event-Hubs-specific. The
at-least-one-hub-enabled rule fires when ZERO hubs in the
namespace have `properties.captureDescription.enabled ==
true`. Operators routinely have multiple hubs per namespace
with different durability requirements; a blanket "every hub
must have Capture" rule would be too prescriptive.

### Terraform Squadron emits

For `eventhubs-diagnostics-enable`:

```hcl
resource "azurerm_monitor_diagnostic_setting" "<name>_diag" {
  name                       = "${azurerm_eventhub_namespace.<name>.name}-diag"
  target_resource_id         = azurerm_eventhub_namespace.<name>.id
  log_analytics_workspace_id = var.log_analytics_workspace_id  # operator provides

  enabled_log { category = "ArchiveLogs" }
  enabled_log { category = "OperationalLogs" }
  enabled_log { category = "AutoScaleLogs" }
  enabled_log { category = "KafkaCoordinatorLogs" }
  enabled_log { category = "KafkaUserErrorLogs" }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
```

For `eventhubs-capture-enable`:

```hcl
resource "azurerm_eventhub" "<hub_name>" {
  # ... existing fields ...

  # NOTE: Squadron does NOT prescribe WHICH hub to enable
  # Capture on. Operator picks based on durability-critical
  # streams during PR review.
  capture_description {
    enabled             = true
    encoding            = "Avro"
    interval_in_seconds = 300       # 5 minutes default
    size_limit_in_bytes = 314572800 # 300 MB default
    skip_empty_archives = true
    destination {
      name                = "EventHubArchive.AzureBlockBlob"
      storage_account_id  = var.capture_storage_account_id  # operator provides
      blob_container_name = "eventhub-capture"
      archive_name_format = "{Namespace}/{EventHub}/{PartitionId}/{Year}/{Month}/{Day}/{Hour}/{Minute}/{Second}"
    }
  }
}
```

### Decline paths

Decline `eventhubs-diagnostics-enable` if your team uses a
non-Insights destination (custom capture pipeline,
third-party SIEM connector). Decline
`eventhubs-capture-enable` if your team has an out-of-band
consumer pipeline doing archival (Databricks + Delta Lake
ingestion, Stream Analytics persisting to its own
destination). Verdict learning loop records both.

### Operator-prescriptive Capture recommendation

The `eventhubs-capture-enable` Terraform leaves `<hub_name>`
as a placeholder. Squadron does NOT prescribe WHICH hub to
enable Capture on — operators routinely have multiple hubs
per namespace with different durability requirements, and
Squadron cannot infer the per-hub durability profile from
the ARM API surface alone. The reasoning text emphasizes
this so PR reviewers see the intent explicitly: pick the
hub that carries the durability-critical stream.

### Three-way dispatcher posture

Slice 8 extends `ScanEventSources` from two-way (Service
Bus + Event Grid) to three-way (Service Bus + Event Grid +
Event Hubs) with combinatorial partial-scan posture
mirroring the slice 4 AWS three-way dispatcher (EventBridge
+ SNS + SQS):

- Any one of three surfaces fails → other two surfaces
  still surface their snapshots
- Any two of three surfaces fail → remaining surface still
  surfaces
- All three surfaces fail → dispatcher returns terminal
  error mentioning all three surface identifiers

Pinned by `TestScanEventSources_ThreeWay_*` (4 tests
covering each combinatorial path).

### IAM extension

NO new IAM beyond what slice 1 + slice 6 already provided.
The existing Reader role at subscription scope covers:

- `Microsoft.EventHub/namespaces/read` (namespace list)
- `Microsoft.EventHub/namespaces/eventhubs/read` (per-namespace
  hubs list for Capture detection)
- `microsoft.insights/diagnosticSettings/read` (already shared
  across Service Bus + Event Grid)

### The Azure analytics + messaging chain Squadron now sees

After slice 8, the Azure event chain is fully visible
end-to-end:

1. **Event Hubs namespace** without diagnostic settings
   (this slice `eventhubs-diagnostics-enable`) — no
   per-namespace delivery audit
2. **Event Hubs namespace** without Capture anywhere (this
   slice `eventhubs-capture-enable`) — event content lost
   after retention window
3. **Event Grid topic** without diagnostic settings (slice
   6 `eventgrid-diagnostics-enable`) — no per-event audit
4. **Service Bus namespace** without diagnostic settings
   (slice 1 `servicebus-diagnostics-enable`) — no queue
   audit
5. **Azure Functions / Logic Apps** without trace primitive
   (serverless + orchestration tiers)
6. **Azure Functions cold-start regression** (substrate's
   three diagnostics) — workload-health view

Six layers. One control plane.

### Slice 9+ deferrals

Per §13 of the slice 8 design doc:
- **OCI Queue Service** — third OCI surface (transactional
  message queues, distinct from ONS pub/sub primitive)
- **Event Hubs Geo-DR** — paired namespace pattern for
  disaster recovery
- **Per-consumer-group lag detection** — Event Hubs
  per-CG offset lag vs. tail position
- **Per-partition throughput-unit utilization** —
  auto-inflate detection via per-namespace metrics through
  the substrate's MetricQuerier
- **Schema Registry validation** — Event Hubs Schema
  Registry integration health
- **Private endpoint configuration validation** — deeper
  network access analysis

## Slice 9 SHIPPED in v0.89.155-v0.89.157 — OCI Queue Service

Slice 9 brings OCI to parity with AWS + Azure on the event
source tier at 3 surfaces. Queue Service is OCI's
transactional FIFO message queue primitive analogous to AWS
SQS — distinct from ONS pub/sub fan-out (one consumer per
message vs. many-consumer fan-out) and from Streaming
partitioned log analytics intake.

After slice 9, the cross-cloud surface count lands at
**3-2-3-3**:

- AWS: EventBridge + SNS + SQS (3 surfaces)
- GCP: Pub/Sub + Cloud Tasks (2 surfaces)
- Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
- OCI: Streaming + Notification Service + Queue Service (3 surfaces)

Total: **11 event source surfaces across 4 clouds.** Only
GCP at 2 surfaces remains for slice 10+ to close the widening
pass at **3-3-3-3 / 12 surfaces**.

### New recommendation kind

1 new kind in slice 9:

- `queues-logging-enable` — OCI Queue has no OCI Logging
  configuration. Without a log group capturing queue
  delivery events, the operator has no audit trail for
  which messages were dequeued, processed, or sent to the
  DLQ — critical for postmortem analysis of consumer-side
  failures and poison-message investigation. When a message
  lands in the DLQ at 2am the operator has no record of
  which consumer attempted it — only that the DLQ count
  incremented.

The Logging axis mirrors the slice 1 Streaming and slice 7
ONS patterns exactly via the same OCI Logging `/logs`
detection helper structure. The slice 1 `listLogsForStream`,
slice 7 `listLogsForTopic`, and slice 9 `listLogsForQueue`
helpers are parallel; a future refactor to a shared
`listLogsForOCID(resource_ocid)` is slice 10+ candidate.

### Terraform Squadron emits

```hcl
resource "oci_logging_log" "<name>_queue_log" {
  display_name = "${oci_queue_queue.<name>.display_name}-delivery-log"
  log_group_id = var.default_log_group_id  # operator provides
  log_type     = "SERVICE"

  configuration {
    source {
      category    = "all"
      resource    = oci_queue_queue.<name>.id
      service     = "queue"
      source_type = "OCISERVICE"
    }
    compartment_id = oci_queue_queue.<name>.compartment_id
  }

  is_enabled         = true
  retention_duration = 30  # operator may tune
}
```

### Decline path

Decline `queues-logging-enable` if your team routes queue
audit through a non-OCI-Logging destination — Cloud Guard
custom recipes, OCI Streaming capture, third-party SIEM
connectors all count. The verdict learning loop records.

### Three-way dispatcher posture

Slice 9 extends `ScanEventSources` from two-way (Streaming
+ Notifications) to three-way (Streaming + Notifications +
Queues) with combinatorial partial-scan posture mirroring
the slice 8 Azure three-way pattern:

- Any one of three surfaces fails → other two still surface
- Any two of three surfaces fail → remaining surface still
  surfaces
- All three surfaces fail → dispatcher returns terminal
  error mentioning all three surface identifiers

The compartment list call is shared across all three OCI
surface walks. When `/compartments` fails at substrate
level, all three surfaces fail with the same root cause;
the dispatcher's all-three-fail error string mentions all
three surfaces explicitly. Pinned by
`TestScanEventSources_ThreeWay_OCI_AllFail_ErrorMentionsAllSurfaces`.

### IAM extension

Slice 9 adds one new OCI policy statement:

```
Allow group SquadronReaders to read queues in compartment <name>
```

The existing slice 1 Logging read policy covers the
per-queue Logging detection call without extension.

### The OCI task-processing chain Squadron now sees

After slice 9, the OCI task-processing chain is fully
visible end-to-end:

1. **Streaming** intake → analytics consumers (slice 1
   `streaming-logging-enable`)
2. **ONS topic** → alert distribution (slice 7
   `ons-logging-enable`)
3. **Queue** → task processing (this slice
   `queues-logging-enable`) — operator has no audit of
   which messages were dequeued or sent to DLQ
4. **Functions / OKE** consumers without trace primitive
   (serverless + kubernetes tiers)
5. **Workload health view** — workload-health dashboard
   panel from v0.89.131-133

Five layers. One control plane. Three primitives covered on
OCI.

### Slice 10+ deferrals

Per §13 of the slice 9 design doc:
- **Third GCP surface** — Cloud Pub/Sub Lite, Cloud
  Dataflow are candidate primitives to bring GCP to 3
  surfaces (closing the widening at 3-3-3-3 / 12 surfaces)
- **DLQ configuration inspection** — per-queue
  `deadLetterQueueDeliveryCount` + redelivery policy
  analysis
- **Per-message visibility timeout analysis** —
  substrate-level analysis of consumer processing lag vs.
  visibility timeout
- **Channel-level inspection** — OCI Queue per-channel
  routing detection
- **Streaming-Queue cross-surface correlation** — when an
  OCI Streaming pipeline routes into an OCI Queue
  downstream
- **Per-queue CMEK / vault key rotation validation**
- **Shared listLogsForOCID helper refactor** — collapse
  the three parallel listLogsForStream / listLogsForTopic /
  listLogsForQueue helpers into one shared
  resource-OCID-agnostic implementation

## Slice 10 SHIPPED in v0.89.158-v0.89.160 — GCP Pub/Sub Lite — **CLOSES THE WIDENING PASS at 3-3-3-3 / 12 surfaces**

Slice 10 brings GCP to parity with AWS + Azure + OCI at 3
event source surfaces by adding Pub/Sub Lite as the third
GCP surface alongside Pub/Sub and Cloud Tasks. **This slice
closes the cross-cloud event source widening pass.**

Pub/Sub Lite is GCP's partitioned-log primitive — the
structural analog of AWS Kinesis Data Streams and Azure
Event Hubs. Distinct from full Pub/Sub in that Lite trades
managed routing + global delivery for cost efficiency at
high volume — operators self-manage partition capacity via
reservations. Zone-pinned by design.

After slice 10, the cross-cloud surface count lands at
**3-3-3-3 / 12 surfaces across 4 clouds**:

- AWS: EventBridge + SNS + SQS (3 surfaces)
- GCP: Pub/Sub + Cloud Tasks + Pub/Sub Lite (3 surfaces)
- Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
- OCI: Streaming + Notification Service + Queue Service (3 surfaces)

**Every cloud at 3 surfaces. Widening pass complete.**

### New recommendation kinds

2 new kinds in slice 10:

- `pubsublite-logging-enable` — Pub/Sub Lite topic has no
  Cloud Logging sink configured filtering on
  `resource.type="pubsublite_topic"` + the topic's ID.
  Without the sink, the operator has no audit trail for
  publish failures, per-partition throughput exhaustion
  events, or reservation-related throttling — the failure
  modes unique to the Lite tier.

- `pubsublite-reservation-attach` — Pub/Sub Lite topic has
  NO reservation attached OR the referenced reservation
  does not exist in the topic's zone. Without a
  reservation, the topic is throttled to the bare minimum
  publish + subscribe throughput per partition — typically
  becoming a silent bottleneck under peak load.

  **CRITICAL: this recommendation CREATES A BILLABLE
  RESOURCE.** This is the FIRST event source tier
  recommendation that creates a billable resource. Default
  sizing is conservative (4 publish + subscribe units) but
  the operator MUST validate against ACTUAL peak throughput
  before merging — under-sized reservations re-create the
  throttling problem the recommendation solves.

### Terraform Squadron emits

For `pubsublite-logging-enable`:

```hcl
resource "google_logging_project_sink" "<name>_lite_log_sink" {
  name        = "pubsublite-${google_pubsub_lite_topic.<name>.name}-audit"
  destination = "bigquery.googleapis.com/projects/${var.project_id}/datasets/${var.pubsublite_logging_dataset_id}"

  filter = <<-EOT
    resource.type="pubsublite_topic"
    resource.labels.topic_id="${google_pubsub_lite_topic.<name>.name}"
  EOT

  unique_writer_identity = true
}
```

For `pubsublite-reservation-attach`:

```hcl
resource "google_pubsub_lite_reservation" "<name>_reservation" {
  name    = "${google_pubsub_lite_topic.<name>.name}-reservation"
  project = var.project_id
  region  = var.lite_region  # operator provides; must match topic zone

  # CONSERVATIVE DEFAULT: 4 publish + subscribe units. Operator MUST
  # tune throughput_capacity to match ACTUAL peak before merging.
  throughput_capacity = 4
}

resource "google_pubsub_lite_topic" "<name>" {
  # ... existing fields ...

  reservation_config {
    throughput_reservation = google_pubsub_lite_reservation.<name>_reservation.name
  }
}
```

### Decline paths

Decline `pubsublite-logging-enable` if your team routes Lite
topic audit through a non-Cloud-Logging destination
(Stackdriver custom exporter, third-party SIEM). Decline
`pubsublite-reservation-attach` if your team deliberately
runs Lite topics at the minimum-throughput floor because
the topic is BELOW the per-zone reservation breakeven. The
verdict learning loop records both.

### Three-way dispatcher posture

Slice 10 extends GCP `ScanEventSources` from two-way
(Pub/Sub + Cloud Tasks) to three-way (Pub/Sub + Cloud Tasks
+ Pub/Sub Lite) with combinatorial partial-scan posture
mirroring slice 8 Azure + slice 9 OCI three-way patterns.

### Zone-pinned scope handling

Pub/Sub Lite is zone-scoped (not regional). Per-zone
partial-scan posture inside `ScanPubSubLiteTopics` handles
zone-failures independently of the three-way dispatcher
posture above it. Topics in zones the SA CAN reach surface
even when other zones fail.

### IAM extension

Slice 10 adds three new GCP IAM permissions:

- `pubsublite.topics.list`
- `pubsublite.topics.get`
- `pubsublite.reservations.list`

The existing slice 1 Logging read scope covers per-topic
detection via the project-wide Cloud Logging sinks list
call.

### The cross-cloud event source coverage matrix

After slice 10, Squadron's event source tier coverage looks
like:

```
            +-----------+-----------+--------------+
            | Compute / | Pub/Sub / | Partitioned- |
            | Queue     | Fan-out   | Log Intake   |
+-----------+-----------+-----------+--------------+
| AWS       | SQS       | SNS       | EventBridge  |
| GCP       | CloudTasks| Pub/Sub   | Pub/Sub Lite |
| Azure     | ServiceBus| Event Grid| Event Hubs   |
| OCI       | Queue     | Notif.    | Streaming    |
+-----------+-----------+-----------+--------------+
```

3-3-3-3. 12 surfaces. 4 clouds. Every cloud carries every
primitive pattern.

### Strategic close — what's next

After slice 10, the event source tier's widening pass is
**COMPLETE**. Future event source work is **per-axis depth**,
NOT per-cloud breadth:

- Per-subscription consumer-side lag detection (slice 11+)
- Cross-surface correlation views (Streaming-Queue,
  Pub/Sub-Lite consumer chains, EventBridge-SQS
  fan-in/fan-out)
- Substrate-level cost modeling for migration
  recommendations (Pub/Sub → Pub/Sub Lite, SQS → SNS+SQS
  fan-out)
- Per-message trace context propagation analysis via
  substrate MetricQuerier
- Per-message visibility timeout / consumer-processing-lag
  detection
- Schema enforcement axes across surfaces with schema
  registries

The horizon shifts from "do we see every cloud's event
source surfaces?" (yes — 3-3-3-3) to "do we see what the
operator actually needs to know about each surface?" The
substrate (MetricQuerier from cold-start slice 1+2) is the
primary enabler for the per-axis depth work — most slice
11+ candidates ride on it.

### Slice 11+ candidates

- **Per-subscription consumer-side lag detection** (all
  four clouds where applicable)
- **Cross-region disaster-recovery analysis** for Pub/Sub
  Lite, Event Hubs Geo-DR pairs, OCI Queue Geo-replication
- **Schema enforcement** axes for Pub/Sub Lite, Event
  Hubs Schema Registry
- **Pub/Sub-to-Lite migration recommendations** (requires
  substrate cost modeling)
- **Per-message trace context propagation analysis** via
  substrate MetricQuerier

## Cross-references

- [Event source tier slice 1 design doc](./proposals/event-source-tier-slice1.md) —
- [Event source tier slice 2 design doc](./proposals/event-source-tier-slice2.md) —
  the slice 2 spec this runbook extension operationalizes.
  Adds per-message propagation detection across all 4
  surfaces and 5 new recommendation kinds reusing the
  slice 1 webhook prefixes.
  the locked spec this runbook operationalizes.
- [Event source tier slice 3 design doc](./proposals/event-source-tier-slice3.md) —
  the slice 3 spec that adds AWS SNS as the second AWS
  surface and 2 new recommendation kinds
  (sns-subscriptions-attach + sns-delivery-logging-enable)
  routed via the new sns- webhook prefix.
- [Event source tier slice 4 design doc](./proposals/event-source-tier-slice4.md) —
  the slice 4 spec that adds AWS SQS as the third AWS
  surface and 2 new recommendation kinds
  (sqs-redrive-policy-enable + sqs-deadletter-queue-attach)
  routed via the new sqs- webhook prefix.
- [Event source tier slice 5 design doc](./proposals/event-source-tier-slice5.md) —
  the slice 5 spec that adds GCP Cloud Tasks as the second
  GCP surface and 2 new recommendation kinds
  (cloudtasks-retry-policy-enable + cloudtasks-logging-enable)
  routed via the new cloudtasks- webhook prefix.
- [Event source tier slice 6 design doc](./proposals/event-source-tier-slice6.md) —
  the slice 6 spec that adds Azure Event Grid as the second
  Azure surface and 2 new recommendation kinds
  (eventgrid-diagnostics-enable +
  eventgrid-cloudevent-schema-enforce — the latter is a
  BREAKING CHANGE for existing subscribers) routed via the
  new eventgrid- webhook prefix.
- [Orchestration tier slice 1](./proposals/orchestration-tier-slice1.md) —
  the prior tier-expansion arc this composes with.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the trace integration arc this composes with.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  the span quality arc whose orphan-span detector catches
  the symptom event sources surface the cause of.
- [Audit log](./audit-log.md) — full catalog of event types.
