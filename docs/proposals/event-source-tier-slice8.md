# Event source tier slice 8 — Azure Event Hubs (third Azure surface)

**Status:** design doc, locked for slice 8 implementation.
Adds Azure Event Hubs as the third Azure event source surface
alongside Service Bus and Event Grid. Matches AWS's
three-surface count.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 4](./event-source-tier-slice4.md),
[Event source tier slice 6](./event-source-tier-slice6.md),
[Event source tier slice 7](./event-source-tier-slice7.md).

## 1. Problem

The slice 7 widening pass closed at 3-2-2-2. Azure had 2
surfaces (Service Bus + Event Grid); AWS had 3 (EventBridge
+ SNS + SQS). Slice 8 brings Azure to parity at 3 by adding
Event Hubs — the analytics + telemetry intake primitive
analogous to AWS Kinesis + GCP Pub/Sub at high throughput.

Azure's three event source primitives serve distinct
patterns:

- **Service Bus** (slice 1, v0.89.99-103): enterprise
  messaging — queues + topics for transactional message
  delivery with ordering and FIFO guarantees.
- **Event Grid** (slice 6, v0.89.146-148): event
  distribution layer for cloud events (CloudEvents 1.0
  schema). Topics + System Topics for many-to-many event
  routing across Azure services.
- **Event Hubs** (this slice): big-data event ingestion at
  millions of events per second. Streaming analytics +
  telemetry intake pattern. Different design center from
  the messaging primitives — Event Hubs is a partitioned
  log analogous to Kafka, not a queue.

The canonical Azure analytics ingestion architecture is
**Event Hubs namespace → Capture → Blob Storage / ADLS**
with parallel **Event Hubs namespace → Stream Analytics /
ASA / Databricks** consumption. Without Event Hubs coverage,
Squadron misses the telemetry intake layer that feeds
downstream analytics — including, frequently, the
observability pipeline itself (third-party SaaS like Datadog
+ Splunk consume from Event Hubs for Azure platform
telemetry).

### Why now?

1. **Parity completion.** AWS has 3 event source surfaces
   shipped; Azure has 2. Slice 8 closes the asymmetry.
2. **Two clean detection axes.** Event Hubs has both a
   diagnostic-settings axis (mirrors Service Bus + Event
   Grid) AND a Capture axis (auto-archive of events to Blob
   Storage / ADLS — distinctly an Event-Hubs feature). The
   two axes are complementary: diagnostic settings audit
   DELIVERY; Capture audits CONTENT.
3. **Pattern reuse.** The three-way dispatcher pattern is
   already shipped from slice 4 (AWS EventBridge + SNS +
   SQS); slice 8 extends the existing Azure two-way
   dispatcher to three-way using the same partial-scan
   posture.

### What slice 8 does NOT address

- **OCI Queue Service** — slice 9+ candidate.
- **Event Hubs Geo-DR** (paired namespaces for disaster
  recovery) — slice 9+ candidate.
- **Per-hub consumer group inspection.** Slice 8 detects
  Capture at the namespace + at-least-one-hub-enabled level;
  per-consumer-group lag detection is slice 9+.
- **Per-partition throughput unit (TU) utilization
  analysis.** Auto-inflate detection requires per-namespace
  metrics that overlap with the substrate's MetricQuerier;
  honest slice 9+ deferral.
- **Schema Registry integration validation.** Event Hubs
  Schema Registry is an Azure-specific feature; slice 9+
  candidate when adoption justifies.

## 2. Non-goals (slice 8)

- **OCI Queue Service** — slice 9+.
- **Event Hubs Geo-DR** — slice 9+.
- **Per-consumer-group lag detection** — slice 9+.
- **Throughput-unit utilization / auto-inflate analysis** —
  slice 9+.
- **Schema Registry validation** — slice 9+.
- **Private endpoint configuration validation** — slice 9+.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — Azure Event Hubs

