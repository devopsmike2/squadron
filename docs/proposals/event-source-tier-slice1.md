# Event source tier slice 1 — sixth tier across four clouds

**Status:** design doc, locked for slice 1 implementation.
Builds on the existing compute / database / kubernetes /
serverless / orchestration tier work + the trace integration
arc + the span quality arc.

**See also:**
[Orchestration tier slice 1](./orchestration-tier-slice1.md),
[Serverless tier slice 1](./serverless-tier-slice1.md),
[Database tier slice 2](./database-tier-slice2.md),
[Kubernetes tier slice 2](./kubernetes-tier-slice2.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Span quality slice 1](./span-quality-slice1.md).

## 1. Problem

Squadron now covers 5 tiers across 4 clouds for outbound trace
emission. The "request → orchestration → execution" chain
Squadron scans goes:

- **Event source** → inbound request arrives at the cloud
- **Orchestration** → workflow sequences the request
- **Serverless / compute / k8s** → execution layer
- **Database** → persistence layer

Today Squadron sees layers 2-5. Layer 1 — the event source —
is invisible. That matters because the event source is where
the trace ID is created (or fails to be created). An
EventBridge rule that doesn't propagate the X-Ray sampling
decision to its targets breaks the trace chain at the first
hop; a Pub/Sub topic that doesn't carry traceparent in
message attributes severs context before the consumer even
runs.

This is operationally critical for three reasons:

- **The event source is the root of trace continuity.**
  Squadron's traceindex sees per-resource spans but can't tell
  the operator whether the resource's spans had a parent
  trace context that arrived via the event source. The
  orphan-span detector in span quality slice 1 catches the
  symptom; the event source tier surfaces the cause.
- **Event sources are increasingly the unit of architecture.**
  EventBridge + Lambda + DynamoDB is the canonical
  event-driven shape; Pub/Sub + Cloud Functions is its GCP
  twin; Service Bus + Functions on Azure; Streams + Functions
  on OCI. Without event source coverage, Squadron's claim of
  "we cover the cloud" has a glaring gap.
- **The competitive landscape:** Datadog APM ships event
  source tracing as a first-class surface; Honeycomb covers it
  via their EventBridge / Pub/Sub integrations; Grafana Cloud
  has dashboards specifically for it. Squadron's OSS
  positioning needs the same.

Slice 1 adds a sixth tier across all four clouds:

- **AWS**: EventBridge (event buses + rules)
- **GCP**: Pub/Sub (topics)
- **Azure**: Service Bus (namespaces + queues/topics)
- **OCI**: Streaming (Kafka-compatible streams)

For each, slice 1 detects whether the event source has the
cloud-native trace primitive enabled at the SOURCE level —
NOT whether each individual message carries traceparent. The
per-message propagation analysis is harder and slated for
slice 2.

## 2. Non-goals (slice 1)

- **Per-message propagation analysis.** Detecting whether
  every Pub/Sub message has the `googclient_OpenTelemetryTraceparent`
  attribute populated, or every EventBridge event has the
  X-Ray trace header in its detail field, requires
  per-message inspection. The substrate (subscriptions /
  consumer-side observation) isn't trivial. Slice 2.
- **Cross-cloud event flows.** A message published to AWS
  EventBridge that flows out via SNS to a GCP Pub/Sub topic
  (or via an event hub federation) — that's a real
  architecture but the trace correlation across cloud
  boundaries is its own arc. Slice 3+.
- **Per-target trace propagation analysis.** EventBridge
  rules can have multiple targets; each target may or may
  not be configured to receive the X-Ray trace header.
  Detecting per-target propagation is slice 2.
- **Schema registry inspection.** EventBridge Schema Registry
  and Schema Registry for Pub/Sub define event payload
  schemas. Squadron does not inspect schema versions for
  traceparent fields in slice 1. Slice 2 candidate.
- **Dead-letter queue trace continuity.** When messages flow
  to DLQs, the trace context they carry may not propagate
  to the alarm or remediation handler. Slice 2.
- **Step Functions targets / Lambda destinations.** These
  are technically "event-source-adjacent" but live in the
  orchestration / serverless tiers. Slice 1 keeps the
  boundary clean.
