# Event source tier slice 9 — OCI Queue Service (third OCI surface)

**Status:** design doc, locked for slice 9 implementation.
Adds OCI Queue Service as the third OCI event source surface
alongside Streaming and Notification Service. Brings OCI to
parity with AWS + Azure at 3 surfaces each.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 4](./event-source-tier-slice4.md),
[Event source tier slice 7](./event-source-tier-slice7.md),
[Event source tier slice 8](./event-source-tier-slice8.md).

## 1. Problem

After slice 8 the cross-cloud count stands at 3-2-3-2. OCI
has 2 event source surfaces (Streaming + Notification
Service); AWS and Azure each have 3. Slice 9 brings OCI to 3
by adding Queue Service.

OCI's three event source primitives serve distinct patterns:

- **Streaming** (slice 1, v0.89.99-103): Kafka-compatible
  partitioned streaming with retention policies; the
  analytics + telemetry intake primitive.
- **Notification Service / ONS** (slice 7, v0.89.149-151):
  pub/sub fan-out notifications. The alert + integration
  distribution primitive.
- **Queue Service** (this slice): transactional FIFO message
  queues with at-least-once delivery semantics. The
  task-queue primitive analogous to AWS SQS — distinct from
  ONS pub/sub fan-out (one consumer per message vs.
  many-consumer fan-out).

The canonical OCI task-processing architecture is **Queue
service → Functions / OKE consumers** with dead-letter
queues for poison-message isolation. Without Queue Service
coverage, Squadron misses the queue layer that feeds OCI
task processors — including, frequently, the substrate of
batch processing pipelines on OKE.

### Why now?

1. **Parity completion.** AWS has 3 event source surfaces;
   Azure has 3 (after slice 8); OCI has 2. Slice 9 closes
   the asymmetry. After slice 9, only GCP remains at 2.
2. **One clean detection axis.** OCI Queue Service has a
   single PR-able observability axis — Logging integration
   — mirroring the slice 7 ONS pattern. Slice 9 ships with
   1 recommendation kind, narrow but honest.
3. **Pattern reuse.** The three-way dispatcher pattern is
   already shipped from slice 8 (Azure) and slice 4 (AWS);
   slice 9 extends the existing OCI two-way dispatcher to
   three-way using the same partial-scan posture.

### What slice 9 does NOT address

- **OCI Streaming-Queue interop** — when an OCI Streaming
  pipeline routes into an OCI Queue downstream, the
  cross-surface correlation is slice 10+ candidate.
- **Per-queue dead-letter queue (DLQ) configuration
  inspection.** OCI Queue Service supports DLQ via the
  `maxRetryCount` + redelivery policy. Slice 9 detects
  Logging axis only; DLQ configuration analysis is
  slice 10+.
- **Per-message visibility timeout tuning.** Substrate-
  level analysis of consumer processing lag vs. visibility
  timeout is slice 10+ candidate.
- **Channel-level inspection** (OCI Queue supports
  per-channel routing within a queue). Slice 10+.

## 2. Non-goals (slice 9)

- **OCI Streaming-Queue interop correlation** — slice 10+.
- **DLQ configuration inspection** — slice 10+.
- **Per-message visibility timeout analysis** — slice 10+.
- **Channel-level inspection** — slice 10+.
- **Third GCP surface** — slice 10+ candidate (Cloud
  Pub/Sub Lite, Cloud Dataflow are candidate primitives).
- **Per-queue CMEK / vault key rotation validation** —
  slice 10+.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — OCI Queue Service

API: `/20210201/queues` via the OCI Queue Service endpoint
(`https://messaging.{region}.oci.oraclecloud.com`).
Required OCI IAM: `read queues in compartment` covers the
queue list; the existing Logging read policy from slice 1
(`read log-content` + `read log-groups`) covers the
per-queue Logging detection call.

Detection axes:

| Axis                    | Source                                                                       | Recommendation kind   |
|-------------------------|------------------------------------------------------------------------------|------------------------|
| OCI Logging configured  | OCI Logging /logs returns ≥1 log with `Configuration.Source.Resource == queue.id` | `queues-logging-enable` |
| Queue lifecycle state   | `lifecycleState == "ACTIVE"`                                                | informational only     |
| Visibility timeout      | `visibilityInSeconds` (default 30)                                          | informational only     |
| Retention period        | `retentionInSeconds` (default 24h, max 7 days)                              | informational only     |
| DLQ configured          | `deadLetterQueueDeliveryCount > 0`                                         | informational only     |
| KMS key reference       | `customEncryptionKeyId` recorded if set                                    | informational only     |

The Logging axis mirrors the slice 1 Streaming + slice 7 ONS
patterns exactly — same OCI Logging `/logs` endpoint, same
`searchTerm=<ocid>` convention, same defensive
`Source.Resource` side-check.