API: `Microsoft.EventHub/namespaces` via Azure Resource
Manager. Required Azure RBAC: existing Reader role on the
resource group covers `Microsoft.EventHub/namespaces/read`
+ `Microsoft.EventHub/namespaces/eventhubs/read`.

Detection axes:

| Axis                           | Source                                                                                  | Recommendation kind          |
|--------------------------------|-----------------------------------------------------------------------------------------|-------------------------------|
| Diagnostic settings configured | Namespace has `Microsoft.Insights/diagnosticSettings` child routing to App Insights OR Log Analytics workspace | `eventhubs-diagnostics-enable` |
| Capture enabled                | At least one event hub in the namespace has `properties.captureDescription.enabled == true` | `eventhubs-capture-enable`    |
| Auto-inflate enabled           | `properties.isAutoInflateEnabled == true` (informational only — flag for review)      | informational only            |
| Zone redundant                 | `properties.zoneRedundant == true` (informational only)                                | informational only            |
| Local auth disabled            | `properties.disableLocalAuth == true` (AAD-only auth) (informational only)            | informational only            |
| Namespace status               | `properties.status == "Active"`                                                       | informational only            |

The diagnostic settings axis mirrors slice 1 Service Bus
(v0.89.101) + slice 6 Event Grid (v0.89.147) exactly — same
`Microsoft.Insights/diagnosticSettings` child resource + same
App Insights OR Log Analytics workspace destination check.

The Capture axis is Event-Hubs-specific. Capture
auto-archives events to Blob Storage or Azure Data Lake
Storage at configurable intervals (default 5min / 300MB).
Without Capture, events expire after the namespace's
configured retention window (default 1 day, max 7 days on
Basic / Standard, max 90 days on Premium). When Capture is
disabled across an entire namespace, the operator has no
event-content audit trail beyond the retention window — only
delivery metadata if diagnostic settings are configured.

The Capture detection is at-least-one-hub-enabled because:
- Operators routinely have multiple hubs per namespace with
  different durability requirements (some hubs are ephemeral
  pipelines; others archive long-term).
- A blanket "every hub must have Capture" rule is too
  prescriptive — Squadron flags namespaces with NO Capture
  anywhere as the actionable signal.
- The recommendation Terraform enables Capture on ONE hub
  (operator picks which during review).

## 4. Storage schema

NO migration. The existing `event_source_instance` table
from v0.89.100 has the right shape. Slice 8 adds rows with
`provider = "azure"` and `surface = "eventhubs"`.

Schema stays at v15.

## 5. Scanner contract