- **Auto-fix.** Squadron remains a recommender. Slice 1
  surfaces gaps + drafts PRs.

## 3. Per-cloud detection surfaces

Four event source surfaces total.

### 3.1 AWS EventBridge

API: `events:ListEventBuses`, `events:ListRules`,
`events:DescribeRule`. Required IAM in the slice 1 trust
policy update: same three actions, all read-only.

Detection axes:

| Axis                  | Source                                                | Recommendation kind                |
|-----------------------|-------------------------------------------------------|------------------------------------|
| X-Ray active tracing  | EventBus has a Schemas discovery rule pointing at X-Ray | `eventbridge-xray-enable`          |
| Schemas discovery     | EventBus has Schemas discoverer enabled              | `eventbridge-schemas-discover`     |
| Log destination set   | EventBus has a log target rule with destination ≠ null | `eventbridge-logging-enable`       |

Coverage caveat: EventBridge doesn't have a single "trace
enabled" toggle. The closest cloud-native trace primitive is
the integration with X-Ray via Schemas Discovery. Squadron
treats Schemas Discovery as the soft proxy for trace
emission readiness; per-rule X-Ray header propagation is
slice 2. The runbook documents this carefully.

### 3.2 GCP Pub/Sub

API: `pubsub.googleapis.com/v1/projects/*/topics`. Required GCP
permissions in slice 1 SA: `pubsub.topics.list`,
`pubsub.topics.get`.

Detection axes:

| Axis                  | Source                                                | Recommendation kind             |
|-----------------------|-------------------------------------------------------|---------------------------------|
| Cloud Trace integration | Topic has `tracingConfig.samplingRatio > 0`         | `pubsub-trace-enable`           |
| Schema attached       | Topic has `schemaSettings.schema` set                | `pubsub-schema-attach`          |
| Message storage policy | Topic has `messageStoragePolicy.allowedPersistenceRegions` not empty | `pubsub-storage-policy`     |

GCP Pub/Sub has a first-class `tracingConfig.samplingRatio`
field — set this above 0 and Cloud Trace receives spans for
publish operations. Squadron treats > 0 as HasTraceAxis; the
recommendation drafts a PR setting it to 1.0 (or operator-
configured floor).

### 3.3 Azure Service Bus

API: `Microsoft.ServiceBus/namespaces` via Resource Manager.
Required Azure RBAC: existing Reader role on the resource
group.

Detection axes:

| Axis                          | Source                                          | Recommendation kind            |
|-------------------------------|-------------------------------------------------|--------------------------------|
| Diagnostic settings configured | Namespace has Microsoft.Insights/diagnosticSettings child routing to App Insights or Log Analytics | `servicebus-diagnostics-enable` |
| Minimum TLS version           | `properties.minimumTlsVersion >= 1.2`           | informational only             |

Azure Service Bus exposes trace via diagnostic settings
flowing to App Insights or Log Analytics. The detection mirrors
the Logic Apps Consumption tier path from v0.89.96 chunk 3.

### 3.4 OCI Streaming

API: `streaming.ListStreams`, `streaming.GetStream`. Required
OCI policy: `inspect streams in compartment`.

Detection axes:

| Axis                  | Source                                                | Recommendation kind             |
|-----------------------|-------------------------------------------------------|---------------------------------|
| Logging configured    | Stream has Logging service log group attached         | `streaming-logging-enable`      |
| Encryption at rest    | `streamPool.kmsKeyId` is set                          | informational only              |

OCI Streaming doesn't have a direct OTel integration; the
closest signal is whether OCI Logging is capturing stream
events. Slice 1 treats this as the trace axis proxy. Slice 2
may add a more granular detection when OCI exposes APM
integration for streams.

## 4. Storage schema

New `event_source_instance` storage table. Schema migration
v12 → v13.

