# Event source tier slice 3 — AWS SNS (second AWS surface)

**Status:** design doc, locked for slice 3 implementation.
Widens the event source tier claim from 1 surface per cloud
to 2 by adding AWS SNS as a second AWS event source surface
alongside EventBridge (slice 1).

**Update (picker-activation arc, v0.89.348):** the
`sns-delivery-logging-enable` recommendation is now emitted
end-to-end. Slice 3 built the scanner (aws/sns.go, HasLogAxis =
any per-protocol delivery-feedback role set) and the iacpicker
emitter (`PickSNSDeliveryLoggingPattern`) but never wired a
detection branch to call the picker — the kind was dormant in
production. The picker-activation arc adds
`proposer.CheckSNSDeliveryLogging` (fires when a scanned SNS topic
has `HasLogAxis == false`, honoring exclusions/verdict-learning)
and the handler append
`DiscoveryHandlers.appendAWSEventSourceRecs`, so a real discovery
scan now surfaces the recommendation and opens a Terraform PR. This
is the reference slice for wiring the remaining dormant
event-source pickers.

**Arc closed (v0.89.348–v0.89.352):** all event-source pickers are
now wired to production detection branches + per-cloud handler
appends. Coverage: AWS SNS delivery-logging + SQS redrive; GCP Cloud
Tasks retry/logging + Pub/Sub Lite logging/reservation; Azure Event
Grid diagnostics/CloudEvents-schema + Event Hubs
diagnostics/Capture; OCI Notification Service logging. Every signal
was confirmed (via a per-cloud parallel survey) to already reach the
handler from the existing scanners — no scanner enrichment was
required. The one remaining dormant picker,
`PickResourceManagerLoggingPattern`, is orchestration-tier (not
event-source) and is tracked separately. The detection branches live
in `internal/proposer/event_source.go` (registry:
`proposer.EventSourceChecks`); the per-cloud appends in
`internal/api/handlers/discovery_event_source_recs.go`.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 2](./event-source-tier-slice2.md),
[Orchestration tier slice 2](./orchestration-tier-slice2.md).

## 1. Problem

Event source tier slice 1 (v0.89.99-103) shipped ONE event
source surface per cloud: AWS EventBridge, GCP Pub/Sub, Azure
Service Bus, OCI Streaming. The slice 1 design doc honestly
qualified this:

> "Slice 1 covers ONE event source surface per cloud — slice
> 2+ will add more (SNS / SQS / EventHubs / Event Grid /
> Cloud Tasks / Notification Service)."

Slice 2 (v0.89.104-107) went DEEPER on those 4 surfaces with
per-message propagation analysis but did NOT widen the
surface count.

The gap remains: AWS has SNS + SQS in addition to EventBridge.
Azure has Event Grid + Event Hubs in addition to Service Bus.
GCP has Cloud Tasks in addition to Pub/Sub. OCI has
Notification Service in addition to Streaming. An operator
running a typical AWS-heavy architecture has SNS topics that
Squadron currently ignores.

Slice 3 adds **AWS SNS** as the second AWS event source
surface. Slices 4-6 (separate arcs) will add the corresponding
second surfaces for GCP / Azure / OCI in turn.

### Why SNS first?

1. **Most-requested surface by operators.** SNS topics are
   the canonical "we have an event but where does it go?"
   surface; teams use them as the fan-out layer in
   pub/sub architectures.
2. **Clean detection axes.** Slice 1's EventBridge scanner
   pattern carries over cleanly — list topics, inspect
   attributes, detect trace primitive + log primitive.
3. **Composes with existing arcs.** SNS topics often fan out
   to Lambda (serverless tier), SQS, and HTTP endpoints.
   Once Squadron sees SNS in the inventory, the trace
   integration arc can correlate per-resource emission.

### Why one surface per arc?

A 6-surface arc (SNS + SQS + EventHubs + Event Grid + Cloud
Tasks + Notification Service) would push past the soft cap
multiple times. The slice 1 pattern of one surface per
cloud per arc has held for serverless / orchestration / event
sources slice 1 — keeping the per-arc cap honest preserves
the verification gate quality.

## 2. Non-goals (slice 3)

- **AWS SQS coverage.** SQS queues are the natural pair to
  SNS topics. Slice 4 candidate (still on AWS).
- **GCP Cloud Tasks / Azure Event Grid + Event Hubs / OCI
  Notification Service.** Each gets its own slice (slices 5-7
  candidate).
