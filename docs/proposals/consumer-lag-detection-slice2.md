# Consumer lag detection slice 2 — Queue tier per-axis depth (post-widening)

**Status:** SHIPPED at v0.89.167 (this doc).
**Implementation chunks queued:** v0.89.168 through v0.89.171.
**Predecessor:** DLQ configuration analysis slice 1
([proposals/dlq-configuration-analysis-slice1.md](./dlq-configuration-analysis-slice1.md)),
which established the per-axis depth horizon and the two
honest-framing patterns (§3.1 managed-primitive-absence +
§3.2 scanner-coverage-gap).

## 1. Strategic context

After the cross-cloud event source widening pass closed at
3-3-3-3 / 12 surfaces (slice 10, v0.89.160), Squadron's
post-widening horizon is per-axis depth. DLQ slice 1
(v0.89.162-166) shipped the FIRST per-axis depth slice and
established the playbook:

- Pick one operational axis that matters for every queue.
- Detect on already-read scanner fields where possible.
- Establish honest-framing recommendations for substrate
  gaps (managed-primitive-absence + scanner-coverage-gap).
- Route via existing per-cloud webhook prefixes.
- Ship the slice across 4 clouds + cross-cloud closeout in
  a 5-tag arc.

Consumer lag is slice 2 — the second per-axis depth axis.

## 2. Problem statement

Asynchronous workloads have a producer-side rate and a
consumer-side rate. When the consumer-side rate falls below
the producer-side rate, messages pile up. The pile-up has
predictable failure modes:

- The retention window expires before messages are consumed,
  silently dropping work.
- Memory / disk pressure on the messaging substrate triggers
  throttling or quota-exceeded errors.
- Latency for downstream business logic balloons (a message
  produced now is consumed 30 minutes later).
- DLQ destinations (if configured per slice 1) flood with
  retry-exhausted messages, masking the underlying capacity
  problem.

Consumer lag is the leading indicator. DLQ slice 1 catches
the lagging consequence. Together they form a queue tier
operational quality vocabulary.

## 3. Detection rules

Slice 2 ships TWO detection rules, both substrate-dependent.

### 3.1 Backlog depth rule

A queue with a non-zero backlog AND no consumer activity in
the last N minutes (the substrate-specific quiet window) is
in a stalled-consumer state. The combination is the signal —
backlog alone is normal (every queue carries some); silence
alone is normal (idle queues exist). Backlog + silence is the
firing condition.

Per-cloud field mapping for the backlog axis:

| Cloud | Surface | Backlog field | Consumer-silence field |
|-------|---------|----------------|------------------------|
| AWS   | SQS queue | ApproximateNumberOfMessages (CloudWatch metric) | ApproximateAgeOfOldestMessage |
| GCP   | Cloud Tasks queue | n/a — §3.1 honest framing (Cloud Tasks API surfaces task count via list pagination only, not as a directly-queryable metric) | n/a |
| Azure | Service Bus queue | activeMessageCount on the per-queue resource | n/a — requires queue walk (§3.2 honest framing inherited from DLQ slice 1) |
| OCI   | Queue Service queue | runtimeMetadata.visibleMessages | runtimeMetadata.timeStateLastChanged |

### 3.2 Throughput inversion rule

A more sophisticated signal: producer-side enqueue rate
exceeds consumer-side dequeue rate over a rolling window.
This requires SUBSTRATE METRICS (CloudWatch / Cloud
Monitoring / Azure Monitor / OCI Monitoring), which slice 2
defers to slice 3+ chunks. Slice 2 ships only the backlog
depth rule.

### 3.3 §3.1 GCP Cloud Tasks special case (managed-primitive-absence)

Same §3.1 pattern as DLQ slice 1 chunk 2. The Cloud Tasks
admin API does NOT surface task count as a directly-queryable
metric. The list endpoint paginates over tasks with a
maximum page size of 1000; counting requires walking every
page. Squadron CANNOT determine "backlog depth" from a
single admin call.

Slice 2 chunk 2 ships `cloudtasks-backlog-monitor-add` as
the honest-framing recommendation: prompt the operator to
wire Cloud Monitoring's
`cloudtasks.googleapis.com/queue/task_count` metric to an
alerting policy. Squadron CANNOT verify the consumer is
actually keeping up; the metric subscription is the
operator's load-bearing review responsibility.

### 3.4 §3.2 Azure Service Bus inheritance

