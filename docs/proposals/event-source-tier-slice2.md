# Event source tier slice 2 — per-message propagation

**Status:** design doc, locked for slice 2 implementation.
Builds directly on event source tier slice 1 (v0.89.99
through v0.89.103), which shipped per-cloud detection of
whether each event source has its cloud-native trace
primitive enabled. Slice 2 goes deeper: detect whether trace
context actually propagates through the event payload.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Orchestration tier slice 1](./orchestration-tier-slice1.md),
[Span quality slice 1](./span-quality-slice1.md),
[Trace integration slice 1](./trace-integration-slice1.md),
[Trace integration slice 2](./trace-integration-slice2.md).

## 1. Problem

Slice 1 of event source tier ships detection at the SOURCE
level — does this EventBridge bus / Pub/Sub topic / Service
Bus namespace / OCI Stream have the cloud-native trace
primitive enabled? That's good, but it's incomplete.

A topic with `tracingConfig.samplingRatio = 1.0` set tells
you GCP Pub/Sub is sampling publish operations into Cloud
Trace. It does NOT tell you whether every message published
to that topic carries `googclient_OpenTelemetryTraceparent`
in its attributes — without that header, the downstream
consumer's spans look like fresh-trace orphans even though
the publish-side primitive is on.

This is the canonical "where did my trace go?" surface
Squadron's universal claim promises to close. After slice 1,
operators can see "the event source has trace on" but still
can't tell:

- Are EventBridge rules using `InputPath` or `InputTransformer`
  configs that strip the X-Ray trace header from the event
  detail field?
- Does the Pub/Sub topic have a schema attached? If yes, does
  that schema include a `traceparent` field, or is the
  publisher's traceparent dropped at validation?
- Does the Service Bus namespace shared access policy permit
  custom application properties (where traceparent lives), or
  is it restricted to a fixed property set?
- Does the OCI Stream retention configuration preserve Kafka
  message headers (where traceparent flows on Kafka protocol),
  or is the retention policy header-truncating?

Slice 2 surfaces these gaps at the control-plane metadata
layer. Each gap becomes a new recommendation kind. The
operator's IaC PR adjusts the config; the next message
published carries traceparent end-to-end; downstream
consumer spans correlate to the upstream source span.

This matters because the orphan-span detector in span quality
slice 1 catches the SYMPTOM (spans with parent_id unresolvable
in the 5min window). Slice 2 of event source tier surfaces
the CAUSE for the event-source-mediated subset of those
orphans.

## 2. Non-goals (slice 2)

- **Message content inspection.** Slice 2 stays read-only on
  control-plane metadata. It does NOT subscribe to the event
  source, does NOT inspect message payload, does NOT inspect
  message attributes/headers/properties. The substrate to do
  that would require consumer-side subscriptions which carry
  significant PII concerns; deferred to slice 3 or never
  (Squadron may always stay control-plane-only).
- **Per-event-instance trace correlation.** Slice 2 detects
  whether the CONFIG would preserve trace context. It does
  NOT detect whether a specific event instance actually
  carried trace context. Slice 3 may add per-instance
  correlation via consumer-side traceindex Observation (the
  same hot path the span quality arc uses).
- **Cross-cloud event flows.** Slice 1 deferred this; slice
  2 keeps the deferral.
- **Schema versioning analysis.** Pub/Sub Schema Registry
  supports schema evolution. Slice 2 inspects the latest
  schema version. Slice 3 may check whether the operator
  added traceparent in a non-default schema version, etc.
- **EventBridge Pipes.** EventBridge Pipes (the source →
  filter → transformer → target abstraction) is a 2023+
  feature that adds transformation steps Pipes can use to
  drop or rewrite headers. Slice 1 + slice 2 don't cover
  Pipes; slice 3+ candidate.
- **OCI Streaming consumer group configuration.** Slice 2
  inspects stream-level config; consumer group config that
  may affect header propagation is slice 3.