- **SNS-to-SQS subscription fan-out trace correlation.** When
  an SNS topic publishes to an SQS queue, the trace context
  needs to propagate via message attributes. Slice 3 detects
  the topic's trace primitive; per-subscription propagation
  is slice 2-style deeper work that depends on having the
  subscription surface scanned. Slice 4+.
- **SNS message filter inspection.** Topic subscription
  filters can drop messages based on attributes. Slice 3
  doesn't inspect filters.
- **Per-region multi-account discovery.** SNS lives per-region
  per-account. Slice 3 follows the existing AWS scanner's
  account+region scope.
- **Topic encryption KMS key rotation analysis.** Squadron
  notes whether `KmsMasterKeyId` is set (informational only).
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — AWS SNS

API: `sns:ListTopics`, `sns:GetTopicAttributes`. Required IAM
extension to the existing AWS scanner policy.

Detection axes:

| Axis                       | Source                                                      | Recommendation kind             |
|----------------------------|-------------------------------------------------------------|----------------------------------|
| Has delivery subscriptions | `SubscriptionsConfirmed > 0` (topic has at least one active sub) | `sns-subscriptions-attach`       |
| Delivery status logging    | Topic has CloudWatch Logs delivery status role configured (`DeliveryPolicy.http.deliveryStatusLogging.successFeedbackRoleArn` set OR `SQSSuccessFeedbackRoleArn`/`LambdaSuccessFeedbackRoleArn`/`HTTPSuccessFeedbackRoleArn` set) | `sns-delivery-logging-enable`    |
| Encryption at rest         | `KmsMasterKeyId` is set                                     | informational only               |
| FIFO content dedup         | FIFO topics: `ContentBasedDeduplication = true`             | informational only               |

The "has delivery subscriptions" axis catches dead-topic
orphans — operators frequently leave SNS topics behind
after refactoring; if a topic has zero active subscriptions,
nothing downstream gets the messages. Squadron's
`sns-subscriptions-attach` is more of an audit signal than a
fix recommendation (the operator might intentionally have a
topic without subs as a placeholder).

The delivery status logging axis is the canonical "did the
message get through?" signal. SNS doesn't natively emit OTel
spans, but the per-protocol success/failure feedback to
CloudWatch Logs is the closest cloud-native trace proxy —
mirrors the EventBridge log-target proxy from slice 1.

## 4. Storage schema

NO migration. The existing `event_source_instance` table from
v0.89.100 has the right shape. Slice 3 adds rows with
`provider = "aws"` and `surface = "sns"`.

Schema stays at v15.

## 5. Scanner contract

The existing AWS scanner from slice 1 (v0.89.100) has
`ScanEventSources` returning EventBridge buses. Slice 3
extends the dispatcher to fan out across BOTH surfaces:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot
    
    buses, ebErr := s.ScanEventBridgeBuses(ctx, scope)
    if ebErr == nil {
        all = append(all, buses...)
    }
    
    topics, snsErr := s.ScanSNSTopics(ctx, scope)
    if snsErr == nil {
        all = append(all, topics...)
    }
    
    // Partial-scan posture: if one fails, the other still surfaces
    if ebErr != nil && snsErr != nil {
        return all, fmt.Errorf("both surfaces failed: eb=%w sns=%w", ebErr, snsErr)
    }
    
    return all, nil
}
```

New file `internal/discovery/aws/sns.go` implements
`ScanSNSTopics`.

The SNS API:
- `sns:ListTopics` returns paginated topic ARNs
- `sns:GetTopicAttributes` per topic returns the attribute
  map (SubscriptionsConfirmed, DeliveryPolicy, KmsMasterKeyId,
  FifoTopic, ContentBasedDeduplication, etc.)
- The AWS SDK Go v2 package `github.com/aws/aws-sdk-go-v2/service/sns`
  exposes both. Already in go.mod from previous discovery
  work — no new dependency.

## 6. API surface

The existing per-provider scan + inventory endpoints already
handle the event_sources field generically. Slice 3 just
populates it with more rows when SNS topics exist.

The Discovery summary endpoint's `event_source_count` for AWS
starts increasing by the topic count.

The trace coverage endpoint's `event_source_pct` aggregation
unchanged — slice 1's emission detection logic per resource
correlates by ARN, which works for SNS ARNs.

## 7. UI

The DiscoveryAWS Event sources sub-tab from slice 1 already
renders rows generically by Surface field. Slice 3 SNS rows
render under Surface = "sns". The existing table columns
(Resource Name / Surface / Type / Region / Trace axis / Log
axis / Last seen / Quality) handle SNS rows correctly without
UI changes.

The Workload Health panel from v0.89.132 stays unchanged
(serverless-only scope).

## 8. Recommendation kinds

2 new kinds:

```
sns-subscriptions-attach
sns-delivery-logging-enable
```

The `sns-` prefix is NEW. Webhook routing extends:

```
sns-       → aws
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template for `sns-delivery-logging-enable`:

> "This SNS topic does NOT have CloudWatch Logs delivery
> status feedback configured. Without it, the operator has
> no visibility into per-message delivery success/failure
> for the topic's protocol-specific subscriptions
> (HTTPS / SQS / Lambda / etc.). This Terraform PR
> configures a CloudWatch Logs role for the delivery
> protocols in use.
>
> Decline if your team uses a non-CloudWatch destination for
> delivery audit (custom Lambda processor, SNS-to-Datadog,
> etc.). The verdict learning loop records."

Terraform pattern for `sns-delivery-logging-enable`:

```hcl
resource "aws_iam_role" "sns_delivery_logging_<name>" {
  name = "sns-delivery-logging-${aws_sns_topic.<name>.name}"
  assume_role_policy = data.aws_iam_policy_document.sns_assume.json
}

resource "aws_iam_role_policy_attachment" "sns_delivery_logging_<name>" {
  role       = aws_iam_role.sns_delivery_logging_<name>.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonSNSRole"
}

resource "aws_sns_topic" "<name>" {
  # ... existing fields ...
  
  http_success_feedback_role_arn    = aws_iam_role.sns_delivery_logging_<name>.arn
  http_failure_feedback_role_arn    = aws_iam_role.sns_delivery_logging_<name>.arn
  sqs_success_feedback_role_arn     = aws_iam_role.sns_delivery_logging_<name>.arn
  sqs_failure_feedback_role_arn     = aws_iam_role.sns_delivery_logging_<name>.arn
  lambda_success_feedback_role_arn  = aws_iam_role.sns_delivery_logging_<name>.arn
  lambda_failure_feedback_role_arn  = aws_iam_role.sns_delivery_logging_<name>.arn
}
```

Reasoning template for `sns-subscriptions-attach`:

> "This SNS topic has zero confirmed subscriptions. Messages
> published to the topic are dropped on the floor. Either the
> topic is a leftover from a refactor (and should be deleted)
> OR a downstream consumer needs to subscribe.
>
> Slice 3 of event sources does NOT auto-draft a subscription
> Terraform PR — the operator decides what to subscribe.
> This recommendation surfaces the topic for review."

No Terraform pattern for `sns-subscriptions-attach` — the
recommendation is audit-only. The runbook documents this.

## 9. Slice 3 contract

**In:**

1. AWS `ScanSNSTopics` implementation populating
   `event_source_instance` with surface=sns.
2. AWS `ScanEventSources` dispatcher extension to fan out
   across EventBridge + SNS with partial-scan posture.
3. IAM template extension: `sns:ListTopics`,
   `sns:GetTopicAttributes`.
4. 2 new recommendation kinds: `sns-subscriptions-attach`
   + `sns-delivery-logging-enable`.
5. Webhook routing extends with `sns-` → aws.
6. iacpicker emitter for `sns-delivery-logging-enable`
   Terraform pattern.
7. Operator runbook section (extend
   docs/event-source-tier-operator-guide.md).
8. README index entry updated to mention SNS coverage.
9. Acceptance tests covering SNS detection, both axes,
   webhook routing, cold-start parity, partial-scan
   posture.

**Out:**

- AWS SQS (slice 4).
- GCP Cloud Tasks / Azure Event Grid + Event Hubs / OCI
  Notification Service (slices 5-7).
- SNS-to-SQS subscription fan-out trace correlation.
- SNS message filter inspection.
- Per-region multi-account discovery.
- Topic encryption KMS key rotation analysis.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: AWS SNS scanner + dispatcher extension.**
  ~500-700 lines. **v0.89.138.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~600-800 lines.
  **v0.89.139.**

Total: 2 release tags. Smaller than recent arcs because slice
3 reuses all the existing event source surface scaffolding —
storage, API, UI, audit all carry through.

## 11. Acceptance tests

1. **AWS ScanSNSTopics returns topics** — paginated list
   response is walked.