The slice 6 Azure scanner has `ScanEventSources` returning
Service Bus + Event Grid via two-way dispatcher. Slice 8
extends to three-way with partial-scan posture mirroring
the slice 4 AWS three-way dispatcher:

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot

    namespaces, sbErr := s.ScanServiceBusNamespaces(ctx, scope)
    if sbErr == nil {
        all = append(all, namespaces...)
    }

    topics, egErr := s.ScanEventGridTopics(ctx, scope)
    if egErr == nil {
        all = append(all, topics...)
    }

    hubs, ehErr := s.ScanEventHubsNamespaces(ctx, scope)
    if ehErr == nil {
        all = append(all, hubs...)
    }

    // Partial-scan posture: error only when ALL THREE failed.
    if sbErr != nil && egErr != nil && ehErr != nil {
        return all, fmt.Errorf("all azure event source surfaces failed: servicebus=%v eventgrid=%v eventhubs=%w", sbErr, egErr, ehErr)
    }

    return all, nil
}
```

New file `internal/discovery/azure/eventhubs.go` implements
`ScanEventHubsNamespaces`.

The Event Hubs API:
- `GET /subscriptions/{sub}/providers/Microsoft.EventHub/namespaces?api-version=2024-01-01`
  returns paginated Event Hubs namespaces across the
  subscription.
- Per-namespace diagnostic settings via the child API:
  `GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.EventHub/namespaces/{name}/providers/Microsoft.Insights/diagnosticSettings?api-version=2021-05-01-preview`
  (same pattern as Service Bus + Event Grid).
- Per-namespace event hubs list via:
  `GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.EventHub/namespaces/{name}/eventhubs?api-version=2024-01-01`
  for the Capture detection axis (walk hubs, check
  `properties.captureDescription.enabled` on each).

The Azure raw-HTTP + ARM signing pattern from slice 1 / slice
6 carries through.

## 6. API surface

Existing per-provider scan + inventory endpoints handle the
event_sources field generically. Slice 8 populates more rows
under provider=azure, surface=eventhubs.

Discovery summary endpoint's `event_source_count` for Azure
starts increasing by the namespace count.

## 7. UI

The DiscoveryAzure Event sources sub-tab renders rows
generically by Surface field. Slice 8 Event Hubs rows render
under Surface = "eventhubs". No UI changes.

## 8. Recommendation kinds

2 new kinds:

```
eventhubs-diagnostics-enable
eventhubs-capture-enable
```

The `eventhubs-` prefix is NEW. Webhook routing extends:

```
eventhubs-       → azure
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`, grouped
alongside `servicebus-` and `eventgrid-` in the Azure family.

Reasoning template for `eventhubs-diagnostics-enable`:

> "This Event Hubs Namespace has no diagnostic settings
> configured. Without diagnostic settings routing to App
> Insights OR a Log Analytics workspace, the operator has no
> visibility into per-namespace delivery health, capture
> status, or throughput unit utilization.
>
> Mirrors the Service Bus `servicebus-diagnostics-enable`
> pattern from slice 1 and the Event Grid
> `eventgrid-diagnostics-enable` pattern from slice 6. This
> Terraform PR configures a
> Microsoft.Insights/diagnosticSettings child routing the
> namespace's events to a Log Analytics workspace.
>
> Decline if your team uses a non-Insights destination
> (custom capture pipeline, third-party SIEM connector).
> The verdict learning loop records."

Reasoning template for `eventhubs-capture-enable`:

> "This Event Hubs Namespace has NO event hub with Capture
> enabled. Without Capture, events expire after the
> namespace's configured retention window (1 day default; 7
> days max on Standard; 90 days max on Premium). The operator
> has no event-content audit trail beyond the retention
> window for incident postmortems.
>
> Capture auto-archives events to Blob Storage or Azure Data
> Lake Storage at configurable intervals (default
> 5min / 300MB). This Terraform PR enables Capture on ONE
> event hub in the namespace (operator picks which during
> review based on which hub carries the durability-critical
> stream).
>
> Decline if your team has an out-of-band consumer pipeline
> doing archival to its own storage tier (Databricks
> ingestion + Delta Lake, Stream Analytics persisting to its
> own destination). The verdict learning loop records."

Terraform pattern for `eventhubs-diagnostics-enable`:

```hcl
resource "azurerm_monitor_diagnostic_setting" "<name>" {
  name                       = "${azurerm_eventhub_namespace.<name>.name}-diag"
  target_resource_id         = azurerm_eventhub_namespace.<name>.id
  log_analytics_workspace_id = var.log_analytics_workspace_id  # operator provides

  enabled_log {
    category = "ArchiveLogs"
  }
  enabled_log {
    category = "OperationalLogs"
  }
  enabled_log {
    category = "AutoScaleLogs"
  }
  enabled_log {
    category = "KafkaCoordinatorLogs"
  }
  enabled_log {
    category = "KafkaUserErrorLogs"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
```

Terraform pattern for `eventhubs-capture-enable`:

```hcl
resource "azurerm_eventhub" "<hub_name>" {
  # ... existing fields ...

  capture_description {
    enabled             = true
    encoding            = "Avro"
    interval_in_seconds = 300       # default
    size_limit_in_bytes = 314572800 # 300 MB default
    skip_empty_archives = true      # operator may tune
    destination {
      name                = "EventHubArchive.AzureBlockBlob"
      storage_account_id  = var.capture_storage_account_id  # operator provides
      blob_container_name = "eventhub-capture"
      archive_name_format = "{Namespace}/{EventHub}/{PartitionId}/{Year}/{Month}/{Day}/{Hour}/{Minute}/{Second}"
    }
  }
}
```

## 9. Slice 8 contract

**In:**

1. Azure `ScanEventHubsNamespaces` implementation populating
   `event_source_instance` with surface=eventhubs.
2. Azure `ScanEventSources` dispatcher extension to fan out
   across Service Bus + Event Grid + Event Hubs with
   three-way partial-scan posture.
3. Azure scanner Reader role already covers
   `Microsoft.EventHub/namespaces/read` +
   `Microsoft.EventHub/namespaces/eventhubs/read` (no IAM
   extension needed beyond what slice 1 + slice 6 already
   covered).
4. 2 new recommendation kinds:
   `eventhubs-diagnostics-enable` +
   `eventhubs-capture-enable`.
5. Webhook routing extends with `eventhubs-` → azure.
6. iacpicker emitters for both Terraform patterns.
7. Operator runbook section.
8. README index entry updated.
9. Acceptance tests covering Event Hubs detection on both
   axes, three-way dispatcher partial-scan posture
   (combinatorial: each of the three surfaces failing
   independently), cold-start parity.

**Out:**

- OCI Queue Service (slice 9+).
- Event Hubs Geo-DR.
- Per-consumer-group lag detection.
- Throughput-unit utilization / auto-inflate analysis.
- Schema Registry validation.
- Private endpoint configuration validation.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Azure Event Hubs scanner + three-way dispatcher
  extension.** ~700-900 lines. **v0.89.153.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~600-800 lines.
  **v0.89.154.**

Total: 2 release tags. Same pattern as slices 3-7.

## 11. Acceptance tests

1. **Azure ScanEventHubsNamespaces returns namespaces** —
   paginated list response is walked.
2. **Namespace with diagnostic settings to App Insights →
   HasLogAxis = true**.
3. **Namespace with diagnostic settings to Log Analytics
   workspace → HasLogAxis = true**.
4. **Namespace without diagnostic settings → HasLogAxis =
   false**.
5. **Namespace with at least one hub having Capture enabled
   → HasContentAuditAxis = true** (uses a new boolean axis
   pinned in the snapshot Detail map for slice 8 — see §3 +
   the Detail mapping in chunk 1).
6. **Namespace with zero hubs having Capture enabled →
   HasContentAuditAxis = false**.
7. **Namespace with empty hubs list (no hubs created yet)
   → HasContentAuditAxis = false (no hubs to audit)** —
   the recommendation does NOT fire on empty namespaces;
   operators get the diagnostic-settings recommendation
   only, which mirrors how slice 6 handles topic-less Event
   Grid namespaces.
8. **Namespace with isAutoInflateEnabled = true → snapshot
   Detail records the flag**.
9. **Namespace with zoneRedundant = true → snapshot Detail
   records the flag**.
10. **Three-way ScanEventSources dispatcher returns Service
    Bus + Event Grid + Event Hubs snapshots**.
11. **Three-way dispatcher partial-scan: Service Bus fails
    → Event Grid + Event Hubs still surface**.
12. **Three-way dispatcher partial-scan: Event Grid fails
    → Service Bus + Event Hubs still surface**.
13. **Three-way dispatcher partial-scan: Event Hubs fails
    → Service Bus + Event Grid still surface**.
14. **Three-way dispatcher: two of three fail → the third
    surface's snapshots still return**.
15. **Three-way dispatcher: all three fail → error
    mentions all three surfaces**.
16. **Webhook routes eventhubs-diagnostics-enable to azure**.
17. **Webhook routes eventhubs-capture-enable to azure**.
18. **Discovery summary Azure event_source_count surfaces
    non-zero when namespaces exist**.
19. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.151 when no Event Hubs rows
    trigger recommendations.

## 12. Threat model

**No new IAM beyond slice 1 + slice 6.** The existing
Reader role on the Azure subscription covers
`Microsoft.EventHub/namespaces/read` +
`Microsoft.EventHub/namespaces/eventhubs/read` + the
diagnostic settings child read.

**Azure ARM rate limits.** The Azure scanner uses the
existing rate limiter shared across slices 1 + 6. Event Hubs
namespaces add 1 list call per subscription + 1 diagnostic
settings call per namespace + 1 hubs list call per
namespace. For a fleet of 100 namespaces averaging 10 hubs
each, that's 1 + 100 + 100 = 201 API calls per scan — well
within Azure's per-subscription ARM rate limit.

The 1 hubs list call per namespace adds incremental load
compared to slice 6's Event Grid (which only needed
diagnostic settings). The per-hub Capture check happens
in-memory after the list response — no per-hub API call.

**Cost surface.** Azure ARM read operations are free. No
new operator-facing cost decisions per the no-money brief.

**Three-way dispatcher partial-scan posture.** Combinatorial
expansion: any one of three surfaces can fail while the
other two surface their snapshots. Same posture as the slice
4 AWS three-way dispatcher (EventBridge + SNS + SQS). Pinned
by tests 11 + 12 + 13 + 14.

**Capture recommendation is operator-prescriptive.** The
`eventhubs-capture-enable` recommendation Terraform enables
Capture on ONE hub. The operator picks WHICH hub during PR
review — Squadron does not prescribe the selection. The
reasoning text emphasizes this so reviewers see the intent
explicitly.

**No span content logging.** Slice 8 reads namespace +
per-hub metadata only. Event Hubs message payloads stay
invisible to Squadron. PII surface stays at zero.

## 13. Slice 9+ candidates

- **OCI Queue Service** — third OCI surface (transactional
  message queues, distinct from ONS pub/sub primitive).
- **Event Hubs Geo-DR** — paired namespace pattern for
  disaster recovery.
- **Per-consumer-group lag detection** — Event Hubs
  per-CG offset lag vs. tail position.
- **Per-partition throughput unit utilization** —
  auto-inflate detection via per-namespace metrics through
  the substrate's MetricQuerier.
- **Schema Registry validation** — Event Hubs Schema
  Registry integration health.
- **Private endpoint configuration validation** — deeper
  network access analysis.

---

**Strategic frame:**

Slice 8 brings Azure to parity with AWS on the event source
tier:

> "Squadron covers TEN event source surfaces across four
> clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks (2 surfaces)
> - Azure: Service Bus + Event Grid + Event Hubs (3 surfaces)
> - OCI: Streaming + Notification Service (2 surfaces)"

After slice 8, the Azure analytics + messaging chain is
fully visible:

1. **Event Hubs namespace** without diagnostic settings
   (this slice `eventhubs-diagnostics-enable`) — operator
   has no per-namespace delivery audit
2. **Event Hubs namespace** without Capture anywhere (this
   slice `eventhubs-capture-enable`) — event-content lost
   after retention window
3. **Event Grid topic** without diagnostic settings (slice
   6 `eventgrid-diagnostics-enable`) — no per-event audit
4. **Service Bus namespace** without diagnostic settings
   (slice 1 `servicebus-diagnostics-enable`) — no queue
   audit
5. **Azure Functions / Logic Apps** without trace primitive
   (serverless + orchestration tiers)
6. **Azure Functions cold-start regression** (substrate's
   three diagnostics) — workload-health view

Six layers. One control plane.

The Tuesday LinkedIn drumbeat narrative gains: "Your Event
Hubs namespace ingests three million events per second from
your platform telemetry pipeline. None of the hubs have
Capture enabled. When a postmortem asks 'what did the event
stream look like at 14:23?', the answer expires from the
namespace four days after the incident — invisibly. Squadron
flagged the namespace for Capture; you pick which hub
durability-critical to archive."
