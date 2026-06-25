# DLQ configuration analysis slice 1 — design doc

**Status:** design doc, locked for slice 1 implementation.
First **per-axis-depth** slice after the cross-cloud event
source widening pass closed at 3-3-3-3 / 12 surfaces.

**See also:**
[Event source tier slice 4](./event-source-tier-slice4.md)
(AWS SQS),
[Event source tier slice 5](./event-source-tier-slice5.md)
(GCP Cloud Tasks),
[Event source tier slice 1](./event-source-tier-slice1.md)
(Azure Service Bus + OCI Streaming foundation),
[Event source tier slice 9](./event-source-tier-slice9.md)
(OCI Queue Service).

## 1. Problem

After the slice 10 strategic close, the event source tier
covers every cloud at 3 surfaces (queue, pub/sub fan-out,
partitioned-log). The widening is structural; the next
horizon is **per-axis depth** — detecting whether the
operator has configured each surface's per-resource
operational knobs correctly.

The first per-axis-depth slice targets **DLQ
configuration** because:

1. **It's a 4-cloud generalization that maps cleanly.** All
   four clouds' queue primitives (AWS SQS, GCP Cloud Tasks,
   Azure Service Bus, OCI Queue Service) carry first-class
   DLQ semantics with different field names but identical
   operator intent: "how many retries before a poison
   message goes to the side channel?"
2. **The operational failure mode is real and recurring.**
   Operators routinely ship queues to production WITHOUT a
   DLQ configured. Poison messages get redelivered
   indefinitely (or until the message expires after the
   retention window — whichever comes first), wasting
   consumer-side processing budget AND silently dropping
   work that should have been routed for human review.
3. **Squadron's existing scanners ALREADY READ the relevant
   fields.** AWS SQS redrivePolicy, GCP Cloud Tasks
   retryConfig, Azure Service Bus
   forwardDeadLetteredMessagesTo + maxDeliveryCount, OCI
   Queue deadLetterQueueDeliveryCount are all already in
   the per-queue snapshot Detail bag (see slices 4 / 5 /
   1 / 9 respectively). Slice 1 of the DLQ axis is largely
   a detection-rule + recommendation-kind addition, NOT a
   new API call surface.
4. **The recommendation Terraform is concrete.** Per-cloud
   DLQ creation patterns are well-known and decline-paths
   are clear (operators using out-of-band poison-message
   handling — manual replay tooling, custom Lambda /
   Function consumers with side-channel write-out — have
   honest decline cases).

The slice ships per-cloud chunks so the per-cloud detail
work stays narrow. Slice 1 covers the design doc; chunks
1-4 ship the four clouds in dependency order.

### Why DLQ FIRST among per-axis-depth candidates?

The slice 11+ candidate list from slice 10 §13 included:
- Per-subscription consumer-side lag detection
- Cross-region disaster-recovery analysis
- Schema enforcement
- Pub/Sub-to-Lite migration recommendations (requires cost
  modeling)
- Per-message trace context propagation analysis

DLQ wins as the FIRST per-axis-depth slice because:

1. **No substrate dependency.** DLQ detection rides on
   existing scanner output. Consumer-side lag detection
   requires substrate MetricQuerier wiring per-cloud
   (slice 12+). Cost modeling for migration recommendations
   requires a cost substrate Squadron does not yet have.
