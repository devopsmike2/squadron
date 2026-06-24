# Event source tier slice 4 — AWS SQS (third AWS surface)

**Status:** design doc, locked for slice 4 implementation.
Continues the widening pass started in slice 3 by adding AWS
SQS as the third AWS event source surface alongside
EventBridge + SNS.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 2](./event-source-tier-slice2.md),
[Event source tier slice 3](./event-source-tier-slice3.md).

## 1. Problem

Event source tier slice 3 (v0.89.137-139) added AWS SNS as
the second AWS event source surface. The widening pass is
in motion but incomplete:

- AWS has THREE meaningful event source primitives: EventBridge
  + SNS + SQS. Slice 1 shipped EventBridge; slice 3 shipped
  SNS. SQS is still invisible to Squadron.
- The canonical AWS pub/sub fan-out architecture is
  **SNS-to-SQS**: an SNS topic publishes messages that
  multiple SQS queues subscribe to. Each SQS queue then
  drives a separate downstream consumer (Lambda, EC2 worker,
  etc.).
- Without SQS coverage, Squadron sees the front of the fan-out
  (SNS topic) but misses the legs (queues that hold messages
  until consumers pull). The "where did my message go?"
  diagnostic chain breaks at the SNS→SQS boundary.

Slice 4 closes this AWS gap by adding SQS as the third AWS
surface.

### Why SQS now?

1. **Composes with SNS architecturally.** Operators use SNS +
   SQS together for pub/sub fan-out. Squadron seeing both
   surfaces completes the AWS event distribution layer:
   `EventBridge | SNS → SQS → consumer`.
2. **The trace continuity gap.** An SNS topic without
   delivery logging → message hits an SQS queue → SQS queue
   without redrive policy + dead-letter queue → on consumer
   failure, message vanishes silently. SQS detection catches
   the second leg of this failure mode.
3. **The scanner pattern is established.** Slice 3 set the
   AWS event source dispatcher partial-scan posture. Slice 4
   extends to fan out across THREE surfaces with the same
   posture.
4. **Operator urgency.** SQS dead-letter queue gaps are the
   canonical "we silently lost messages" production failure.
   Squadron flagging the absence of redrive policies is real
   operational value.

### What slice 4 does NOT address

- **Non-AWS event source widening** — slices 5-7 will add
  GCP Cloud Tasks, Azure Event Grid + Event Hubs, OCI
  Notification Service.
- **SNS-to-SQS subscription fan-out trace correlation** —
  per-subscription propagation analysis remains slice 8+
  candidate.
- **SQS consumer-side analysis** — Lambda event source
  mappings already covered in serverless tier; deeper
  consumer-side correlation is slice 8+.

## 2. Non-goals (slice 4)

- **AWS SES, EventBridge Scheduler, AppFlow** — other AWS
  event-adjacent surfaces. Not in slice 4; deferred to
  later slices when prioritized.
- **GCP Cloud Tasks / Azure Event Grid + Event Hubs / OCI
  Notification Service** — slices 5-7.
- **SNS-to-SQS subscription configuration inspection** —
  whether the SNS subscription is configured to wrap message
  attributes. Slice 8+.
- **SQS message body inspection.** Squadron reads queue
  metadata only.
- **Per-queue depth alerting.** Queue depth is informational
  only in slice 4. Slice 5+ may add anomaly detection on
  message-in / message-out rates.
- **Per-queue access policy inspection.** Squadron does NOT
  parse the queue's IAM policy for fine-grained permissions
  in slice 4.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — AWS SQS

API: `sqs:ListQueues`, `sqs:GetQueueAttributes`. Required IAM
extension to the existing AWS scanner policy.

Detection axes:

| Axis                        | Source                                                                | Recommendation kind             |
|-----------------------------|-----------------------------------------------------------------------|----------------------------------|
| Has redrive policy          | `RedrivePolicy` attribute is set AND parses with non-empty `deadLetterTargetArn` | `sqs-redrive-policy-enable`      |
| Dead-letter queue reachable | The DLQ ARN in RedrivePolicy can be resolved (queue with that ARN exists in the same account/region) | `sqs-deadletter-queue-attach`    |
| Visibility timeout sanity   | `VisibilityTimeout` is set (default is 30s, queue creation explicitly defaults; informational unless 0) | informational only               |
| Encryption at rest          | `KmsMasterKeyId` is set                                               | informational only               |
| FIFO content dedup          | FIFO queues: `ContentBasedDeduplication = true`                       | informational only               |

The trace + log axis mapping for SQS mirrors the slice 1
log-target proxy pattern: SQS doesn't have a direct OTel
integration, so Squadron uses the canonical operational signal
(redrive policy → DLQ) as the trace primitive proxy. A queue
with a redrive policy + reachable DLQ means failed messages
get captured for post-mortem; without the redrive policy,
failures vanish silently.

The SQS arc is operationally narrow but operationally critical:
the `sqs-redrive-policy-enable` recommendation catches the
single most common AWS messaging production failure (silent
message loss).

## 4. Storage schema

NO migration. The existing `event_source_instance` table from
v0.89.100 has the right shape. Slice 4 adds rows with
`provider = "aws"` and `surface = "sqs"`.

Schema stays at v15.

## 5. Scanner contract

The slice 3 AWS scanner extended `ScanEventSources` to fan
out across EventBridge + SNS with partial-scan posture. Slice
4 extends to three surfaces:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot
    
    buses, ebErr := s.ScanEventBridgeBuses(ctx, scope)
    if ebErr == nil { all = append(all, buses...) }
    
    topics, snsErr := s.ScanSNSTopics(ctx, scope)
    if snsErr == nil { all = append(all, topics...) }
    
    queues, sqsErr := s.ScanSQSQueues(ctx, scope)
    if sqsErr == nil { all = append(all, queues...) }
    
    // Partial-scan posture: only error when ALL three failed
    if ebErr != nil && snsErr != nil && sqsErr != nil {
        return all, fmt.Errorf("all surfaces failed: eb=%w sns=%v sqs=%v", ebErr, snsErr, sqsErr)
    }
    
    return all, nil
}
```

New file `internal/discovery/aws/sqs.go` implements
`ScanSQSQueues`.

The SQS API:
- `sqs:ListQueues` returns paginated queue URLs (NOT ARNs)
- `sqs:GetQueueAttributes` per queue URL with `AttributeNames =
  ["All"]` returns the attribute map including
  RedrivePolicy, VisibilityTimeout, KmsMasterKeyId,
  QueueArn, FifoQueue, ContentBasedDeduplication, etc.
- The AWS SDK Go v2 package `github.com/aws/aws-sdk-go-v2/service/sqs`
  exposes both. Already in go.mod from previous discovery
  work — no new dependency.

### DLQ resolution

The `dead-letter queue reachable` detection axis requires
resolving the DLQ ARN from a queue's RedrivePolicy. Slice 4
takes a pragmatic approach:

1. Collect all queue ARNs in the scan (from the ARN attribute
   of each GetQueueAttributes response)
2. After scanning, walk the queues a second time and check
   whether each queue's RedrivePolicy.deadLetterTargetArn
   matches an ARN in the collected set
3. If matched, set HasLogAxis (the trace primitive proxy) to
   true; if not, the DLQ is in a different account/region OR
   doesn't exist (operator should investigate)

The two-pass walk is O(2N) for N queues. Acceptable for
typical fleets.

## 6. API surface

Existing per-provider scan + inventory endpoints already
handle the event_sources field generically. Slice 4 just
populates more rows.

Discovery summary endpoint's `event_source_count` for AWS
starts increasing by the queue count.

## 7. UI

The DiscoveryAWS Event sources sub-tab from slice 1 renders
rows generically by Surface field. Slice 4 SQS rows render
under Surface = "sqs". No UI changes.

## 8. Recommendation kinds

2 new kinds:

```
sqs-redrive-policy-enable
sqs-deadletter-queue-attach
```

Both reuse the existing AWS event source webhook prefix
convention. Specifically, the `sqs-` prefix is NEW — webhook
routing extends:

```
sqs-       → aws
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template for `sqs-redrive-policy-enable`:

> "This SQS queue has NO RedrivePolicy configured. When a
> message fails processing (consumer throws / times out),
> the message gets put back on the queue and retried
> indefinitely. Without a redrive policy + dead-letter
> queue, eventually the message expires from the queue's
> retention window and vanishes silently — the
> single most common AWS messaging production failure.
>
> This Terraform PR configures a dead-letter queue + redrive
> policy targeting it. Operators using a custom retry
> coordinator (Step Functions retry handler, etc.) should
> decline; the verdict learning loop records."

Reasoning template for `sqs-deadletter-queue-attach`:

> "This SQS queue has a RedrivePolicy set with
> deadLetterTargetArn pointing at a queue ARN that Squadron
> could NOT resolve in the same account+region. The DLQ may
> be in a different account/region (cross-account DLQ —
> verify the source queue's IAM policy permits send) OR the
> DLQ doesn't exist (the policy is dangling).
>
> Squadron does NOT auto-draft a fix because the operator
> needs to confirm the intent (cross-account vs deleted DLQ).
> This recommendation surfaces the queue for review.
> Decline if the cross-account setup is intentional."

Terraform pattern for `sqs-redrive-policy-enable`:

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

There's NO Terraform pattern for `sqs-deadletter-queue-attach`
— it's an audit-only recommendation. The runbook documents
this.

## 9. Slice 4 contract

**In:**

1. AWS `ScanSQSQueues` implementation populating
   `event_source_instance` with surface=sqs.
2. AWS `ScanEventSources` dispatcher extension to fan out
   across EventBridge + SNS + SQS with three-way partial-scan
   posture.
3. IAM template extension: `sqs:ListQueues`,
   `sqs:GetQueueAttributes`.
4. 2 new recommendation kinds:
   `sqs-redrive-policy-enable` + `sqs-deadletter-queue-attach`.
5. Webhook routing extends with `sqs-` → aws.
6. iacpicker emitter for `sqs-redrive-policy-enable`
   Terraform pattern.
7. Operator runbook section (extend
   docs/event-source-tier-operator-guide.md).
8. README index entry updated to mention SQS coverage.
9. Acceptance tests covering SQS detection, both axes
   (redrive policy + DLQ reachability), three-way dispatcher
   partial-scan posture, cold-start parity.

**Out:**

- GCP Cloud Tasks / Azure Event Grid + Event Hubs / OCI
  Notification Service (slices 5-7).
- SNS-to-SQS subscription fan-out trace correlation.
- SQS message body inspection.
- Per-queue depth alerting.
- Per-queue access policy inspection.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: AWS SQS scanner + three-way dispatcher
  extension.** ~600-800 lines. **v0.89.141.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~600-800 lines.
  **v0.89.142.**

Total: 2 release tags. Same pattern as slice 3 — small
arcs that reuse the slice 1 + slice 3 scaffolding.

## 11. Acceptance tests

1. **AWS ScanSQSQueues returns queues** — paginated list
   response is walked.
2. **Queue with RedrivePolicy + reachable DLQ → both axes
   true**.
3. **Queue with NO RedrivePolicy → both axes false**.
4. **Queue with RedrivePolicy but unresolvable DLQ ARN →
   has_trace_axis = true (policy exists) BUT
   has_log_axis = false (DLQ unreachable)** — the audit
   path.
5. **Queue with KmsMasterKeyId → snapshot Detail records
   the encryption flag**.
6. **FIFO queue with ContentBasedDeduplication → snapshot
   Detail records the dedup flag**.
7. **Three-way ScanEventSources dispatcher returns buses +
   topics + queues**.
8. **Three-way dispatcher partial-scan: EventBridge fails →
   SNS + SQS still surface**.
9. **Three-way dispatcher partial-scan: SNS fails → EB + SQS
   still surface**.
10. **Three-way dispatcher partial-scan: SQS fails → EB +
    SNS still surface**.
11. **Three-way dispatcher: all three fail → error mentions
    all three surfaces**.
12. **Webhook routes sqs-redrive-policy-enable to aws**.
13. **Webhook routes sqs-deadletter-queue-attach to aws**.
14. **Discovery summary AWS event_source_count surfaces
    non-zero when queues exist**.
15. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.139 when no SQS rows trigger
    recommendations.

## 12. Threat model

**Wider AWS IAM permissions.** Slice 4 adds `sqs:ListQueues`
and `sqs:GetQueueAttributes` to the AWS scanner policy. Both
read-only. Operators get the in-product IAM upgrade path
(#590).

**SQS API rate limits.** SQS has a per-region per-account
ListQueues + GetQueueAttributes rate limit (~30 TPS each).
For a fleet of 1000 queues in one region:
- 1000 GetQueueAttributes queries
- Substrate's existing AWS rate limiter absorbs
- ~33 seconds added to scan duration

The two-pass DLQ resolution walk adds another N=1000 string
matches in-memory — negligible.

**Cost surface.** SQS API queries are free. No new
operator-facing cost decisions per the no-money brief.

**Three-way dispatcher partial-scan posture.** When one or
two of the three surfaces fail (IAM lag from connections
that predate v0.89.100 / v0.89.138 / v0.89.141), the
remaining surfaces still scan. Pinned by tests 8-11.

**False positives on intentional no-DLQ queues.** A queue
used for ephemeral notifications (where message loss is
acceptable) doesn't need a redrive policy. The exclusion
table + verdict learning loop handle. Runbook documents.

**Cross-account DLQ false positive.** A queue with a DLQ in
a different account+region (Squadron can't resolve it
within the same scan) triggers `sqs-deadletter-queue-attach`
recommendation. The audit-only nature of this kind means
the operator reviews; no false PR is drafted.

**No span content logging.** Slice 4 reads queue metadata
only. Message bodies stay invisible to Squadron. PII surface
stays at zero.

## 13. Slice 5+ candidates

- **GCP Cloud Tasks** — second GCP surface. Slice 5.
- **Azure Event Grid** — second Azure surface. Slice 6.
- **Azure Event Hubs** — third Azure surface. Slice 6 or 7.
- **OCI Notification Service** — second OCI surface. Slice 7.
- **SNS-to-SQS subscription fan-out trace correlation** —
  per-subscription propagation detection.
- **SQS consumer-side analysis** — Lambda event source
  mappings already covered in serverless; cross-tier
  correlation is slice 8+.
- **Per-queue depth anomaly detection** — slice 5+ may add
  message-in / message-out rate baselining using the
  substrate's MetricQuerier (the third diagnostic
  dimension), turning SQS depth into a metric-correlated
  signal alongside cold-start / sampling / error rate.

---

**Strategic frame:**

Slice 4 continues the widening pass on the event source
tier. After slice 4:

> "Squadron covers EIGHT event source surfaces across four
> clouds — AWS EventBridge + SNS + SQS, GCP Pub/Sub, Azure
> Service Bus, OCI Streaming."

Wait — that's only six. The widening pass added one
surface to AWS in slice 3 and another in slice 4; the other
three clouds still have one surface each. The honest framing
remains: AWS now has the most complete event source
coverage; the other clouds catch up in slices 5-7.

The Tuesday LinkedIn drumbeat narrative gains the most
operationally-resonant detection yet: "Your SQS queue
`order-processing` has no DLQ configured. When a consumer
throws, the message gets retried until it expires from the
14-day retention window and silently disappears. This is
the single most common AWS messaging production failure.
Squadron just drafted the PR to add a dead-letter queue +
redrive policy."

After slice 4, the canonical pub/sub failure chain is fully
visible in Squadron:

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
recommendation kind + IaC PR. The widening pass is what
makes this chain complete.
