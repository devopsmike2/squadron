# Event source tier slice 10 — GCP Pub/Sub Lite (third GCP surface, closes the widening pass)

**Status:** design doc, locked for slice 10 implementation.
Adds GCP Pub/Sub Lite as the third GCP event source surface
alongside Pub/Sub and Cloud Tasks. Closes the cross-cloud
event source widening pass at **3-3-3-3 / 12 surfaces
across 4 clouds**.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 5](./event-source-tier-slice5.md),
[Event source tier slice 8](./event-source-tier-slice8.md),
[Event source tier slice 9](./event-source-tier-slice9.md).

## 1. Problem

After slice 9 the cross-cloud count stands at 3-2-3-3. GCP
has 2 event source surfaces (Pub/Sub + Cloud Tasks); AWS,
Azure, and OCI each have 3. Slice 10 brings GCP to 3 by
adding Pub/Sub Lite — the high-volume low-latency intake
primitive at a lower price point than full Pub/Sub.

GCP's three event source primitives serve distinct
patterns:

- **Pub/Sub** (slice 1, v0.89.99-103): managed message
  delivery with at-least-once semantics + global routing.
  The general-purpose event distribution primitive.
- **Cloud Tasks** (slice 5, v0.89.143-145): HTTP / App
  Engine task queues with retry policies and scheduling.
  The task-queue primitive analogous to AWS SQS for
  HTTP-target workloads.
- **Pub/Sub Lite** (this slice): zone-pinned, partitioned,
  low-cost high-throughput streaming. The partitioned-log
  primitive analogous to AWS Kinesis Data Streams and
  Azure Event Hubs. Distinct from full Pub/Sub in that
  Lite trades managed routing + global delivery for cost
  efficiency at high volume — operators self-manage
  partition capacity via reservations.

The canonical GCP high-throughput analytics architecture is
**Pub/Sub Lite topic → Dataflow / Cloud Run consumers** with
**reservations** managing per-partition capacity. Without
Pub/Sub Lite coverage, Squadron misses the GCP equivalent
of the partitioned-log intake layer Azure Event Hubs covers
on the Azure side.

### Why now? Why Pub/Sub Lite specifically?

1. **Parity completion — closes the widening pass.** After
   slice 10, all four clouds carry 3 event source surfaces
   each. The Squadron claim "covers every event source
   primitive on every major cloud at 3 surfaces" becomes
   tight + load-bearing.
2. **Architectural symmetry.** Pub/Sub Lite is GCP's
   partitioned-log primitive, the structural analog of
   Azure Event Hubs (slice 8). The detection axes align
   cleanly — Logging configured (Cloud Logging sink) +
   per-reservation capacity allocation.
3. **Operationally meaningful.** Pub/Sub Lite operators
   self-manage capacity via reservations. A topic with no
   reservation attached is throttled to the bare minimum;
   a reservation with no autoscaling configured leaves
   throughput growth unaddressed at peak. Both conditions
   are detectable + recommend-able.

### What slice 10 does NOT address

- **Per-subscription consumer-side lag detection** —
  per-subscription backlog analysis is slice 11+
  candidate.
- **Cross-region reservation analysis** — Pub/Sub Lite is
  zone-pinned by design; cross-region capacity planning
  is the operator's architectural decision, not
  Squadron's recommendation.
- **Schema enforcement on Lite topics** — Pub/Sub Lite
  supports schemas but at narrower fidelity than Pub/Sub;
  honest deferral to slice 11+.
- **Migration recommendations from full Pub/Sub** — when
  to migrate a Pub/Sub topic to Pub/Sub Lite for cost
  reasons requires substrate-level cost modeling outside
  the slice 10 scope.
- **Auto-fix.** Squadron remains a recommender.

## 2. Non-goals (slice 10)

- **Per-subscription consumer-side lag detection** —
  slice 11+.
- **Cross-region reservation analysis** — out of scope.
- **Schema enforcement** — slice 11+.
- **Pub/Sub-to-Lite migration recommendations** — requires
  cost-modeling substrate; out of slice 10 scope.
- **Per-message trace context propagation analysis** —
  slice 11+ candidate using the substrate's MetricQuerier.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — GCP Pub/Sub Lite

API: `pubsublite.googleapis.com/v1/admin/projects/{project}/locations/{zone}/topics`
via the Pub/Sub Lite Admin API.
Required GCP IAM: `pubsublite.topics.list` +
`pubsublite.topics.get` + `pubsublite.reservations.list` +
the existing Logging read scope from slice 1
(`logging.logSinks.list` covers the per-topic Logging axis
detection).

Pub/Sub Lite is **zone-pinned** — list calls require a
zone parameter, not a region. The scanner walks each
configured zone in `scope.Regions` (zones convention reuses
the GCP region field with explicit zone suffix per the
existing GCP scanner pattern).

