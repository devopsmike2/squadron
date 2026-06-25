# Event source tier slice 7 — OCI Notification Service (second OCI surface)

**Status:** design doc, locked for slice 7 implementation.
Closes the cross-cloud widening pass by adding OCI Notification
Service as the second OCI event source surface alongside Streaming.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 3](./event-source-tier-slice3.md),
[Event source tier slice 4](./event-source-tier-slice4.md),
[Event source tier slice 5](./event-source-tier-slice5.md),
[Event source tier slice 6](./event-source-tier-slice6.md).

## 1. Problem

The widening pass closes with OCI — the fourth cloud — by adding
Notification Service alongside Streaming.

OCI's two event source primitives serve different patterns:

- **Streaming** (slice 1, v0.89.99-103): Kafka-compatible
  streaming with retention policies; the analytics + telemetry
  intake pattern (parallels Azure Event Hubs + AWS Kinesis).
- **Notification Service / ONS** (this slice): pub/sub
  notifications — topics + subscriptions for fan-out delivery
  to HTTP(S), email, PagerDuty, Slack, Functions, etc. The
  classic alert + integration distribution pattern (parallels
  AWS SNS + GCP Pub/Sub on the alert-out side).

The canonical OCI alerting + integration architecture is
**Monitoring alarm → ONS topic → subscribers**. A Monitoring
alarm fires; ONS distributes to operations channels. Without
ONS coverage, Squadron sees the streaming intake layer
(Streaming) but misses the alert + integration distribution
layer that typically routes operational signals OUT of OCI.

The trace continuity gap operates at the ONS topic → subscriber
boundary: a topic without OCI Logging configured means the
operator has no audit trail for which alarms were delivered
to which subscribers — a critical gap for incident
postmortems where "did PagerDuty actually get the page?" is
the first question.

### Why ONS closes the OCI widening pass

1. **Architectural parity with SNS (AWS) + Pub/Sub (GCP) on
   the pub/sub side.** ONS is OCI's pub/sub fan-out primitive
   — the analog Squadron already covers on the other three
   clouds.
2. **Operational criticality.** Most OCI deployments route
   Monitoring alarms through ONS. Missing ONS visibility means
   missing the most operationally critical event source in
   typical OCI workloads.
3. **Clean detection axis.** ONS's OCI Logging integration
   mirrors the slice 1 Streaming pattern directly (same
   Logging /logs detection + same searchTerm walk). The
   scanner reuses the slice 1 Logging detection helper.

### What slice 7 does NOT address

- **Azure Event Hubs** — slice 8+ candidate.
- **OCI Queue Service** — distinct primitive (transactional
  message queues); slice 8+ candidate.
- **Per-subscription protocol enforcement.** Slice 7 detects
  topic-level Logging axis; per-subscription HTTP→HTTPS or
  retry-policy enforcement is slice 8+ (the recommendation
  target is the subscription resource, not the topic resource;
  iacpicker placement is meaningfully different).
- **Per-message delivery audit.** Slice 7 detects whether the
  Logging axis is configured; per-delivery audit
  reconstruction from the Logging stream is slice 8+
  candidate when adoption justifies.
- **ONS Subscription confirmation lag detection.** Subscriptions
  in PENDING state for >24h indicate misconfiguration; slice
  8+ candidate.

## 2. Non-goals (slice 7)

- **Azure Event Hubs** — slice 8+.
- **OCI Queue Service** — slice 8+.
- **Per-subscription protocol / retry-policy enforcement** —
  slice 8+.
- **Per-delivery audit reconstruction** — slice 8+.
- **ONS Subscription confirmation lag detection** — slice 8+.
- **CMEK / vault integration validation** — slice 7 records
  the topic's KMS key informationally; deeper key rotation
  analysis is slice 8+ candidate.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — OCI Notification Service

API: `/20181201/topics` via the OCI ONS endpoint
(`https://notification.{region}.oraclecloud.com`).
Required OCI IAM: `read ons-topics in compartment` covers the
topic list; the existing Logging read policy from slice 1
(`read log-content-content in tenancy` /
`read log-groups in tenancy`) covers the per-topic Logging
detection call.

Detection axes:

| Axis                    | Source                                                           | Recommendation kind   |
|-------------------------|------------------------------------------------------------------|------------------------|
| OCI Logging configured  | OCI Logging /logs returns ≥1 log with `Configuration.Source.Resource == topic.id` | `ons-logging-enable`   |
| Topic lifecycle state   | Topic `lifecycleState == "ACTIVE"`                              | informational only     |
| Short topic name        | Topic `shortTopicName` populated (vs synthesized OCID-only)     | informational only     |
| Subscription count      | Per-topic subscription count via paginated `/20181201/subscriptions?topicId=` walk | informational only |
| KMS key reference       | Topic `apiEndpoint` reflects regional endpoint; `kmsKeyId` recorded if set | informational only |