The Service Bus scanner walks
`Microsoft.ServiceBus/namespaces` (not per-queue). The
`activeMessageCount` field sits at the per-queue ARM
sub-resource layer. Slice 2 chunk 3 INHERITS the §3.2
scanner-coverage-gap honest framing from DLQ slice 1 chunk
3: ships `servicebus-backlog-queue-walk-prerequisite` with
reasoning text explicitly calling out the scanner coverage
gap.

The future Azure Service Bus per-queue walk slice closes
BOTH the DLQ slice 1 chunk 3 deferrals AND the slice 2
chunk 3 deferrals in one go — a single API extension
unblocks two per-axis detection rules.

## 4. Storage schema

NO migration. The existing `event_source_instance` table
from v0.89.100 has the right shape. Slice 2 records the
per-queue lag axis as informational Detail bag entries:

- `lag_backlog_depth` (int) — the per-cloud backlog field
  value when readable; -1 sentinel when the field is absent
  or honest-framing applies.
- `lag_backlog_depth_high` (bool) — true when
  `lag_backlog_depth` exceeds the heuristic threshold
  `BacklogDepthHighThreshold = 1000` (same per-cloud shared
  semantics as the DLQ slice 1 band).
- `lag_consumer_silence_seconds` (int) — seconds since the
  last consumer activity heartbeat (substrate-specific
  surrogate); -1 when not readable from admin scan.
- `lag_consumer_silence_high` (bool) — true when
  `lag_consumer_silence_seconds` exceeds
  `ConsumerSilenceHighThreshold = 300` (5 minutes).

Schema stays at v15.

## 5. Scanner contract

Per chunk:

- AWS SQS (chunk 1, v0.89.168): adds
  `approximateNumberOfMessages` +
  `approximateAgeOfOldestMessage` to the GetQueueAttributes
  attribute list. Backlog axis fires when count > 1000 AND
  oldest message age > 300 seconds. NO new IAM permission
  — both attributes are part of the existing
  `sqs:GetQueueAttributes` call already permitted by the
  slice 4 IAM template.
- GCP Cloud Tasks (chunk 2, v0.89.169): NO new API calls.
  Ships §3.1 honest-framing
  `cloudtasks-backlog-monitor-add` recommendation per queue.
- Azure Service Bus (chunk 3, v0.89.170): NO new API calls
  in slice 2. Ships §3.2 inherited honest-framing
  `servicebus-backlog-queue-walk-prerequisite` per
  namespace. The per-queue walk is deferred to a future
  slice that unblocks BOTH the slice 1 chunk 3 DLQ deferrals
  AND the slice 2 chunk 3 backlog deferrals.
- OCI Queue Service (chunk 4, v0.89.171): adds
  `runtimeMetadata.visibleMessages` +
  `runtimeMetadata.timeStateLastChanged` to the existing
  queue list response parsing (the OCI Queue Service list
  response already includes runtimeMetadata; slice 2 just
  reads more fields from the same payload). Backlog axis
  fires when visibleMessages > 1000 AND
  timeStateLastChanged > 300 seconds ago. NO new API call,
  NO new IAM permission.

NO new endpoint calls (the AWS chunk adds two attribute
names to an existing GetQueueAttributes parameter list which
is NOT a new endpoint call — it's a wider response from
the same call), NO new pagination, NO IAM extension.

## 6. API surface

Existing per-provider scan + inventory endpoints handle
the event_sources field generically. Slice 2 populates more
entries in the existing Detail bag.

## 7. UI

The existing per-cloud event-sources Inventory tab renders
the Detail bag's `lag_backlog_depth` + `lag_consumer_silence_seconds`
informational columns generically. Slice 2 adds a "Lag status"
badge to the per-row drilldown panel summarizing the axis.

No new UI page; no per-tab schema migration.

## 8. Recommendation kinds

7 new kinds (mirrors DLQ slice 1 kind shape):

```
sqs-backlog-monitor-add
sqs-consumer-silence-investigate

cloudtasks-backlog-monitor-add        (§3.1 honest framing)

servicebus-backlog-queue-walk-prerequisite  (§3.2 honest framing)

queues-backlog-monitor-add
queues-consumer-silence-investigate
```

Webhook routing extends THE EXISTING per-cloud prefixes —
NO new prefixes needed.

Reasoning template for `sqs-backlog-monitor-add`:

> "This SQS queue has a backlog of N messages AND the
> oldest message has been in the queue for M seconds.
> Together these signals indicate the consumer is not
> keeping up with the producer.
>
> This Terraform PR creates a CloudWatch alarm on
> `ApproximateNumberOfMessages` with threshold 1000 +
> evaluation window 5 minutes, plus an alarm on
> `ApproximateAgeOfOldestMessage` with threshold 300
> seconds. Operators receive a paged alert when both
> signals fire together.
>
> Decline if your consumer is intentionally batch-processing
> (e.g. nightly drain). The verdict learning loop records."