2. **Topic with SubscriptionsConfirmed > 0 → has_trace_axis
   reflects delivery logging path, has_log_axis reflects
   delivery logging path** (mirrors slice 1 EventBridge
   log-target proxy pattern).
3. **Topic with SubscriptionsConfirmed = 0 → fires
   sns-subscriptions-attach recommendation**.
4. **Topic without delivery feedback role → fires
   sns-delivery-logging-enable** when subscriptions are
   present (the recommendation only fires when there's a
   subscription to log delivery for).
5. **Topic with KmsMasterKeyId → snapshot Detail records
   the encryption flag**.
6. **FIFO topic with ContentBasedDeduplication → snapshot
   Detail records the dedup flag**.
7. **ScanEventSources dispatcher returns BOTH buses AND
   topics**.
8. **Dispatcher partial-scan posture: EventBridge fails →
   SNS topics still surface**.
9. **Dispatcher partial-scan posture: SNS fails →
   EventBridge buses still surface**.
10. **Webhook routes sns-subscriptions-attach to aws**.
11. **Webhook routes sns-delivery-logging-enable to aws**.
12. **Discovery summary AWS event_source_count surfaces
    non-zero when topics exist**.
13. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.136 when no SNS rows trigger
    recommendations.

## 12. Threat model

**Wider AWS IAM permissions.** Slice 3 adds
`sns:ListTopics` and `sns:GetTopicAttributes` to the AWS
scanner policy. Both read-only. Operators get the in-product
IAM upgrade path (#590).

**SNS API rate limits.** SNS has a per-region per-account
ListTopics rate limit (~30 TPS) and GetTopicAttributes
(~30 TPS). For a fleet of 1000 SNS topics in one region,
that's 1000 GetTopicAttributes queries. The substrate's
existing AWS rate limiter (10 RPS for CloudWatch, but SNS
has its own quota) absorbs this — ~100 seconds added to the
scan duration.

**Cost surface.** SNS API queries are free. No new
operator-facing cost decisions per the no-money brief.

**Dispatcher partial-scan posture is load-bearing.** When
EventBridge fails (IAM not yet updated for a connection that
predates v0.89.100) AND SNS succeeds (slice 3 IAM update
applied), the operator should still see SNS topics in the
inventory. The dispatcher returns partial results with an
honest error sum. Pinned by tests 8 + 9.

**False positives on intentional dead topics.** A team may
keep an SNS topic with zero subscriptions as a documentation
placeholder. Squadron's `sns-subscriptions-attach`
recommendation fires; the exclusion table + verdict learning
loop handle. Runbook documents.

**No span content logging.** Slice 3 reads topic metadata
only. No published message content flows through Squadron's
audit chain. PII surface stays at zero.

## 13. Slice 4+ candidates

- **AWS SQS** — second AWS event source surface alongside
  EventBridge + SNS. Slice 4.
- **GCP Cloud Tasks** — second GCP surface. Slice 5.
- **Azure Event Grid** — second Azure surface. Slice 6.
- **Azure Event Hubs** — third Azure surface. Slice 6 or 7.
- **OCI Notification Service** — second OCI surface. Slice 7.
- **SNS-to-SQS subscription fan-out trace correlation** —
  per-subscription propagation detection like slice 2 for
  EventBridge rules. Slice 8+.
- **SNS message filter inspection** — does the subscription's
  filter drop messages with traceparent attributes? Slice 8+.
- **Per-region multi-account fan-out** — coordinate SNS
  scans across accounts when an operator has many. Slice 9+.

---

**Strategic frame:**

Slice 3 starts the widening pass on the event source tier.
Each subsequent slice adds one new surface, keeping the
per-arc cap honest while compounding the claim:

> "Squadron covers SIX event source surfaces across four
> clouds — AWS EventBridge + SNS, GCP Pub/Sub + Cloud Tasks,
> Azure Service Bus + Event Grid, OCI Streaming +
> Notification Service."

The universal claim doesn't grow a new tier or new verb —
the existing event source tier becomes more rigorous. The
operator-meaningful framing: "Squadron sees the event
sources you actually use."

The Tuesday LinkedIn drumbeat narrative gains: "Your AWS
account has 47 SNS topics. 12 of them have zero
subscriptions — they're orphans from a refactor. Squadron
just flagged them for review. The other 35 have active
subscriptions; 11 of those don't have CloudWatch Logs
delivery status feedback configured. Here are the IaC PRs."