- **Auto-fix.** Squadron remains a recommender.

## 3. Per-cloud detection surfaces

### 3.1 AWS EventBridge — per-rule propagation config

API: `events:DescribeRule` (already in slice 1's required
actions). The rule's `EventPattern` + per-target
`InputPath` / `InputTransformer` fields tell Squadron whether
the rule's configured to preserve trace headers.

Detection logic:

- **Rule has no `InputPath` and no `InputTransformer`**: the
  full event flows through to the target including the X-Ray
  trace header in `detail` (when present). PROPAGATION
  PRESERVED.
- **Rule has `InputPath = "$"`**: same as no path —
  full event. PROPAGATION PRESERVED.
- **Rule has `InputPath = "$.detail"` (or similar narrow
  path)**: only the `detail` field flows, but the X-Ray
  trace header is OUTSIDE detail (in the event's top-level
  metadata). PROPAGATION BROKEN. Recommendation:
  `eventbridge-rule-preserves-trace`.
- **Rule has `InputTransformer` with `inputPathsMap` and a
  `template` that omits the X-Ray trace header path**:
  PROPAGATION BROKEN unless the transformer explicitly
  includes the trace header. Slice 2 detects via heuristic:
  template contains `"x-amzn-trace-id"` or `"traceparent"`
  literal string → PRESERVED. Otherwise BROKEN.

This is a per-rule detection. The slice 1 event source
inventory is per-bus; slice 2 adds a per-rule sub-resource
inspection. The HasPropagationConfig axis on the bus is true
when ALL its rules have propagation preserved (or there are
no rules). False if any rule breaks propagation. The
recommendation kind targets the bus + offending rule names.

### 3.2 GCP Pub/Sub — topic schema + subscription delivery

APIs: `pubsub.googleapis.com/v1/projects/*/topics/*`
(already), plus `pubsub.googleapis.com/v1/projects/*/schemas/*`
to fetch attached schemas, plus
`pubsub.googleapis.com/v1/projects/*/subscriptions` to
inspect subscription configs.

Detection logic — **topic schema axis:**

- **Topic has no `schemaSettings`**: no schema enforcement,
  publisher controls attribute presence. PROPAGATION
  PRESERVED (publisher's responsibility, not Pub/Sub's).
- **Topic has `schemaSettings.schema` set**: fetch the schema
  definition. Check whether the schema includes a field with
  name matching `traceparent` / `googclient_OpenTelemetryTraceparent`
  / `trace_context` (case-insensitive substring match).
  - If included: PROPAGATION PRESERVED.
  - If not included: schema enforcement may drop or reject
    messages with traceparent attributes. PROPAGATION
    POTENTIALLY BROKEN. Recommendation:
    `pubsub-schema-includes-traceparent`.

Detection logic — **subscription delivery axis:**

- For each subscription on the topic, check
  `pushConfig.attributes` and `bigqueryConfig` /
  `cloudStorageConfig`. If the subscription is push-mode AND
  has explicit attribute filters that don't include the
  traceparent attribute key → PROPAGATION POTENTIALLY
  BROKEN. Recommendation: `pubsub-subscription-preserves-attrs`.

This is a per-topic detection that may emit two recommendation
kinds. The HasPropagationConfig axis is true when both
sub-axes are satisfied.

### 3.3 Azure Service Bus — namespace shared access policy

API: `Microsoft.ServiceBus/namespaces/{name}/authorizationRules`
via Resource Manager (no new IAM beyond the existing Reader
role).

Detection logic:

- **Namespace has at least one authorizationRule with `Listen`
  AND `Send` rights**: standard SDK usage; publishers can
  attach `ApplicationProperties` including traceparent;
  consumers can read them. PROPAGATION PRESERVED.
- **Namespace has only `Listen`-only rules**: read-only
  access; not a propagation concern but flag in Detail.
- **Namespace has only `Send`-only rules**: write-only;
  same.