(Per-cloud reasoning templates for the remaining 5 kinds
follow the same pattern. Documented in detail in each
per-cloud chunk's commit message.)

## 9. Slice 2 contract

**In:**

1. Per-cloud detection helpers for the backlog-depth axis
   (4 helpers, one per cloud; 2 with §3.1/§3.2 honest framing).
2. Per-cloud detection helpers for the consumer-silence axis
   (4 helpers; same honest-framing distribution).
3. Detail bag fields `lag_backlog_depth`,
   `lag_backlog_depth_high`, `lag_consumer_silence_seconds`,
   `lag_consumer_silence_high` per per-queue projection
   function.
4. 7 new recommendation kinds routed via existing per-cloud
   prefixes.
5. Proposer prompt section concatenated into the discovery
   system prompt.
6. Cross-cloud runbook update (event source operator guide)
   + README index extension.

**Out:**

- Throughput inversion rule (§3.2 above; substrate-dependent;
  defers to slice 3+).
- Cross-substrate correlation between backlog axis and
  DLQ slice 1 axes (a separate "queue tier health summary"
  slice).

## 10. Implementation chunks

- **Chunk 1: AWS SQS lag detection + 2 recommendation
  kinds.** ~400-600 lines. **v0.89.168.**
- **Chunk 2: GCP Cloud Tasks honest-framing lag kind.**
  ~300-500 lines. **v0.89.169.**
- **Chunk 3: Azure Service Bus inherited honest-framing
  lag kind.** ~300-500 lines. **v0.89.170.**
- **Chunk 4: OCI Queue Service lag detection +
  iacpicker emitters + proposer prompt + webhook routing
  + runbook + README index (closes arc).** ~800-1200
  lines. **v0.89.171.**

Total: 5 release tags (this design doc + 4 chunks).

## 11. Acceptance tests (slice 2 contract test list)

Per-cloud acceptance tests ride in each chunk; the slice 2
design doc enumerates them so the chunk authors know what to
pin.

### AWS SQS (chunk 1)

1. **Queue with ApproximateNumberOfMessages=500 AND
   ApproximateAgeOfOldestMessage=60 → lag_backlog_depth_high
   =false, lag_consumer_silence_high=false, no firing**.
2. **Queue with ApproximateNumberOfMessages=2000 AND
   ApproximateAgeOfOldestMessage=400 → both axes fire,
   sqs-backlog-monitor-add fires AND
   sqs-consumer-silence-investigate fires**.
3. **Queue with ApproximateNumberOfMessages=2000 AND
   ApproximateAgeOfOldestMessage=60 → only backlog axis
   fires, ONLY sqs-backlog-monitor-add fires**.
4. **Queue with attribute missing → -1 absent sentinel
   (defensive)**.

### GCP Cloud Tasks (chunk 2)

5. **Queue with any shape → lag_backlog_depth=-1,
   cloudtasks-backlog-monitor-add fires (§3.1 always)**.

### Azure Service Bus (chunk 3)

6. **Namespace with any shape → lag_backlog_depth=-1,
   servicebus-backlog-queue-walk-prerequisite fires (§3.2
   always)**.

### OCI Queue Service (chunk 4)

7. **Queue with runtimeMetadata.visibleMessages=2000 AND
   timeStateLastChanged 400 seconds ago → both axes fire**.
8. **Queue with runtimeMetadata.visibleMessages=500 → no
   backlog-high firing**.

### Cross-cloud (chunk 4)

9. **All 4 clouds' lag axes surface generically through
   the existing event_source_count + per-row Detail bag
   fields**.
10. **Webhook routes all 6 kinds (2 AWS + 1 GCP + 1 Azure +
    2 OCI) via the existing per-cloud prefix switch**.
11. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.166 when no lag rows trigger
    recommendations.

## 12. Cross-references

- [DLQ configuration analysis slice 1](./dlq-configuration-analysis-slice1.md) —
  the predecessor slice that established the per-axis depth
  playbook + the two honest-framing patterns.
- [Event source tier slice 1 design doc](./event-source-tier-slice1.md) —
  the slice that introduced the EventSourceInstanceSnapshot
  Detail bag shape this slice extends.
- [Event source tier — operator guide](../event-source-tier-operator-guide.md) —
  the runbook this slice extends with the lag axis close.