2. **High operator value.** DLQ misconfigurations are
   among the most common SEV1 root causes Squadron is
   designed to catch — see the existing
   slice-9-design-doc Tuesday LinkedIn drumbeat anchor
   ("when a message lands in the DLQ at 2am the operator
   has no record of which consumer attempted it") which
   already names DLQ as the operational pivot point.
3. **4-cloud generalization is symmetric.** Every cloud's
   queue primitive has a DLQ knob with well-named fields;
   the structural per-cloud detection rule is uniform
   across the matrix.

### What slice 1 does NOT address

- **Per-message DLQ destination analysis** — checking
  whether the DLQ queue itself is logged + monitored is
  slice 2+ candidate.
- **DLQ depth / age alerts** — "the DLQ has N messages
  older than X hours" requires substrate MetricQuerier
  integration; slice 12+.
- **Cross-queue DLQ topology** — when multiple primary
  queues share a single DLQ, detecting fan-in correlation
  is slice 12+.
- **Per-subscription DLQ for Pub/Sub topics** — Azure
  Service Bus subscriptions have per-subscription DLQ
  config; AWS SNS subscriptions also; slice 2+ extends to
  the pub/sub family.
- **Auto-fix.** Squadron remains a recommender.

## 2. Non-goals (slice 1)

- **Per-message DLQ destination analysis** — slice 2+.
- **DLQ depth / age substrate analysis** — slice 12+.
- **Cross-queue DLQ topology** — slice 12+.
- **Pub/Sub-tier DLQ (SNS subscriptions, Service Bus topic
  subscriptions, Pub/Sub subscriptions)** — slice 2+.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection rules — 4-cloud queue tier

The slice 1 detection rule fires when EITHER condition is
true:

- **Missing DLQ:** the queue has no DLQ configured at all.
- **Inappropriate retry count:** the queue has a DLQ
  configured BUT the per-cloud retry count is OUTSIDE the
  band `[2, 50]`. Counts below 2 send transient failures
  straight to the DLQ (too aggressive); counts above 50
  defer DLQ routing past the typical
  consumer-restart-and-retry horizon (too lenient).

The band `[2, 50]` is heuristic and the slice 1 design doc
makes the threshold explicit so future tuning is auditable.
Operators with deliberately tight (≤1) or deliberately
loose (>50) retry policies have honest decline cases — both
recommendation kinds carry decline-path framing.

### Per-cloud field mapping

| Cloud | Resource | DLQ-presence field | Retry-count field |
|-------|----------|---------------------|--------------------|
| AWS   | SQS queue | `Attributes.RedrivePolicy` (non-empty + valid JSON resolving to a sibling queue ARN) | `Attributes.RedrivePolicy.maxReceiveCount` |
| GCP   | Cloud Tasks queue | n/a (Cloud Tasks does NOT have a DLQ primitive — operator pattern is consumer-side dead-letter routing) — see §3.1 special case | `retryConfig.maxAttempts` |
| Azure | Service Bus queue | `forwardDeadLetteredMessagesTo` non-empty OR enableDeadLetteringOnMessageExpiration true (the latter routes to the namespace's default DLQ which Service Bus auto-creates) | `maxDeliveryCount` |
| OCI   | Queue Service queue | `deadLetterQueueDeliveryCount > 0` (the value itself is the count; presence of the field flips presence) | (same field — the field both gates DLQ presence AND counts retries) |

### §3.1 Cloud Tasks special case

GCP Cloud Tasks does NOT have a managed DLQ primitive. The
canonical operator pattern is consumer-side
dead-letter routing: the HTTP target / App Engine handler
catches the final retry's failure and writes the task
payload to a separate "dead-letter" queue or storage
bucket. Squadron CANNOT detect this from the Cloud Tasks
admin surface alone.

Slice 1 ships GCP Cloud Tasks DLQ detection as an
**informational only** axis: the snapshot Detail bag
records `has_dlq_pattern_likely=false` (always, for slice
1) and the recommendation kind `cloudtasks-dlq-pattern-add`
is honest about the operator-side-detection limitation. The
recommendation Terraform creates a sibling Cloud Tasks
queue named `${original}-dlq` and emits a reasoning text
calling out that Squadron CANNOT verify the operator's
HTTP target actually routes to the DLQ on final-retry
failure — the operator review is load-bearing.

For the retry-count axis on Cloud Tasks, the standard
detection rule applies (`maxAttempts` outside `[2, 50]`
fires `cloudtasks-retry-count-bound`).

## 4. Storage schema

NO migration. The existing `event_source_instance` table
from v0.89.100 has the right shape. Slice 1 records the
per-queue DLQ axis as informational Detail bag entries:

- `has_dlq` (bool) — true for AWS / Azure / OCI when the
  per-cloud presence rule fires; always false for Cloud
  Tasks per §3.1.
- `dlq_retry_count` (int) — the per-cloud retry-count
  field value when readable; -1 sentinel when the field is
  absent or the queue has no DLQ.
- `dlq_retry_count_in_band` (bool) — true when
  `dlq_retry_count` is in `[2, 50]`.

Schema stays at v15.

## 5. Scanner contract

No new API calls. The existing per-cloud queue scanners
ALREADY read the relevant fields:

- AWS SQS: slice 4 chunk 1 (`Attributes.RedrivePolicy`).
- GCP Cloud Tasks: slice 5 chunk 1 (`retryConfig.maxAttempts`).
- Azure Service Bus: slice 1 chunk 3 + slice 2 chunk 3
  (`forwardDeadLetteredMessagesTo`, `maxDeliveryCount`,
  `enableDeadLetteringOnMessageExpiration`).
- OCI Queue Service: slice 9 chunk 1
  (`deadLetterQueueDeliveryCount`).

Slice 1 chunks add detection-rule helpers + Detail bag
fields to each per-cloud projection function. NO new
endpoint calls, NO new pagination, NO IAM extension.

## 6. API surface

Existing per-provider scan + inventory endpoints handle
the event_sources field generically. Slice 1 populates
more entries in the existing Detail bag.

## 7. UI

The existing per-cloud event-sources Inventory tab renders
the Detail bag's `has_dlq` + `dlq_retry_count` informational
columns generically. Slice 1 adds a "DLQ status" badge to
the per-row drilldown panel summarizing the axis.

No new UI page; no per-tab schema migration.

## 8. Recommendation kinds

8 new kinds (2 per cloud):

```
sqs-dlq-attach
sqs-dlq-retry-count-bound

cloudtasks-dlq-pattern-add
cloudtasks-retry-count-bound

servicebus-dlq-attach
servicebus-dlq-retry-count-bound

queues-dlq-attach
queues-dlq-retry-count-bound
```

Webhook routing extends THE EXISTING per-cloud prefixes —
NO new prefixes needed. `sqs-*` → AWS, `cloudtasks-*` →
GCP, `servicebus-*` → Azure, `queues-*` → OCI. The dlq-
suffix patterns extend cleanly under the existing prefix
families.

Reasoning template for `sqs-dlq-attach`:

> "This SQS queue has no DLQ (redrive policy) configured.
> Poison messages will redeliver indefinitely until the
> retention window expires, wasting consumer-side
> processing budget AND silently dropping work that should
> have been routed for human review.
>
> This Terraform PR creates a sibling SQS queue named
> `${original}-dlq` AND attaches it via the original queue's
> `redrive_policy` block with a conservative
> `maxReceiveCount = 5`. Operators with deliberately
> alternative poison-message handling (manual replay
> tooling, consumer-side side-channel write-out) should
> decline. The verdict learning loop records."

Reasoning template for `cloudtasks-dlq-pattern-add`:

> "This Cloud Tasks queue has no sibling DLQ pattern that
> Squadron can detect from the Tasks admin surface. The
> canonical Cloud Tasks DLQ pattern is consumer-side
> dead-letter routing — the HTTP target catches the final
> retry's failure and writes to a separate dead-letter
> destination.
>
> SQUADRON CANNOT VERIFY YOUR HTTP TARGET ROUTES TO A DLQ
> on final-retry failure. This Terraform PR creates a
> sibling Cloud Tasks queue named `${original}-dlq` as the
> destination; operators MUST update their HTTP target's
> failure handler to enqueue to the DLQ on the final
> retry. The PR review is load-bearing for the consumer-
> side wiring.
>
> Decline if your team uses a non-Cloud-Tasks
> dead-letter destination (Pub/Sub topic, Cloud Storage
> bucket, BigQuery streaming insert). The verdict learning
> loop records."

(Per-cloud reasoning templates for the remaining 6 kinds
follow the same pattern — concrete Terraform + clear
decline path. Documented in detail in each per-cloud
chunk's commit message.)

## 9. Slice 1 contract

**In:**

1. Per-cloud detection helpers for the missing-DLQ axis
   (4 helpers, one per cloud).
2. Per-cloud detection helpers for the retry-count band
   axis (4 helpers, one per cloud).
3. Detail bag fields `has_dlq` + `dlq_retry_count` +
   `dlq_retry_count_in_band` added to each cloud's
   per-queue projection function.
4. 8 new recommendation kinds (2 per cloud).
5. Webhook routing extends UNDER EXISTING per-cloud
   prefixes — no new prefixes.
6. iacpicker emitters for all 8 Terraform patterns.
7. Operator runbook section.
8. README index entry updated.
9. Acceptance tests covering the detection rules
   per-cloud (both axes) + cold-start parity (the
   user-message renderer stays byte-identical to
   v0.89.161 when no DLQ rows trigger recommendations).

**Out:**

- Per-message DLQ destination logging analysis
  (slice 2+).
- DLQ depth / age substrate analysis (slice 12+).
- Cross-queue DLQ topology / fan-in correlation
  (slice 12+).
- Pub/Sub-tier DLQ extension (slice 2+).
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: AWS SQS DLQ detection + 2 recommendation
  kinds.** ~400-600 lines. **v0.89.163.**
- **Chunk 2: GCP Cloud Tasks DLQ pattern + retry-count
  detection.** ~400-600 lines. **v0.89.164.**
- **Chunk 3: Azure Service Bus DLQ detection.**
  ~400-600 lines. **v0.89.165.**
- **Chunk 4: OCI Queue Service DLQ detection +
  iacpicker emitters + proposer prompt + webhook routing
  + runbook + README index (closes arc).** ~800-1200
  lines. **v0.89.166.**

Total: 5 release tags (this design doc + 4 chunks).
Per-cloud chunks are independent — chunk 4 closes the arc
with the cross-cloud proposer prompt + runbook + README
work that depends on all four detection helpers landing.

## 11. Acceptance tests (slice 1 contract test list)

Per-cloud acceptance tests ride in each chunk; the
slice 1 design doc enumerates them so the chunk authors
know what to pin.

### AWS SQS (chunk 1)

1. **Queue with no `Attributes.RedrivePolicy` →
   has_dlq=false, sqs-dlq-attach fires**.
2. **Queue with `RedrivePolicy.maxReceiveCount=5` →
   has_dlq=true, retry_count=5, dlq_retry_count_in_band
   =true**.
3. **Queue with `RedrivePolicy.maxReceiveCount=1` →
   has_dlq=true, retry_count=1, retry_count_in_band=false,
   sqs-dlq-retry-count-bound fires**.
4. **Queue with `RedrivePolicy.maxReceiveCount=1000` →
   has_dlq=true, retry_count=1000, retry_count_in_band=false,
   sqs-dlq-retry-count-bound fires**.
5. **Queue with malformed RedrivePolicy JSON →
   has_dlq=false (defensive: malformed is NOT presence)**.

### GCP Cloud Tasks (chunk 2)

6. **Queue with no retryConfig → has_dlq_pattern_likely=
   false, cloudtasks-dlq-pattern-add fires AND
   cloudtasks-retry-count-bound fires (maxAttempts=0 is
   outside band)**.
7. **Queue with `retryConfig.maxAttempts=5` →
   has_dlq_pattern_likely=false (always per §3.1),
   cloudtasks-retry-count-bound does NOT fire (5 is in
   band)**.
8. **Queue with `retryConfig.maxAttempts=-1` (unlimited)
   → retry_count_in_band=false,
   cloudtasks-retry-count-bound fires**.

### Azure Service Bus (chunk 3)

9. **Queue with no `forwardDeadLetteredMessagesTo` AND
   `enableDeadLetteringOnMessageExpiration=false` →
   has_dlq=false, servicebus-dlq-attach fires**.
10. **Queue with `enableDeadLetteringOnMessageExpiration
    =true` → has_dlq=true (the namespace's auto-DLQ is
    the destination)**.
11. **Queue with `forwardDeadLetteredMessagesTo` non-empty
    → has_dlq=true (operator pointed at a custom
    destination)**.
12. **Queue with `maxDeliveryCount=10` → retry_count=10,
    retry_count_in_band=true**.
13. **Queue with `maxDeliveryCount=1` →
    retry_count_in_band=false,
    servicebus-dlq-retry-count-bound fires**.

### OCI Queue Service (chunk 4)

14. **Queue with `deadLetterQueueDeliveryCount=0` →
    has_dlq=false, queues-dlq-attach fires**.
15. **Queue with `deadLetterQueueDeliveryCount=5` →
    has_dlq=true, retry_count=5, retry_count_in_band=true**.
16. **Queue with `deadLetterQueueDeliveryCount=100` →
    has_dlq=true, retry_count_in_band=false,
    queues-dlq-retry-count-bound fires**.

### Cross-cloud (chunk 4)

17. **All 4 clouds' DLQ axes surface generically through
    the existing event_source_count + per-row Detail bag
    fields**.
18. **Webhook routes all 8 kinds to the correct cloud
    via the existing per-cloud prefix switch (no new
    prefix routing logic)**.
19. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.161 when no DLQ rows trigger
    recommendations.

## 12. Threat model

**No new IAM permissions.** Slice 1 reads ONLY fields the
existing per-cloud queue scanners already read.

**Detection-rule discipline.** The `[2, 50]` band is
heuristic. Tuning is documented in §3 so a future slice
that changes the band has an auditable starting point.
Operators with legitimately tight or loose policies have
honest decline cases.

**Cloud Tasks honest framing.** The cloudtasks-dlq-pattern
recommendation explicitly calls out that Squadron CANNOT
verify the consumer-side wiring. The reasoning text shifts
the verification burden to PR review — Squadron drafts
the Terraform skeleton; the operator confirms their HTTP
target actually enqueues to the DLQ on final-retry
failure.

**No span content logging.** Slice 1 reads queue metadata
only. Message payloads stay invisible to Squadron. PII
surface stays at zero.

## 13. Slice 2+ candidates (DLQ axis)

- **Pub/Sub-tier DLQ extension** — Azure Service Bus
  subscription per-subscription DLQ, AWS SNS subscription
  DLQ, GCP Pub/Sub dead-letter topic.
- **DLQ destination logging axis** — does the DLQ queue
  itself have Logging configured? (Reuses the per-cloud
  Logging detection helpers consolidated in Stream 200.)
- **Cross-queue DLQ topology** — when multiple primary
  queues share one DLQ, detecting fan-in correlation
  + surfacing the topology view on the Inventory tab.
- **DLQ-destination existence check** — for Service Bus
  `forwardDeadLetteredMessagesTo`, verify the target
  queue/topic actually exists in the namespace (currently
  a static-string check).

## 14. Slice 12+ candidates (substrate-driven)

These need substrate MetricQuerier wiring:

- **DLQ depth / age alerts** — "the DLQ has N messages
  older than X hours". Requires per-cloud CloudWatch /
  Cloud Monitoring / Azure Monitor / OCI Monitoring
  metric reads.
- **Consumer-side lag** vs. DLQ-arrival correlation —
  flagging when a DLQ arrival burst correlates with a
  consumer-side latency spike (the slice 11+ candidate
  list called this out).
- **Cost-impact estimation** for DLQ presence — the cost
  of DLQ-bound retries vs. inline retries.

---

**Strategic frame:**

Slice 1 of the DLQ-configuration arc is the **first
per-axis-depth slice** Squadron ships after the
cross-cloud widening pass closed at 3-3-3-3.

> "Squadron now sees TWELVE event source surfaces across
> four clouds. Slice 11 starts the depth pass. The first
> axis: DLQ configuration. The first recommendation kinds
> in the depth pass: do your queues actually route poison
> messages somewhere a human can review them?"

This is also the first Squadron arc that ships
recommendations CALLING OUT that Squadron's detection has
limits the operator's review must close (Cloud Tasks
§3.1). The honest framing IS the load-bearing pattern —
slice 12+ depth work will repeatedly hit
"substrate-dependent detection where Squadron cannot prove
the operator's invariant from the admin API alone"; slice
11 establishes the honest-framing precedent in the
proposer prompt.

The Tuesday LinkedIn drumbeat narrative gains: "Squadron's
widening pass closed at twelve event source surfaces. The
depth pass starts with the question every SRE knows the
answer to at 3am: when a poison message goes to the DLQ,
does anyone hear it? Twelve surfaces. Eight recommendation
kinds. Honest framing for the one cloud where the
detection sits in the operator's HTTP handler."
