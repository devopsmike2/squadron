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

## What slice 2 will add

Per §13 of the design doc:

- Per-message propagation analysis (Pub/Sub `googclient_OpenTelemetryTraceparent`
  attribute, EventBridge X-Ray header in detail field,
  Service Bus traceparent in ApplicationProperties).
- Per-target trace propagation on EventBridge rules.
- Schema registry inspection (EventBridge Schema Registry,
  Pub/Sub Schema).
- Dead-letter queue trace continuity.
- Cross-cloud event flows.
- Step Functions / Lambda destination correlation.
- Additional surfaces per cloud (SNS / SQS / EventHubs /
  Event Grid / Cloud Tasks / Notification Service).
- QualityDot column unification across all 4 providers.
- Direct Schemas Discoverer detection on EventBridge
  (slice 1 uses log-target proxy).

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
on); slice 2 will ship the per-message propagation diagnosis.

## Cross-references

- [Event source tier slice 1 design doc](./proposals/event-source-tier-slice1.md) —
  the locked spec this runbook operationalizes.
- [Orchestration tier slice 1](./proposals/orchestration-tier-slice1.md) —
  the prior tier-expansion arc this composes with.
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  the trace integration arc this composes with.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  the span quality arc whose orphan-span detector catches
  the symptom event sources surface the cause of.
- [Audit log](./audit-log.md) — full catalog of event types.