DLQ configuration is recorded informationally but does NOT
fire its own recommendation kind in slice 9. The DLQ
configuration analysis (is the DLQ itself logged? is the
retry count appropriate for the consumer's processing
profile?) requires per-queue substrate detection that's
slice 10+ territory.

## 4. Storage schema

NO migration. The existing `event_source_instance` table
from v0.89.100 has the right shape. Slice 9 adds rows with
`provider = "oci"` and `surface = "queues"`.

Schema stays at v15.

## 5. Scanner contract

The slice 7 OCI scanner has `ScanEventSources` returning
Streaming + Notifications via two-way dispatcher. Slice 9
extends to three-way with partial-scan posture mirroring
the slice 8 Azure three-way dispatcher:

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

    queues, qErr := s.ScanQueues(ctx, scope)
    if qErr == nil {
        all = append(all, queues...)
    }

    // Partial-scan posture: error only when ALL THREE failed.
    if strErr != nil && onsErr != nil && qErr != nil {
        return all, fmt.Errorf("oci: all event source surfaces failed: streaming=%v notifications=%v queues=%w", strErr, onsErr, qErr)
    }

    return all, nil
}
```

New file `internal/discovery/oci/scanner_queues.go`
implements `ScanQueues`.

The Queue Service API:
- `GET /20210201/queues?compartmentId={cid}` returns
  paginated queues per compartment (opc-next-page
  pagination, same as Streaming + ONS).
- Per-queue Logging detection reuses the existing
  `listLogsForStream` / `listLogsForTopic` helper pattern
  with a new `listLogsForQueue` (parallel structure, same
  OCI Logging endpoint).

The OCI raw-HTTP + signing pattern carries through. Queue
Service uses a different per-service hostname
(`messaging.{region}.oci.oraclecloud.com`) — the scanner
constructs the endpoint via the existing region helper.

## 6. API surface

Existing per-provider scan + inventory endpoints handle the
event_sources field generically. Slice 9 populates more rows
under provider=oci, surface=queues.

Discovery summary endpoint's `event_source_count` for OCI
starts increasing by the queue count.

## 7. UI

The DiscoveryOCI Event sources sub-tab renders rows
generically by Surface field. Slice 9 Queue rows render
under Surface = "queues". No UI changes.

## 8. Recommendation kinds

1 new kind:

```
queues-logging-enable
```

The `queues-` prefix is NEW. Webhook routing extends:

```
queues-       → oci
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`, grouped
alongside `streaming-` + `ons-` in the OCI family.

Reasoning template for `queues-logging-enable`:

> "This OCI Queue has no OCI Logging configuration. Without
> a log group capturing queue delivery events, the operator
> has no audit trail for which messages were dequeued,
> processed, or sent to the DLQ — critical for postmortem
> analysis of consumer-side failures and poison-message
> investigation.
>
> Mirrors the Streaming `streaming-logging-enable` and ONS
> `ons-logging-enable` patterns from slices 1 and 7. This
> Terraform PR configures an `oci_logging_log` routing
> queue events to a log group (operator's existing log
> group reused via `var.default_log_group_id`; new log
> group created if no operator-default is provided).
>
> Decline if your team routes queue audit through a
> non-OCI-Logging destination (Cloud Guard custom recipe,
> OCI Streaming capture, third-party SIEM connector). The
> verdict learning loop records."

Terraform pattern for `queues-logging-enable`:

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

## 9. Slice 9 contract

**In:**

1. OCI `ScanQueues` implementation populating
   `event_source_instance` with surface=queues.
2. OCI `ScanEventSources` dispatcher extension to fan out
   across Streaming + Notifications + Queues with
   three-way partial-scan posture.
3. OCI scanner IAM policy template extension: add
   `read queues in compartment`. Existing slice 1 Logging
   read policy covers the per-queue detection call.
4. 1 new recommendation kind: `queues-logging-enable`.
5. Webhook routing extends with `queues-` → oci.
6. iacpicker emitter for the Terraform pattern.
7. Operator runbook section.
8. README index entry updated.
9. Acceptance tests covering Queue detection, Logging
   axis, three-way dispatcher partial-scan posture
   (combinatorial), cold-start parity.

**Out:**

- DLQ configuration inspection (slice 10+).
- Per-message visibility timeout analysis (slice 10+).
- Channel-level inspection (slice 10+).
- Third GCP surface (slice 10+).
- Streaming-Queue cross-surface correlation.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: OCI Queue Service scanner + three-way
  dispatcher extension.** ~700-900 lines. **v0.89.156.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~500-700 lines.
  **v0.89.157.**

Total: 2 release tags. Same pattern as slices 3-8.

## 11. Acceptance tests

1. **OCI ScanQueues returns queues** — paginated list
   response walked.
2. **Queue with Logging configured → HasLogAxis = true** —
   shared lookup helper resolves the queue OCID against the
   Logging /logs response.
3. **Queue without Logging configured → HasLogAxis = false**.
4. **Queue with lifecycleState != "ACTIVE" → snapshot
   Detail records the non-active state; snapshot still
   returned**.
5. **Queue with DLQ configured → snapshot Detail records
   the dead_letter_queue_delivery_count**.
6. **Queue with customEncryptionKeyId → snapshot Detail
   records the KMS key reference (informational)**.
7. **Three-way ScanEventSources dispatcher returns
   Streaming + ONS + Queue snapshots**.
8. **Three-way dispatcher partial-scan: Streaming fails →
   ONS + Queues still surface**.
9. **Three-way dispatcher partial-scan: ONS fails →
   Streaming + Queues still surface**.
10. **Three-way dispatcher partial-scan: Queues fails →
    Streaming + ONS still surface**.
11. **Three-way dispatcher: two of three fail → third
    still surfaces**.
12. **Three-way dispatcher: all three fail → error
    mentions all three surfaces**.
13. **Webhook routes queues-logging-enable to oci**.
14. **Discovery summary OCI event_source_count surfaces
    non-zero when queues exist**.
15. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.154 when no Queue rows trigger
    recommendations.

## 12. Threat model

**New IAM permission.** Queue Service adds
`read queues in compartment` to the OCI scanner policy
template. Read-only — Squadron never executes a
PutMessages / CreateQueue / DeleteQueue mutation. The
slice 1 Logging read policy covers the per-queue detection
call without extension.

**OCI Queue Service rate limits.** The OCI scanner uses the
existing rate limiter shared across slices 1 + 7. Queue
Service adds 1 list call per compartment + 1 Logging /logs
call per queue. For a fleet of 200 queues across 10
compartments, that's 210 API calls per scan — well within
OCI's per-tenancy Queue Service rate limit.

**Cost surface.** OCI Queue Service list calls are free.
OCI Logging list calls are free. No new operator-facing
cost decisions per the no-money brief.

**Three-way dispatcher partial-scan posture.** Combinatorial
expansion: any one or two of the three OCI surfaces can fail
while remaining surface(s) surface their snapshots. Same
posture as the slice 8 Azure three-way dispatcher. Pinned
by tests 8-11.

**Logging detection helper sharing.** The existing
`listLogsForStream` / `listLogsForTopic` helpers are
parallel; slice 9 adds `listLogsForQueue` with identical
structure. A future refactor that extracts the common
helper to `listLogsForOCID(resource_ocid)` is slice 10+
candidate but NOT in slice 9 scope (cold-start parity
test 15 pins the existing helpers' behavior byte-identical).

**No span content logging.** Slice 9 reads queue metadata
only. Queue message payloads stay invisible to Squadron.
PII surface stays at zero.

## 13. Slice 10+ candidates

- **Third GCP surface** — Cloud Pub/Sub Lite, Cloud
  Dataflow are candidate primitives to bring GCP to 3
  surfaces (closing the widening at 3-3-3-3).
- **DLQ configuration inspection** — per-queue
  `deadLetterQueueDeliveryCount` + redelivery policy
  analysis; flag queues with no DLQ or with DLQ count
  inappropriate for the consumer's processing profile.
- **Per-message visibility timeout analysis** —
  substrate-level analysis of consumer processing lag
  vs. visibility timeout.
- **Channel-level inspection** — OCI Queue per-channel
  routing detection.
- **Streaming-Queue cross-surface correlation** — when an
  OCI Streaming pipeline routes into an OCI Queue
  downstream, the cross-surface correlation view.
- **Per-queue CMEK / vault key rotation validation** —
  deeper encryption posture.

---

**Strategic frame:**

Slice 9 brings OCI to parity with AWS + Azure on the event
source tier at 3 surfaces. After slice 9, only GCP remains
at 2 surfaces:

> "Squadron covers ELEVEN event source surfaces across four
> clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks (2 surfaces)
> - Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
> - OCI: Streaming + Notification Service + Queue Service (3 surfaces)"

After slice 9, the OCI task-processing chain is fully
visible:

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
OCI. The widening pass leaves GCP as the only 2-surface
cloud — slice 10+ candidate to close at 3-3-3-3 / 12 surfaces.

The Tuesday LinkedIn drumbeat narrative gains: "Your OCI
Queue carries the task pipeline that feeds Functions.
Without OCI Logging configured, when a message lands in
the DLQ at 2am the operator has no record of which
consumer attempted it — only that the DLQ count
incremented. Squadron flagged the queue; one PR enables
delivery logging across the queue's lifecycle events."