The Logging axis mirrors the slice 1 Streaming pattern
(v0.89.101c) — same OCI Logging /logs call + same searchTerm
walk. The scanner shares the existing Logging detection helper
from `scanner_streaming.go` (extracted as a package-internal
helper in chunk 1).

Subscription count is informational only — a topic with 0
subscriptions is operationally meaningful (alarms route
nowhere) but the recommendation is the operator's
configuration choice, not a Squadron-PRed fix.

## 4. Storage schema

NO migration. The existing `event_source_instance` table from
v0.89.100 has the right shape. Slice 7 adds rows with
`provider = "oci"` and `surface = "notifications"`.

Schema stays at v15.

## 5. Scanner contract

The existing OCI scanner from slice 1 (v0.89.101c) has
`ScanEventSources` returning Streaming snapshots only:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    return s.ScanStreams(ctx, scope)
}
```

Slice 7 extends the dispatcher to two-way (Streaming +
Notifications) with partial-scan posture mirroring the
slice 6 Azure two-way dispatcher:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot

    streams, strErr := s.ScanStreams(ctx, scope)
    if strErr == nil {
        all = append(all, streams...)
    }

    topics, onsErr := s.ScanNotificationTopics(ctx, scope)
    if onsErr == nil {
        all = append(all, topics...)
    }

    // Partial-scan posture: error only when BOTH failed
    if strErr != nil && onsErr != nil {
        return all, fmt.Errorf("all oci event source surfaces failed: streaming=%v notifications=%w", strErr, onsErr)
    }

    return all, nil
}
```

New file `internal/discovery/oci/scanner_ons.go` implements
`ScanNotificationTopics`.

The ONS API:
- `GET /20181201/topics?compartmentId={cid}` returns paginated
  topics (opc-next-page pagination, same as Streaming).
- Per-topic Logging detection reuses the slice 1
  `lookupLogResourceForOCID` helper (extracted package-internal
  in chunk 1).

The OCI raw-HTTP + signing pattern from
`internal/discovery/oci/scanner_streaming.go` carries through.
ONS uses a different per-service hostname
(`notification.{region}.oraclecloud.com`) — the scanner
constructs the endpoint via the existing region helper.

## 6. API surface

Existing per-provider scan + inventory endpoints handle the
event_sources field generically. Slice 7 populates more rows
under provider=oci, surface=notifications.

Discovery summary endpoint's `event_source_count` for OCI
starts increasing by the topic count.

## 7. UI

The DiscoveryOCI Event sources sub-tab renders rows generically
by Surface field. Slice 7 ONS rows render under
Surface = "notifications". No UI changes.

## 8. Recommendation kinds

1 new kind:

```
ons-logging-enable
```

The `ons-` prefix is NEW. Webhook routing extends:

```
ons-       → oci
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template for `ons-logging-enable`:

> "This ONS Topic has no OCI Logging configuration. Without a
> log group capturing topic delivery events, the operator has
> no audit trail for which alarms / notifications were
> delivered to which subscribers — the first question in any
> incident postmortem where the operator needs to confirm
> 'did the page actually get sent?'.
>
> Mirrors the Streaming `streaming-logging-enable` pattern
> from slice 1. This Terraform PR configures an
> `oci_logging_log` routing the topic's delivery events to a
> log group (operator's existing log group reused; new log
> group created if no operator-default is provided via
> `var.default_log_group_id`).
>
> Decline if your team routes ONS audit through a non-OCI-Logging
> destination (Cloud Guard custom recipe, OCI Streaming
> capture, third-party SIEM connector). The verdict learning
> loop records."

Terraform pattern for `ons-logging-enable`:

```hcl
resource "oci_logging_log" "<name>" {
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

## 9. Slice 7 contract

**In:**

1. OCI `ScanNotificationTopics` implementation populating
   `event_source_instance` with surface=notifications.
2. OCI `ScanEventSources` dispatcher extension to fan out
   across Streaming + Notifications with partial-scan
   posture.
3. OCI scanner IAM policy template extension: add
   `read ons-topics in compartment`. Existing Logging read
   policy from slice 1 covers the per-topic Logging detection
   call.
4. 1 new recommendation kind: `ons-logging-enable`.
5. Webhook routing extends with `ons-` → oci.
6. iacpicker emitter for the Terraform pattern.
7. Operator runbook section.
8. README index entry updated.
9. Acceptance tests covering ONS detection, Logging axis,
   two-way dispatcher partial-scan posture (both directions),
   cold-start parity.

**Out:**

- Azure Event Hubs (slice 8+).
- OCI Queue Service (slice 8+).
- Per-subscription protocol / retry-policy enforcement.
- Per-delivery audit reconstruction.
- ONS Subscription confirmation lag detection.
- CMEK / vault deeper validation.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: OCI ONS scanner + dispatcher extension.**
  ~600-800 lines. **v0.89.150.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~500-700 lines.
  **v0.89.151.**

Total: 2 release tags. Same pattern as slices 3, 4, 5, 6.

## 11. Acceptance tests

1. **OCI ScanNotificationTopics returns topics** — paginated
   list response is walked.
2. **Topic with Logging configured → HasLogAxis = true** —
   shared lookup helper resolves the topic OCID against the
   Logging /logs response.
3. **Topic without Logging configured → HasLogAxis = false**.
4. **Topic with lifecycleState != "ACTIVE" → snapshot Detail
   records the non-active state, snapshot still returned**.
5. **Topic with kmsKeyId set → snapshot Detail records the
   KMS key reference**.
6. **Topic with 0 subscriptions → snapshot Detail records
   subscription_count=0** (informational signal).
7. **Two-way ScanEventSources dispatcher returns Streaming
   streams + ONS topics**.
8. **Two-way dispatcher partial-scan: Streaming fails → ONS
   topics still surface**.
9. **Two-way dispatcher partial-scan: ONS fails → Streaming
   streams still surface**.
10. **Two-way dispatcher: both fail → error mentions both
    surfaces**.
11. **Webhook routes ons-logging-enable to oci**.
12. **Discovery summary OCI event_source_count surfaces
    non-zero when topics exist**.
13. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.148 when no ONS rows trigger
    recommendations.

## 12. Threat model

**New IAM permission.** ONS adds
`read ons-topics in compartment` to the OCI scanner policy
template. Read-only — Squadron never executes a
PublishMessage / CreateTopic / DeleteTopic mutation. The
slice 1 Logging read policy covers the per-topic detection
call without extension.

**OCI ONS rate limits.** The OCI scanner already uses the
existing rate limiter from slice 1 Streaming (and from
v0.89.118 metrics). ONS topics add 1 list call per
compartment + 1 Logging /logs call per topic. For a fleet of
500 topics spread across 10 compartments, that's 510 API
calls per scan — well within OCI's per-tenancy ONS rate
limit (default 100 req/sec per region).

**Cost surface.** OCI ONS list calls are free. OCI Logging
list calls are free. No new operator-facing cost decisions
per the no-money brief.

**Two-way dispatcher partial-scan posture.** When Streaming
fails (compartment IAM propagation lag on a connection that
predates v0.89.101c) AND ONS succeeds (slice 7's broader
read-only IAM), the operator still sees ONS topics. Same in
the other direction. Pinned by tests 8 + 9.

**Logging detection helper extraction.** The existing
`lookupLogResourceForOCID` helper in `scanner_streaming.go`
is extracted package-internal in chunk 1. The Streaming
detection path stays byte-identical to v0.89.148 — the
helper is shared, not rewritten. Cold-start parity test 13
pins this.

**No span content logging.** Slice 7 reads topic metadata
only. ONS message payloads stay invisible to Squadron. PII
surface stays at zero.

## 13. Slice 8+ candidates

- **Azure Event Hubs** — third Azure surface.
- **OCI Queue Service** — third OCI surface (transactional
  message queues).
- **Per-subscription protocol enforcement** — HTTP → HTTPS
  recommendation at subscription scope.
- **Per-subscription retry-policy tuning** — extending
  `deliveryPolicy.maxRetryDuration` on subscriptions with
  short default retries.
- **ONS Subscription confirmation lag detection** — flag
  PENDING subscriptions older than 24h.
- **CMEK / vault key rotation validation** — deeper
  encryption posture.
- **Per-delivery audit reconstruction** — assemble per-event
  delivery timelines from the Logging stream.

---

**Strategic frame:**

Slice 7 closes the cross-cloud widening pass for the event
source tier:

> "Squadron covers NINE event source surfaces across four
> clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks (2 surfaces)
> - Azure: Service Bus + Event Grid (2 surfaces)
> - OCI: Streaming + Notification Service (2 surfaces)"

After slice 7, the OCI alerting + integration chain is fully
visible:

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

Five layers. One control plane. Four clouds — fully widened
on the event source tier.

The Tuesday LinkedIn drumbeat narrative gains: "Your ONS
topic routes critical Monitoring alarms to PagerDuty. The
topic has no OCI Logging configured. Squadron flagged it.
When the page didn't fire last Friday, the postmortem
question is now answerable in two clicks instead of three
hours."