Detection axes:

| Axis                       | Source                                                                              | Recommendation kind         |
|----------------------------|-------------------------------------------------------------------------------------|------------------------------|
| Cloud Logging configured   | Topic has a Cloud Logging sink filtering on `resource.type="pubsublite_topic" AND resource.labels.topic_id="{topic_id}"` | `pubsublite-logging-enable` |
| Reservation attached       | Topic `properties.reservationConfig.throughputReservation` resolves to an existing reservation | `pubsublite-reservation-attach` |
| Per-partition retention    | `properties.retentionConfig.perPartitionBytes` (informational only)                | informational only           |
| Topic partition count      | `properties.partitionConfig.count` (informational only)                            | informational only           |
| Topic scale tier           | `properties.partitionConfig.capacity` (informational only — publish + subscribe units) | informational only       |

The Logging axis mirrors the slice 1 Pub/Sub pattern via
Cloud Logging sink discovery. The detection helper is a new
`pubsubliteHasLoggingSink` reusing the existing
`listLoggingSinksForResource` walk from slice 1 — the same
`resource.type` + `resource.labels` filter pattern but
keyed against `pubsublite_topic`.

The Reservation axis is Pub/Sub-Lite-specific. Pub/Sub Lite
topics without a reservation attached are throttled to the
bare minimum publish + subscribe throughput per partition,
which becomes a silent bottleneck under peak load.
Detection fires when `topic.reservationConfig` is empty OR
when the referenced reservation OCID does not resolve in
the per-zone reservation list.

## 4. Storage schema

NO migration. The existing `event_source_instance` table
from v0.89.100 has the right shape. Slice 10 adds rows with
`provider = "gcp"` and `surface = "pubsublite"`.

Schema stays at v15.

## 5. Scanner contract

The slice 5 GCP scanner has `ScanEventSources` returning
Pub/Sub + Cloud Tasks via two-way dispatcher. Slice 10
extends to three-way with partial-scan posture mirroring
the slice 8 Azure / slice 9 OCI three-way dispatchers:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot

    topics, psErr := s.ScanPubSubTopics(ctx, scope)
    if psErr == nil {
        all = append(all, topics...)
    }

    tasks, ctErr := s.ScanCloudTasksQueues(ctx, scope)
    if ctErr == nil {
        all = append(all, tasks...)
    }

    liteTopics, pslErr := s.ScanPubSubLiteTopics(ctx, scope)
    if pslErr == nil {
        all = append(all, liteTopics...)
    }

    if psErr != nil && ctErr != nil && pslErr != nil {
        return all, fmt.Errorf("gcp: all event source surfaces failed: pubsub=%v cloudtasks=%v pubsublite=%w", psErr, ctErr, pslErr)
    }

    return all, nil
}
```

New file `internal/discovery/gcp/pubsublite_scanner.go`
implements `ScanPubSubLiteTopics`.

The Pub/Sub Lite Admin API:
- `GET pubsublite.googleapis.com/v1/admin/projects/{project}/locations/{zone}/topics`
  returns the list of Lite topics in a zone.
- Per-topic reservation resolution via the topic's
  `reservationConfig.throughputReservation` (already
  embedded in the list response — no extra API call).
- Per-zone reservation list:
  `GET pubsublite.googleapis.com/v1/admin/projects/{project}/locations/{zone}/reservations`
  — only called when at least one topic in the zone has a
  reservation reference, to resolve the references.

## 6. API surface

Existing per-provider scan + inventory endpoints handle
the event_sources field generically. Slice 10 populates
more rows under provider=gcp, surface=pubsublite.

Discovery summary endpoint's `event_source_count` for GCP
starts increasing by the Lite topic count.

## 7. UI

The DiscoveryGCP Event sources sub-tab renders rows
generically by Surface field. Slice 10 Pub/Sub Lite rows
render under Surface = "pubsublite". No UI changes.

## 8. Recommendation kinds

2 new kinds:

```
pubsublite-logging-enable
pubsublite-reservation-attach
```

The `pubsublite-` prefix is NEW. Webhook routing extends:

```
pubsublite-       → gcp
```

The 1 new prefix extends the existing kind-prefix switch
in `internal/api/handlers/iac_github_webhook.go`, grouped
alongside `pubsub-` + `cloudtasks-` in the GCP family.

Reasoning template for `pubsublite-logging-enable`:

> "This Pub/Sub Lite topic has no Cloud Logging sink
> configured. Without a sink filtering on
> `resource.type=\"pubsublite_topic\"` + the topic's ID,
> the operator has no audit trail for publish failures,
> per-partition throughput exhaustion events, or
> reservation-related throttling — the failure modes
> unique to the Lite tier.
>
> Mirrors the slice 1 Pub/Sub `pubsub-trace-enable`
> pattern through the Cloud Logging sink primitive. This
> Terraform PR configures a
> `google_logging_project_sink` filtering on the topic's
> ID, with destination defaulting to a BigQuery dataset
> via `var.pubsublite_logging_dataset_id`.
>
> Decline if your team routes Lite topic audit through a
> non-Cloud-Logging destination (Stackdriver custom
> exporter, third-party SIEM). The verdict learning loop
> records."