```sql
CREATE TABLE IF NOT EXISTS event_source_instance (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL,
    scan_id TEXT NOT NULL,
    provider TEXT NOT NULL, -- aws / gcp / azure / oci
    surface TEXT NOT NULL, -- eventbridge / pubsub / servicebus / streaming
    account_id TEXT NOT NULL,
    region TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    resource_arn TEXT,
    source_type TEXT, -- bus / topic / queue / namespace / stream
    has_trace_axis INTEGER NOT NULL,
    has_log_axis INTEGER NOT NULL,
    last_seen_at TIMESTAMP,
    snapshot_json TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (connection_id, scan_id, resource_arn)
);
CREATE INDEX IF NOT EXISTS idx_event_source_scan ON event_source_instance(scan_id);
CREATE INDEX IF NOT EXISTS idx_event_source_conn ON event_source_instance(connection_id);
```

Schema version bumps to v13. Migration is idempotent.

## 5. Scanner contract

Each per-cloud scanner gains `ScanEventSources(ctx, scope)`.
The EventSourceInstanceSnapshot struct lives in
`internal/discovery/scanner/scanner.go`:

```go
type EventSourceInstanceSnapshot struct {
    Provider      string         `json:"provider"`
    Surface       string         `json:"surface"`   // eventbridge / pubsub / servicebus / streaming
    AccountID     string         `json:"account_id"`
    Region        string         `json:"region"`
    ResourceName  string         `json:"resource_name"`
    ResourceARN   string         `json:"resource_arn"`
    SourceType    string         `json:"source_type,omitempty"` // bus / topic / queue / namespace / stream
    HasTraceAxis  bool           `json:"has_trace_axis"`
    HasLogAxis    bool           `json:"has_log_axis"`
    LastSeenAt    *time.Time     `json:"last_seen_at,omitempty"`
    Detail        map[string]any `json:"detail,omitempty"`
}
```

Per-cloud scanner files:
- `internal/discovery/aws/eventbridge.go`
- `internal/discovery/gcp/pubsub.go`
- `internal/discovery/azure/servicebus.go`
- `internal/discovery/oci/streaming.go`

## 6. API surface

### 6.1 Per-provider scan endpoint extension

`POST /api/v1/discovery/{provider}/scan` accepts
`"event_source"` as a valid tier value. Default tier list
extends from
`[compute, database, kubernetes, serverless, orchestration]`
to `[compute, database, kubernetes, serverless, orchestration, event_source]`.

For OCI, default tier list extends from 4 tiers to 5 (orchestration
remains deferred from slice 1 but event_source is included
fully; OCI Streaming is shape-compatible with the cross-cloud
detection).

### 6.2 Per-provider inventory endpoint extension

Response shape gains `event_sources: []` field on all four
providers:

```json
{
  "compute": [...],
  "databases": [...],
  "clusters": [...],
  "serverless": [...],
  "orchestrations": [...],
  "event_sources": [...]
}
```

### 6.3 Discovery summary endpoint extension

`GET /api/v1/discovery/summary` per-provider response gains
`event_source_count` field. All 4 providers populate.

### 6.4 Trace coverage endpoint extension

`GET /api/v1/discovery/trace_coverage` per-provider response
gains `event_source_pct` field — % of inventoried event
sources emitting in 24h. All 4 providers populate.

## 7. UI

Each per-provider page (DiscoveryAWS / DiscoveryGCP /
DiscoveryAzure / DiscoveryOCI) gains a sixth Inventory
sub-tab:

> [ Compute ]  [ Databases ]  [ Kubernetes ]  [ Serverless ]  [ Orchestration ]  [ Event sources ]

For DiscoveryOCI the Orchestration tab remains hidden (slice
1 contract from v0.89.94) but Event sources renders for OCI.

Event sources sub-tab columns:
- Resource Name
- Surface (eventbridge / pubsub / servicebus / streaming)
- Type (bus / topic / queue / namespace / stream)
- Region
- Trace axis (✓ / ✗)
- Log axis (✓ / ✗)
- Last seen (relative time)
- Quality dot (AWS only in slice 1 per the established pattern)

The Discovery dashboard TRACE COVERAGE panel chip breakdown
adds an "EVT" column:

```
COMPUTE 67% | DB 42% | K8S 89% | SERVERLESS 33% | ORCH 12% | EVT 8%
```

The chip line hides when zero across all providers.

## 8. Recommendation kinds

7 new kinds across the 4 surfaces:

```
eventbridge-xray-enable        pubsub-trace-enable          servicebus-diagnostics-enable
eventbridge-schemas-discover   pubsub-schema-attach         streaming-logging-enable
eventbridge-logging-enable
```

