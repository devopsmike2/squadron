# Poison-message rate analysis slice 3 — Queue tier per-axis depth (post-lag)

**Status:** SHIPPED at v0.89.172 (this doc).
**Implementation chunks queued:** v0.89.173 through v0.89.176.
**Predecessors:**
[DLQ configuration analysis slice 1](./dlq-configuration-analysis-slice1.md)
+ [Consumer lag detection slice 2](./consumer-lag-detection-slice2.md).

## 1. Strategic context

Slice 1 detects DLQ presence. Slice 2 detects consumer lag.
Slice 3 sits between them: HOW OFTEN are messages actually
reaching the DLQ? A queue can have:

- DLQ configured (slice 1 axis: green).
- Consumer lag in band (slice 2 axis: green).
- BUT a continuously high poison-message rate that drains
  hours of consumer-side processing budget into the DLQ —
  the operator never sees the leading indicator unless
  Squadron surfaces the poison-message rate explicitly.

Squadron's queue tier vocabulary after slice 3:
- Slice 1: "is failure handling configured?"
- Slice 2: "is the consumer keeping up?"
- Slice 3: "is the consumer succeeding?"

Together the three axes form a complete reliability-of-
asynchronous-processing diagnostic.

## 2. Problem statement

Poison messages reach the DLQ when:
- Schema drift breaks deserialization.
- Downstream dependency outage causes processing failures
  past the retry-count band.
- New code path crashes on a specific message shape.
- Authentication / authorization regressions hit a class
  of messages.

In each case, the LEADING indicator is the poison-message
rate over time. Squadron's slice 3 axis surfaces the rate
+ flags rates that exceed the band.

## 3. Detection rules

Slice 3 ships ONE detection rule with two-band semantics:

### 3.1 Poison-message rate band rule

A queue with poison-message rate over the rolling 1-hour
window exceeding the heuristic band high threshold is in
a poison-message-surge state. The signal is substrate-
dependent — Squadron needs the per-queue DLQ depth delta
over time, which is only available via cloud-specific
metrics APIs (CloudWatch / Cloud Monitoring / Azure
Monitor / OCI Monitoring).

Per design doc constraint (mirroring slice 1 + slice 2):
NO new API calls in slice 3 chunks 1-3. The substrate
MetricQuerier integration is a future slice 4+ chunk that
explicitly extends each cloud scanner.

Slice 3 therefore ships chunks 1-4 as HONEST FRAMING
across ALL FOUR CLOUDS — the §3.1 managed-primitive-
absence pattern variant where Squadron CAN detect the
DLQ configuration (slice 1) but CANNOT compute the rate
from the scanner-pass scope alone.

### 3.2 §3.3 four-way honest framing

Squadron honestly scopes slice 3 to "we surface the
recommendation for operator-side monitoring wiring; we do
NOT yet compute the rate ourselves." This is the THIRD
variant in the honest-framing taxonomy:

- §3.1 (DLQ slice 1 chunk 2; lag slice 2 chunk 2):
  managed-primitive-absence — the cloud has no managed
  primitive for the axis.
- §3.2 (DLQ slice 1 chunk 3; lag slice 2 chunk 3):
  scanner-coverage-gap — the field exists but at an
  unwalked sub-resource.
- **§3.3 (NEW, slice 3 all chunks):** substrate-metric-
  dependence — the field is queryable from cloud
  metrics, but slice 3 does not yet integrate with the
  per-cloud MetricQuerier substrate.

§3.3 is the cleanest of the three honest-framing variants:
EVERY cloud falls under it for slice 3, so there's no
mixed shape across chunks. A future slice closes §3.3 by
extending each cloud's scanner with the MetricQuerier
calls (mirroring how the cold-start latency slice 1+2 arc
built the substrate per cloud).

## 4. Storage schema

NO migration. The existing `event_source_instance` table
has the right shape. Slice 3 records the per-queue poison
rate axis as informational Detail bag entries:

- `poison_rate_per_hour` (int) — the per-cloud poison-
  message rate over the rolling 1-hour window when
  readable; -1 sentinel when honest framing applies
  (slice 3 is always -1).
- `poison_rate_high_band` (bool) — true when
  `poison_rate_per_hour` exceeds the heuristic threshold
  `PoisonRatePerHourHighThreshold = 60` (1 per minute);
  false when honest framing applies.

Schema stays at v15.

## 5. Scanner contract

NO new API calls. NO new IAM. NO new pagination. Slice 3
detection helpers across ALL four clouds return the
honest-framing absent state. The future substrate-
MetricQuerier slice integrates per-cloud metric reads.

## 6. API surface

Existing per-provider scan + inventory endpoints handle
the event_sources field generically. Slice 3 populates two
more entries in the existing Detail bag.