- **Namespace has a rule with restricted property allowlist
  (via `requiredClaims`)**: this is unusual but possible via
  Azure RBAC role assignments at the entity level. Slice 2
  detects this via the role assignment scope; if a custom
  role with property restrictions is in place,
  PROPAGATION POTENTIALLY BROKEN.

For slice 2, the simpler heuristic: if the namespace has at
least one `Send`-capable rule AND no property-restricting
RBAC role, PROPAGATION PRESERVED. Otherwise emit the
`servicebus-policy-preserves-traceparent` recommendation.

### 3.4 OCI Streaming — retention + Kafka header config

API: `streaming.GetStream` (already in slice 1's calls).
OCI Streaming uses Kafka protocol on the wire; Kafka
messages carry headers including traceparent. The stream's
config controls header retention.

Detection logic:

- **Stream `retentionInHours >= 24`**: standard retention;
  headers preserved for the retention window. PROPAGATION
  PRESERVED.
- **Stream `retentionInHours < 24`**: short retention may
  truncate headers in some OCI Streaming versions (the
  per-message metadata budget shrinks with shorter retention).
  PROPAGATION POTENTIALLY BROKEN. Recommendation:
  `streaming-config-preserves-headers`.

For slice 2 this is the simplest detection — a single
threshold check. Slice 3 may add consumer-group-level
detection.

## 4. Storage schema

NO schema migration in slice 2. The existing
`event_source_instance` table (v0.89.100, schema v13) gains
a new field in its `snapshot_json` blob:
`has_propagation_config: bool` and `propagation_notes: []`.

The bool is stored in the JSON; the schema stays at v13.
This avoids the operational risk of yet another migration
for an additive field.

For the runtime in-memory snapshot, extend the existing
struct:

```go
type EventSourceInstanceSnapshot struct {
    // ... existing fields ...
    
    HasPropagationConfig bool     `json:"has_propagation_config"`
    PropagationNotes     []string `json:"propagation_notes,omitempty"` // human-readable per-issue notes
}
```

The `PropagationNotes` field carries specific per-issue
strings: "rule 'foo' has InputPath '$.detail' that strips
trace header", "topic schema 'bar' missing traceparent field".
The proposer uses these notes in the recommendation
reasoning text.

## 5. Scanner contract

Each per-cloud scanner extends its existing
`ScanEventSources` method to populate the new
`HasPropagationConfig` + `PropagationNotes` fields. No new
methods on the scanner interface.

The per-cloud detection logic lives alongside the slice 1
files:
- `internal/discovery/aws/eventbridge.go` extends with rule
  propagation detection.
- `internal/discovery/gcp/pubsub.go` extends with schema
  + subscription detection.
- `internal/discovery/azure/servicebus_scanner.go` extends
  with authorizationRules detection.
- `internal/discovery/oci/scanner_streaming.go` extends
  with retention detection.

## 6. API surface

Existing endpoints continue to work. Slice 2's new fields
flow through the existing JSON marshalling. No new endpoints.

`GET /api/v1/discovery/{provider}/inventory` response:

```json
{
  "event_sources": [
    {
      "provider": "aws",
      "surface": "eventbridge",
      "resource_name": "default",
      "has_trace_axis": true,
      "has_log_axis": true,
      "has_propagation_config": false,
      "propagation_notes": [
        "rule 'order-events' has InputPath '$.detail' that strips trace header",
        "rule 'shipping-events' has InputTransformer template omitting x-amzn-trace-id"
      ]
    }
  ]
}
```

The trace_coverage endpoint gains a `propagation_pct` field
on the per-provider response — % of event sources whose
HasPropagationConfig is true.

## 7. UI

The Event sources sub-tab from v0.89.102 gains a new column
"Propagation" between the existing "Log axis" and "Last
seen" columns:

- ✓ if `has_propagation_config` is true
- ✗ if false (the row is also flagged with a small hover
  tooltip showing the first `propagation_notes` entry)
- — (em dash) if there are no rules / no schema / no
  subscriptions to evaluate

Clicking the ✗ opens a side panel showing all
`propagation_notes` for the row.

The dashboard TRACE COVERAGE panel chip breakdown adds a
small "(propagation: N%)" suffix to the EVT column when
propagation_pct is computable:

```
EVT 8% (prop 23%)
```

The suffix is omitted when there are no event sources or
when propagation can't be computed.

## 8. Recommendation kinds

5 new kinds, one per cloud × surface plus an extra for
Pub/Sub subscriptions:

```
eventbridge-rule-preserves-trace          pubsub-schema-includes-traceparent     servicebus-policy-preserves-traceparent
                                          pubsub-subscription-preserves-attrs    streaming-config-preserves-headers
```

Webhook routing extends the existing prefix matchers from
slice 1 — no new prefixes; the new kinds match the existing
`eventbridge-` / `pubsub-` / `servicebus-` / `streaming-`
prefixes already in place.

## 9. Slice 2 contract

**In:**

1. EventSourceInstanceSnapshot gains `has_propagation_config`
   + `propagation_notes` fields.
2. Per-cloud detection logic extends in the 4 slice 1
   scanners.
3. snapshot_json blob carries the new fields (schema stays
   at v13).
4. Trace coverage endpoint gains `propagation_pct` per
   provider.
5. UI Event sources sub-tab gains Propagation column +
   notes side panel.
6. Dashboard TRACE COVERAGE chip gains propagation suffix
   in EVT column.
7. Proposer prompt extension with 5 new recommendation kinds.
8. Operator runbook covering all the above.
9. Acceptance tests covering all 4 surfaces' propagation
   detection, the new endpoint field, the new UI column,
   cold-start parity.

**Out:**

- Message content inspection.
- Per-event-instance trace correlation.
- Cross-cloud event flows.
- Schema versioning analysis.
- EventBridge Pipes.
- OCI Streaming consumer group config.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Foundation + AWS EventBridge per-rule detection.**
  EventSourceInstanceSnapshot extension, AWS rule propagation
  detection logic, trace_coverage endpoint propagation_pct.
  ~900-1100 lines. **v0.89.105.**
- **Chunk 2: GCP Pub/Sub schema + subscription detection.**
  Parallel-eligible. ~600-800 lines. **v0.89.106.**
- **Chunk 3: Azure Service Bus policy detection.**
  Parallel-eligible. ~500-700 lines. **v0.89.106.**
- **Chunk 4: OCI Streaming retention detection.**
  Parallel-eligible. ~400-600 lines. **v0.89.106.**
- **Chunk 5: Proposer prompt + UI Propagation column +
  dashboard suffix + runbook.** ~1000-1200 lines.
  **v0.89.107.**

Total: 3 release tags. Parallel scanner fan-out for chunks
2-4 lands in a single release.

## 11. Acceptance tests

1. **EventBridge rule with no InputPath and no
   InputTransformer**: propagation preserved.
2. **EventBridge rule with `InputPath = "$"`**: same as no
   path; propagation preserved.
3. **EventBridge rule with `InputPath = "$.detail"`**:
   propagation broken; PropagationNotes records the rule
   name and reason.
4. **EventBridge rule with InputTransformer including
   `x-amzn-trace-id` in template**: propagation preserved.
5. **EventBridge rule with InputTransformer template
   omitting trace header**: propagation broken.
6. **EventBridge bus with mixed rules** (some preserve,
   some don't): HasPropagationConfig = false (any broken
   rule fails the bus axis).
7. **Pub/Sub topic with no schemaSettings**: propagation
   preserved (publisher controls).
8. **Pub/Sub topic with schema including `traceparent`
   field**: propagation preserved.
9. **Pub/Sub topic with schema NOT including `traceparent`
   field**: propagation broken; PropagationNotes records.
10. **Pub/Sub subscription with attribute filter excluding
    traceparent**: propagation broken.
11. **Service Bus namespace with at least one Listen+Send
    authorizationRule and no property restrictions**:
    propagation preserved.
12. **Service Bus namespace with only Send-only rules and
    no property restrictions**: propagation preserved (with
    informational note).
13. **OCI Stream with retentionInHours >= 24**: propagation
    preserved.
14. **OCI Stream with retentionInHours < 24**: propagation
    broken; PropagationNotes records.
15. **Trace coverage endpoint returns propagation_pct**.
16. **Cold-start parity preserved**. All 4 providers
    cold-start prompts byte-identical to v0.89.103 when no
    event sources trigger propagation recommendations.

## 12. Threat model

**No new external surface.** Slice 2 reuses the existing
slice 1 API calls. EventBridge `DescribeRule` is already
in the IAM template. Pub/Sub `schemas.get` requires
`pubsub.schemas.get` — slight permission expansion. Service
Bus `authorizationRules` listing requires the existing
Reader role. OCI Streaming retention is read on the same
GetStream call.

**False-positive risk on EventBridge InputTransformer.**
Slice 2 uses heuristic string matching in the template
(`x-amzn-trace-id` / `traceparent`). An operator who has
written a transformer that preserves trace context via a
different mechanism (e.g. explicit `<traceparent>` JSON
encoding) won't match the heuristic. False positives flow
through to the existing exclusion table; verdict learning
loop records.

**Schema fetch latency.** Pub/Sub schema fetch adds one
API call per topic with a schema attached. For a project
with thousands of topics and schemas, this adds scan
latency. Slice 2 caches per-scan: if a schema is referenced
by multiple topics, fetch once.

**Service Bus authorizationRules read.** Reading
authorizationRules at the namespace level exposes the rule
NAMES (not the keys). No secret material flows through
Squadron. Audit confirms.

**OCI Streaming retention threshold tuning.** The 24h
threshold is a heuristic. Operators with deliberately short
retention for cost reasons will see false-positive
recommendations. The exclusion table handles.

## 13. Slice 3 candidates

- Message content inspection (consumer-side substrate).
- Per-event-instance trace correlation.
- Cross-cloud event flows.
- EventBridge Pipes coverage.
- OCI Streaming consumer group config.
- Schema versioning analysis (Pub/Sub schema evolution).
- Service Bus per-entity (queue/topic/subscription) policy
  detection.
- Per-language SDK customization for trace context emission.

---

**Strategic frame:**

Slice 1 of event source tier shipped visibility. Slice 2
ships diagnosis. The recurring pattern across Squadron arcs
holds:

- Trace integration slice 1 (visibility) → slice 2 (action,
  recommendation kinds) → span quality slice 1 (diagnosis).
- Event source tier slice 1 (visibility) → slice 2
  (diagnosis, propagation gaps) → slice 3 (consumer-side
  correlation, deferred).

The universal claim doesn't grow a new tier or new verb —
slice 2 makes the EXISTING claim more rigorous. Operators
who run Squadron's discovery against a fleet now get an
honest answer at TWO levels:

1. Is the cloud-native trace primitive enabled at the event
   source? (slice 1)
2. Does the event source's CONFIG preserve trace context
   end-to-end? (slice 2)

The Tuesday LinkedIn drumbeat narrative gains another
concrete answer:

> "Your EventBridge rule has X-Ray on, but it's using
> InputPath '$.detail' which strips the X-Ray trace header
> before the event reaches the Lambda. Your Lambda's spans
> look orphaned because the parent context never arrived.
> Squadron just drafted the PR to remove the InputPath and
> let the full event through."

This is exactly the diagnosis operators struggle to find
themselves — buried in EventBridge rule configs, hidden in
schema definitions, never surfaced by the cloud console's
default views. Slice 2 makes Squadron the place where this
diagnosis happens.