(pubsub-storage-policy not slated as a recommendation kind in
slice 1 — it's informational only because storage region
choice is operator policy, not observability.)

Webhook kind-prefix routing extends:
- `eventbridge-` → AWS
- `pubsub-` → GCP
- `servicebus-` → Azure
- `streaming-` → OCI

The 4 new prefixes extend the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

## 9. Slice 1 contract

**In:**

1. Storage schema v12 → v13 with `event_source_instance` table.
2. EventSourceInstanceSnapshot struct on scanner package.
3. ScanEventSources methods on all 4 provider scanners (one
   surface per cloud in slice 1).
4. Per-provider scan + inventory endpoint extensions.
5. Discovery summary + trace_coverage endpoint extensions.
6. Per-provider page Event sources sub-tab (4 pages).
7. Dashboard TRACE COVERAGE chip breakdown gains EVT column.
8. Proposer prompt extension with 7 new recommendation kinds.
9. Webhook routing for the 4 new kind prefixes.
10. Operator runbook covering all the above.
11. Acceptance tests covering all 4 surfaces' scanner
    detection, endpoint shape, UI rendering, cold-start parity.

**Out:**

- Per-message propagation analysis.
- Cross-cloud event flows.
- Per-target trace propagation.
- Schema registry inspection.
- DLQ trace continuity.
- Step Functions / Lambda destination correlation.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation + AWS EventBridge scanner.**
  EventSourceInstanceSnapshot struct, storage migration
  v12→v13, AWS EventBridge scanner (simplest of the 4 — single
  API surface), scan endpoint tier extension. ~900-1100 lines.
  **v0.89.100.**
- **Chunk 2: GCP Pub/Sub scanner.** Parallel-eligible with
  chunks 3+4. ~500-700 lines. **v0.89.101.**
- **Chunk 3: Azure Service Bus scanner.** Parallel-eligible.
  ~500-700 lines. **v0.89.101.**
- **Chunk 4: OCI Streaming scanner.** Parallel-eligible.
  ~500-700 lines. **v0.89.101.**
- **Chunk 5: Proposer prompt + UI Event sources sub-tab +
  webhook routing + summary/coverage extensions.** ~1000-1200
  lines. **v0.89.102.**
- **Chunk 6: Operator runbook + dashboard chip extension +
  README index entry.** ~400-500 lines. **v0.89.103.**

Total: ~5 release tags. Parallel scanner fan-out reduces tag
count.

## 11. Acceptance tests

1. **AWS EventBridge scanner — bus with Schemas discoverer
   enabled.** Mock `events:ListEventBuses` to return a bus
   with `schemasDiscoverer = "Active"`. Assert: HasTraceAxis=true.
2. **AWS EventBridge scanner — bus without Schemas.** Assert:
   HasTraceAxis=false.
3. **AWS EventBridge scanner — bus with log-target rule.**
   Mock `events:ListRules` for the bus with a rule pointing
   at a CloudWatch Logs target. Assert: HasLogAxis=true.
4. **GCP Pub/Sub scanner — topic with samplingRatio=1.0.**
   Assert: HasTraceAxis=true.
5. **GCP Pub/Sub scanner — topic with samplingRatio=0.**
   Assert: HasTraceAxis=false.
6. **GCP Pub/Sub scanner — topic with schemaSettings set.**
   Assert: snapshot Detail includes the schema reference.
7. **Azure Service Bus — namespace with diagnostic settings
   to App Insights.** Assert: HasTraceAxis=true.
8. **Azure Service Bus — namespace with diagnostic settings
   to Log Analytics.** Assert: HasTraceAxis=true (either
   destination satisfies).
9. **Azure Service Bus — namespace without diagnostic
   settings.** Assert: HasTraceAxis=false.
10. **OCI Streaming — stream with Logging log group.**
    Assert: HasLogAxis=true.
11. **OCI Streaming — stream without Logging.** Assert:
    HasLogAxis=false.
12. **Storage migration v12→v13 idempotent.** Run migration
    twice; no error, table exists.
13. **Discovery summary includes event_source_count.** Seed
    inventory with 4 event sources; assert per-provider
    count=4.