## 7. UI

The existing per-cloud event-sources Inventory tab renders
the Detail bag's `poison_rate_per_hour` informational
column generically. Slice 3 adds a "Poison rate" badge to
the per-row drilldown panel.

No new UI page; no per-tab schema migration.

## 8. Recommendation kinds

4 new kinds (1 per cloud, same shape across all 4):

```
sqs-poison-rate-monitor-add        (§3.3 honest framing)
cloudtasks-poison-rate-monitor-add (§3.3 honest framing)
servicebus-poison-rate-monitor-add (§3.3 honest framing)
queues-poison-rate-monitor-add     (§3.3 honest framing)
```

Webhook routing extends THE EXISTING per-cloud prefixes —
NO new prefixes needed.

Reasoning template for `sqs-poison-rate-monitor-add`:

> "Squadron detected this SQS queue has a configured DLQ
> (slice 1 axis: green). The poison-message RATE over time
> is a leading indicator for schema drift, downstream
> dependency outages, and code regressions on a specific
> message shape — high rates exhaust consumer-side
> processing budget before reaching the DLQ.
>
> SQUADRON CANNOT YET COMPUTE THIS RATE FROM THE SCANNER
> PASS — the per-queue ApproximateNumberOfMessages on the
> DLQ over time requires a CloudWatch GetMetricStatistics
> integration that a future slice will add.
>
> This Terraform PR creates a CloudWatch alarm on the DLQ's
> ApproximateNumberOfMessages metric with threshold 60
> (1/minute) + evaluation window 5 minutes. Operators
> receive a paged alert when the poison-message rate
> spikes.
>
> Decline if your team monitors poison-message rates via a
> different surface (Datadog metric, SignalFx detector,
> existing observability stack)."

(Per-cloud reasoning templates for the remaining 3 kinds
follow the same pattern.)

## 9. Slice 3 contract

**In:**

1. Per-cloud detection helpers for the poison-rate axis
   (4 helpers; all §3.3 honest framing).
2. Detail bag fields `poison_rate_per_hour` +
   `poison_rate_high_band` per per-queue projection
   function.
3. 4 new recommendation kinds routed via existing per-cloud
   prefixes.
4. Proposer prompt section concatenated into the discovery
   system prompt.
5. Cross-cloud runbook + README index extension.

**Out:**

- Substrate-MetricQuerier integration (slice 4+).
- Per-queue DLQ depth delta computation (substrate-
  dependent).

## 10. Implementation chunks

- **Chunk 1: AWS SQS poison-rate honest framing.**
  ~250-400 lines. **v0.89.173.**
- **Chunk 2: GCP Cloud Tasks poison-rate honest framing.**
  ~250-400 lines. **v0.89.174.**
- **Chunk 3: Azure Service Bus poison-rate honest framing.**
  ~250-400 lines. **v0.89.175.**
- **Chunk 4: OCI Queue Service poison-rate honest framing
  + proposer prompt + runbook + README index (closes arc).**
  ~600-900 lines. **v0.89.176.**

Total: 5 release tags (this design doc + 4 chunks).

## 11. Acceptance tests (slice 3 contract test list)

Per-cloud acceptance tests ride in each chunk:

### AWS SQS (chunk 1)

1. **Any queue shape → poison_rate_per_hour=-1,
   poison_rate_high_band=false (§3.3 honest framing always)**.
2. **sqs-poison-rate-monitor-add fires per queue**.

### GCP Cloud Tasks (chunk 2)

3. **Any queue shape → poison_rate_per_hour=-1 (§3.3)**.

### Azure Service Bus (chunk 3)

4. **Any namespace shape → poison_rate_per_hour=-1 (§3.3)**.

### OCI Queue Service (chunk 4)

5. **Any queue shape → poison_rate_per_hour=-1 (§3.3)**.

### Cross-cloud (chunk 4)

6. **All 4 clouds' poison axes surface generically via
   the existing event_source_count + Detail bag**.
7. **Cold-start parity preserved** — proposer prompts
   byte-identical to v0.89.171 when no poison rows trigger
   recommendations.

## 12. Cross-references

- [DLQ configuration analysis slice 1](./dlq-configuration-analysis-slice1.md) —
  the first per-axis depth slice.
- [Consumer lag detection slice 2](./consumer-lag-detection-slice2.md) —
  the second per-axis depth slice.
- [Cold-start latency slice 1 design doc](./cold-start-latency-analysis-slice1.md) —
  the substrate MetricQuerier work that slice 4+ will
  reuse for closing the §3.3 honest framing.
- [Event source tier — operator guide](../event-source-tier-operator-guide.md) —
  the runbook this slice extends.