Reasoning template for `pubsublite-reservation-attach`:

> "This Pub/Sub Lite topic has NO reservation attached
> (or the referenced reservation does not exist in the
> topic's zone). Without a reservation, the topic is
> throttled to the bare minimum publish + subscribe
> throughput per partition — typically becoming a silent
> bottleneck under peak load.
>
> This Terraform PR creates a
> `google_pubsub_lite_reservation` sized for the topic's
> expected peak throughput AND updates the topic's
> `reservation_config.throughput_reservation` reference.
> Sizing defaults conservative (4 publish + subscribe
> units); operator tunes the
> `throughput_capacity` value for actual peak.
>
> Decline if your team intentionally runs Lite topics at
> minimum-throughput floor for cost reasons (the topic is
> below the per-zone reservation breakeven). The verdict
> learning loop records."

Terraform pattern for `pubsublite-logging-enable`:

```hcl
resource "google_logging_project_sink" "<name>_lite_log_sink" {
  name = "pubsublite-${google_pubsub_lite_topic.<name>.name}-audit"
  destination = "bigquery.googleapis.com/projects/${var.project_id}/datasets/${var.pubsublite_logging_dataset_id}"

  filter = <<-EOT
    resource.type="pubsublite_topic"
    resource.labels.topic_id="${google_pubsub_lite_topic.<name>.name}"
  EOT

  unique_writer_identity = true
}
```

Terraform pattern for `pubsublite-reservation-attach`:

```hcl
resource "google_pubsub_lite_reservation" "<name>_reservation" {
  name                = "${google_pubsub_lite_topic.<name>.name}-reservation"
  project             = var.project_id
  region              = var.lite_region  # operator provides; must match topic zone

  # Conservative default: 4 publish + subscribe units. Operator tunes
  # for actual peak throughput.
  throughput_capacity = 4
}

resource "google_pubsub_lite_topic" "<name>" {
  # ... existing fields ...

  reservation_config {
    throughput_reservation = google_pubsub_lite_reservation.<name>_reservation.name
  }
}
```

## 9. Slice 10 contract

**In:**

1. GCP `ScanPubSubLiteTopics` implementation populating
   `event_source_instance` with surface=pubsublite.
2. GCP `ScanEventSources` dispatcher extension to fan out
   across Pub/Sub + Cloud Tasks + Pub/Sub Lite with
   three-way partial-scan posture.
3. GCP scanner IAM policy template extension:
   `pubsublite.topics.list` + `pubsublite.topics.get` +
   `pubsublite.reservations.list`. Existing slice 1
   Logging read scope covers per-topic detection.
4. 2 new recommendation kinds:
   `pubsublite-logging-enable` +
   `pubsublite-reservation-attach`.
5. Webhook routing extends with `pubsublite-` → gcp.
6. iacpicker emitters for both Terraform patterns.
7. Operator runbook section.
8. README index entry updated — **closes the cross-cloud
   widening pass at 3-3-3-3 / 12 surfaces**.
9. Acceptance tests covering Pub/Sub Lite detection on
   both axes, three-way dispatcher partial-scan posture
   (combinatorial), cold-start parity.

**Out:**

- Per-subscription consumer-side lag detection.
- Cross-region reservation analysis.
- Schema enforcement.
- Pub/Sub-to-Lite migration recommendations.
- Per-message trace context propagation analysis.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: GCP Pub/Sub Lite scanner + three-way
  dispatcher extension.** ~700-900 lines. **v0.89.159.**
- **Chunk 2: Proposer prompt + iacpicker + webhook
  routing + runbook update + README index.** ~600-800
  lines. **v0.89.160.**

Total: 2 release tags. Same pattern as slices 3-9.

## 11. Acceptance tests

1. **GCP ScanPubSubLiteTopics returns topics** —
   zone-walked list response is walked across configured
   zones.
2. **Topic with Cloud Logging sink filtering on the
   topic ID → HasLogAxis = true**.
3. **Topic without Cloud Logging sink → HasLogAxis =
   false**.
4. **Topic with reservation reference resolving to an
   existing reservation → Detail[has_reservation] = true**.
5. **Topic with reservation reference NOT resolving (dead
   reference) → Detail[has_reservation] = false**.
6. **Topic with empty reservationConfig →
   Detail[has_reservation] = false**.
7. **Three-way ScanEventSources dispatcher returns
   Pub/Sub + Cloud Tasks + Pub/Sub Lite snapshots**.
8. **Three-way dispatcher partial-scan: Pub/Sub fails →
   Cloud Tasks + Lite still surface**.
9. **Three-way dispatcher partial-scan: Cloud Tasks
   fails → Pub/Sub + Lite still surface**.
10. **Three-way dispatcher partial-scan: Lite fails →
    Pub/Sub + Cloud Tasks still surface**.
11. **Three-way dispatcher: two of three fail → third
    still surfaces**.
12. **Three-way dispatcher: all three fail → error
    mentions all three surfaces**.
13. **Webhook routes pubsublite-logging-enable to gcp**.
14. **Webhook routes pubsublite-reservation-attach to gcp**.
15. **Discovery summary GCP event_source_count surfaces
    non-zero when Lite topics exist**.
16. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.157 when no Pub/Sub Lite rows
    trigger recommendations.

## 12. Threat model

**New IAM permissions.** Pub/Sub Lite adds
`pubsublite.topics.list`, `pubsublite.topics.get`, and
`pubsublite.reservations.list` to the GCP scanner role.
Read-only — Squadron never executes a
PublishMessages / CreateTopic / DeleteTopic /
CreateReservation mutation. The existing Logging read
scope covers the per-topic sink detection call.

**Pub/Sub Lite Admin API rate limits.** The Admin API has
generous quotas at the project level. Per-zone list calls
+ per-zone reservation lookups for fleets of <500 topics
are well within quota.

**Zone-pinned scope.** Pub/Sub Lite topics are zone-scoped
(not regional). The scanner walks each zone in
`scope.Regions` independently. A zone-failure on one of
{us-east1-a, us-east1-b} leaves the OTHER zone's topics
surfacing — pinned by partial-scan posture at the per-zone
level inside ScanPubSubLiteTopics (independent of the
three-way dispatcher posture above it).

**Cost surface.** Admin API list calls are free. No new
operator-facing cost decisions per the no-money brief.

**Three-way dispatcher partial-scan posture.**
Combinatorial expansion: any one or two of three GCP event
source surfaces can fail while remaining surface(s)
surface their snapshots. Same posture as slice 8 Azure /
slice 9 OCI. Pinned by tests 8-11.

**Reservation recommendation Terraform creates a NEW
resource.** The `pubsublite-reservation-attach`
recommendation creates a `google_pubsub_lite_reservation`
resource — meaning the recommendation is operator-incurred
cost. The reasoning text emphasizes this so PR reviewers
see the cost implication explicitly. Default sizing is
conservative (4 publish + subscribe units) but the
operator must validate against actual peak throughput
before merging. This is the FIRST recommendation in the
event source tier that creates a billable resource —
prior kinds only configured Logging sinks or attached to
existing resources. The verdict learning loop's decline
path is load-bearing here for operators who deliberately
run below reservation breakeven.

**No span content logging.** Slice 10 reads topic +
reservation metadata only. Lite message payloads stay
invisible to Squadron. PII surface stays at zero.

## 13. Slice 11+ candidates

- **Per-subscription consumer-side lag detection** —
  Pub/Sub Lite subscription backlog vs. publish rate.
- **Cross-region disaster-recovery analysis** — Pub/Sub
  Lite is zone-pinned by design; multi-zone redundancy
  patterns deserve a separate analysis surface.
- **Schema enforcement** — Pub/Sub Lite schema fidelity
  analysis.
- **Pub/Sub-to-Lite migration recommendations** — when
  to migrate a full Pub/Sub topic to Lite for cost
  reasons; requires substrate-level cost modeling.
- **Per-message trace context propagation analysis** —
  using the substrate's MetricQuerier to detect whether
  publisher-side traceparent is making it across the
  partition.

---

**Strategic frame:**

Slice 10 **closes the cross-cloud event source widening
pass**. After slice 10, all four clouds carry 3 event
source surfaces each:

> "Squadron covers TWELVE event source surfaces across
> four clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks + Pub/Sub Lite (3 surfaces)
> - Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
> - OCI: Streaming + Notification Service + Queue Service (3 surfaces)"

The widening pass is complete. After slice 10, the event
source tier's surface count is **3-3-3-3 / 12** — a
4-by-3 grid. The Tuesday LinkedIn drumbeat narrative
gains the strategic close: "Squadron now covers every
event source primitive on every major cloud at three
surfaces each. Twelve surfaces. Four clouds. One control
plane."

The next horizon work is NOT widening — it's deepening:
per-subscription consumer-side analysis (slice 11+),
substrate-level cost modeling for migration
recommendations, cross-surface correlation views
(Streaming-Queue, Pub/Sub-Lite, EventBridge-SQS), and
slicing into the orchestration + serverless tiers where
operator feedback identifies the highest-leverage gaps.

After slice 10, the event source tier's structural arc is
closed. Future event source work is per-axis depth, not
per-cloud breadth.