14. **Trace coverage includes event_source_pct.** Seed
    inventory with 2 event sources, 1 emitting; assert
    event_source_pct=50.
15. **Webhook routes eventbridge-xray-enable to aws.**
16. **Webhook routes pubsub-trace-enable to gcp.**
17. **Webhook routes servicebus-diagnostics-enable to azure.**
18. **Webhook routes streaming-logging-enable to oci.**
19. **Cold-start parity preserved.** All 4 providers
    cold-start prompts byte-identical to v0.89.98 when no
    event source rows trigger recommendations.

## 12. Threat model

**Wider API permission requests.** Each cloud's slice 1 IAM
template grows to cover the new event source API calls:
- AWS: `events:ListEventBuses`, `events:ListRules`,
  `events:DescribeRule` (3 new actions).
- GCP: `pubsub.topics.list`, `pubsub.topics.get` (2 new).
- Azure: existing Reader role covers Service Bus.
- OCI: `inspect streams in compartment` (1 new policy
  statement).

All read-only. Operators get the in-product IAM upgrade flow
(#590) extended with the event source actions.

**EventBridge Schemas detection softness.** Slice 1 treats
Schemas Discoverer as the proxy for trace readiness. An
operator who deliberately disabled Schemas (because they
have their own schema registry) gets false-positive
`eventbridge-schemas-discover` recommendations. The verdict
learning loop records declines; runbook documents the
trade-off.

**Pub/Sub samplingRatio=0 ambiguity.** A topic with no
`tracingConfig` (the field defaults to absent, treated as
samplingRatio=0) is indistinguishable from a topic explicitly
set to 0. Both produce HasTraceAxis=false in slice 1. The
recommendation reasoning doesn't distinguish; the Terraform
PR sets `tracing_config { sampling_ratio = 1.0 }` either way.
Acceptable.

**OCI Streaming logging proxy.** OCI Streaming doesn't have
direct OTel integration. Slice 1 treats OCI Logging coverage
as a proxy for trace axis. Operators may decline
`streaming-logging-enable` if they use a non-Logging
destination (e.g. Streams flowing to a custom processor that
emits to its own observability stack). Verdict learning loop
captures.

**No per-message inspection.** Slice 1 explicitly does NOT
inspect message contents or attributes. PII surface stays
zero. Slice 2 may extend; the threat model will be revisited.

## 13. Slice 2 candidates

- Per-message propagation analysis (Pub/Sub
  googclient_OpenTelemetryTraceparent attribute, EventBridge
  X-Ray header in detail field, Service Bus traceparent in
  ApplicationProperties).
- Per-target trace propagation on EventBridge rules.
- Schema registry inspection (EventBridge Schema Registry,
  Pub/Sub Schema).
- Dead-letter queue trace continuity.
- Cross-cloud event flows.
- Step Functions / Lambda destination correlation back to
  the event source.
- Additional surfaces per cloud (SNS / SQS / EventHubs /
  Event Grid / Cloud Tasks / Notification Service).
- QualityDot column unification across all 4 providers.

---

**Strategic frame:**

Squadron's universal claim grows from five tiers to six:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
> AND EVENT SOURCES for observability gaps, verifies
> telemetry is actually flowing, validates the spans
> Squadron receives are healthy, AND drafts the IaC PRs
> that close the gaps it finds.

Four clouds. Six tiers. Four verbs. One control plane.
Twenty-four scanner surfaces (4 clouds × 5 prior tiers + 4
new event source surfaces). The honest framing: slice 1
covers ONE event source surface per cloud — there are more
(SNS / SQS / EventHubs / Event Grid / Cloud Tasks /
Notification Service) which slice 2+ will add.

Event sources are the root of trace continuity. The "request
→ orchestration → execution" chain Squadron scans now starts
at the request entry point. The Tuesday LinkedIn drumbeat
narrative gains another concrete answer to "where did the
trace go?": "your EventBridge rule didn't pass the X-Ray
sampling decision; the Lambda's spans look orphaned because
the parent context never arrived." Slice 1 ships the
visibility (does the event source have a trace primitive on);
slice 2 will ship the per-message propagation diagnosis.
