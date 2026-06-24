# Event source tier slice 6 — Azure Event Grid (second Azure surface)

**Status:** design doc, locked for slice 6 implementation.
Continues the widening pass by adding Azure Event Grid as the
second Azure event source surface alongside Service Bus.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 3](./event-source-tier-slice3.md),
[Event source tier slice 4](./event-source-tier-slice4.md),
[Event source tier slice 5](./event-source-tier-slice5.md).

## 1. Problem

The widening pass continues with Azure — the third cloud — by
adding Event Grid alongside Service Bus.

Azure's three event source primitives serve different
patterns:

- **Service Bus** (slice 1, v0.89.99-103): enterprise
  messaging — queues + topics for transactional message
  delivery with ordering and FIFO guarantees.
- **Event Grid** (this slice): event distribution layer for
  cloud events (CloudEvents 1.0 schema). Topics + System
  Topics for many-to-many event routing across Azure
  services.
- **Event Hubs** (slice 7): big-data event ingestion at
  millions of events per second. Different design center —
  streaming analytics + telemetry intake.

The canonical Azure event distribution architecture is
**Event Grid → Service Bus / Functions / Logic Apps**: an
Event Grid Topic publishes CloudEvents-formatted events;
subscribers (Service Bus queues, Functions, Logic Apps, custom
webhooks) consume them via filter rules.

Without Event Grid coverage, Squadron sees the queue layer
(Service Bus) but misses the event distribution layer that
typically routes events INTO Service Bus. The trace continuity
gap operates at the Event Grid → Service Bus boundary: an
Event Grid topic without diagnostic settings means the
operator has no audit trail for which events were delivered
to which subscribers.

### Why Event Grid before Event Hubs?

1. **Architectural parity with Pub/Sub / EventBridge.** Event
   Grid serves the same architectural role as GCP Pub/Sub
   (slice 1) and AWS EventBridge (slice 1) — many-to-many
   event distribution. Service Bus (slice 1) is the queue
   pattern; Event Grid is the topic/fan-out pattern.
2. **Larger adoption surface.** Event Grid is more broadly
   used than Event Hubs across typical Azure deployments.
3. **Cleaner detection axes.** Event Grid's diagnostic
   settings pattern mirrors Service Bus directly (slice 1
   established this pattern). Event Hubs requires per-namespace
   throughput + retention detection that's structurally
   different.

Event Hubs is honest slice 7 deferral.

### What slice 6 does NOT address

- **Azure Event Hubs** — slice 7.
- **OCI Notification Service** — slice 7 alongside Event Hubs.
- **Event Grid Domains** — multi-tenant Event Grid pattern.
  Slice 8+ candidate.
- **Per-subscription filter rule inspection.** Slice 6
  detects topic-level diagnostic settings; per-subscription
  filter rules that drop CloudEvents based on type/source/etc.
  are slice 8+.
- **Per-event CloudEvents schema validation.** Slice 6
  detects whether the topic enforces schema; per-event
  payload validation requires consumer-side substrate.

## 2. Non-goals (slice 6)

- **Azure Event Hubs** — slice 7.
- **OCI Notification Service** — slice 7.
- **Event Grid Domains** — slice 8+.
- **Per-subscription filter rule inspection** — slice 8+.
- **Per-event CloudEvents payload validation** — requires
  consumer-side substrate; slice 8+.
- **Private endpoint configuration validation** — slice 6
  records PublicNetworkAccess as informational; deeper
  private endpoint analysis is slice 8+.
- **Event Grid System Topics for resource-event routing** —
  slice 6 covers Custom Topics (created by user); System
  Topics (auto-created by Azure services like Blob Storage)
  are slice 7+ candidate when adoption justifies.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — Azure Event Grid

API: `Microsoft.EventGrid/topics` via Azure Resource Manager.
Required Azure RBAC: existing Reader role on the resource
group covers `Microsoft.EventGrid/topics/read`.

Detection axes:

| Axis                           | Source                                                                    | Recommendation kind             |
|--------------------------------|---------------------------------------------------------------------------|----------------------------------|
| Diagnostic settings configured | Topic has `Microsoft.Insights/diagnosticSettings` child routing to App Insights OR Log Analytics workspace | `eventgrid-diagnostics-enable`   |
| Input schema enforcement       | `properties.inputSchema` is `"CloudEventSchemaV1_0"` (vs `"EventGridSchema"` or `"CustomEventSchema"`) | `eventgrid-cloudevent-schema-enforce` |
| Public network access          | `properties.publicNetworkAccess` is `"Enabled"` (informational only — flag for review) | informational only               |
| Topic state                    | `properties.provisioningState == "Succeeded"`                            | informational only               |
| Local auth disabled            | `properties.disableLocalAuth == true` (AAD-only auth)                     | informational only               |

The diagnostic settings axis mirrors the slice 1 Service Bus
pattern (v0.89.101) — same Microsoft.Insights/diagnosticSettings
child resource + same App Insights OR Log Analytics workspace
destination check.

The CloudEvents schema enforcement axis is operationally
meaningful: Event Grid topics that accept the proprietary
EventGridSchema OR CustomEventSchema lose interoperability
with non-Azure consumers. CloudEvents 1.0 is the W3C standard;
enforcing it via `inputSchema = "CloudEventSchemaV1_0"` ensures
the topic's events carry standard trace context
(`traceparent` in the CloudEvents distributed tracing
extension).

## 4. Storage schema

NO migration. The existing `event_source_instance` table from
v0.89.100 has the right shape. Slice 6 adds rows with
`provider = "azure"` and `surface = "eventgrid"`.

Schema stays at v15.

## 5. Scanner contract

The existing Azure scanner from slice 1 (v0.89.101) has
`ScanEventSources` returning Service Bus namespaces. Slice 6
extends the dispatcher to two-way (Service Bus + Event Grid)
with partial-scan posture:

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
    
    // Partial-scan posture: error only when BOTH failed
    if sbErr != nil && egErr != nil {
        return all, fmt.Errorf("all azure event source surfaces failed: servicebus=%v eventgrid=%w", sbErr, egErr)
    }
    
    return all, nil
}
```

New file `internal/discovery/azure/eventgrid.go` implements
`ScanEventGridTopics`.

The Event Grid API:
- `GET /subscriptions/{sub}/providers/Microsoft.EventGrid/topics?api-version=2025-02-15`
  returns paginated Event Grid Custom Topics across the
  subscription
- Per-topic `properties.diagnosticSettings` is exposed via
  a separate child API call:
  `GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.EventGrid/topics/{name}/providers/Microsoft.Insights/diagnosticSettings?api-version=2021-05-01-preview`
  (same pattern as slice 1 Service Bus)

The Azure raw-HTTP + ARM signing pattern from
`internal/discovery/azure/servicebus_scanner.go` carries
through.

## 6. API surface

Existing per-provider scan + inventory endpoints handle the
event_sources field generically. Slice 6 populates more rows
under provider=azure, surface=eventgrid.

Discovery summary endpoint's `event_source_count` for Azure
starts increasing by the topic count.

## 7. UI

The DiscoveryAzure Event sources sub-tab renders rows
generically by Surface field. Slice 6 Event Grid rows render
under Surface = "eventgrid". No UI changes.

## 8. Recommendation kinds

2 new kinds:

```
eventgrid-diagnostics-enable
eventgrid-cloudevent-schema-enforce
```

The `eventgrid-` prefix is NEW. Webhook routing extends:

```
eventgrid-       → azure
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template for `eventgrid-diagnostics-enable`:

> "This Event Grid Topic has no diagnostic settings
> configured. Without diagnostic settings routing to App
> Insights OR a Log Analytics workspace, the operator has no
> visibility into per-event delivery success/failure for the
> topic's subscriptions.
>
> Mirrors the Service Bus `servicebus-diagnostics-enable`
> pattern from slice 1. This Terraform PR configures a
> Microsoft.Insights/diagnosticSettings child routing to App
> Insights (operators using Log Analytics directly can
> retarget; either destination satisfies Squadron's log axis
> detection).
>
> Decline if your team uses a non-Insights destination
> (custom webhook capture, etc.). The verdict learning loop
> records."

Reasoning template for `eventgrid-cloudevent-schema-enforce`:

> "This Event Grid Topic has `inputSchema` set to
> `EventGridSchema` (Azure proprietary) OR `CustomEventSchema`
> (operator-defined). CloudEvents 1.0 — the W3C standard —
> is the canonical format for cross-vendor event
> interoperability AND includes the distributed tracing
> extension (`traceparent` in event extensions).
>
> Switching to `CloudEventSchemaV1_0` is a breaking change
> for existing subscribers — they need to consume the
> CloudEvents wire format. This PR proposes the schema
> change; coordinate with subscribers before merging.
>
> Decline if your team has standardized on the proprietary
> Azure schema for ecosystem reasons. The verdict learning
> loop records."

Terraform pattern for `eventgrid-diagnostics-enable`:

```hcl
resource "azurerm_monitor_diagnostic_setting" "<name>" {
  name                       = "${azurerm_eventgrid_topic.<name>.name}-diag"
  target_resource_id         = azurerm_eventgrid_topic.<name>.id
  log_analytics_workspace_id = var.log_analytics_workspace_id  # operator provides
  
  enabled_log {
    category = "PublishFailures"
  }
  enabled_log {
    category = "PublishSuccess"
  }
  enabled_log {
    category = "DeliveryFailures"
  }
  enabled_log {
    category = "DeliverySuccess"
  }
  
  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
```

Terraform pattern for `eventgrid-cloudevent-schema-enforce`:

```hcl
resource "azurerm_eventgrid_topic" "<name>" {
  # ... existing fields ...
  
  input_schema = "CloudEventSchemaV1_0"  # was: EventGridSchema or CustomEventSchema
  # WARNING: changing input_schema is a BREAKING CHANGE for
  # existing subscribers — they must consume the new wire
  # format. Coordinate before merging.
}
```

## 9. Slice 6 contract

**In:**

1. Azure `ScanEventGridTopics` implementation populating
   `event_source_instance` with surface=eventgrid.
2. Azure `ScanEventSources` dispatcher extension to fan out
   across Service Bus + Event Grid with partial-scan posture.
3. Azure scanner Reader role already covers
   `Microsoft.EventGrid/topics/read` (no IAM extension needed
   beyond what slice 1 provided for Service Bus).
4. 2 new recommendation kinds:
   `eventgrid-diagnostics-enable` +
   `eventgrid-cloudevent-schema-enforce`.
5. Webhook routing extends with `eventgrid-` → azure.
6. iacpicker emitters for both Terraform patterns.
7. Operator runbook section.
8. README index entry updated.
9. Acceptance tests covering Event Grid detection, both axes,
   two-way dispatcher partial-scan posture (both directions),
   cold-start parity.

**Out:**

- Azure Event Hubs (slice 7).
- OCI Notification Service (slice 7).
- Event Grid Domains.
- Event Grid System Topics.
- Per-subscription filter rule inspection.
- Per-event CloudEvents payload validation.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: Azure Event Grid scanner + dispatcher extension.**
  ~600-800 lines. **v0.89.147.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~600-800 lines.
  **v0.89.148.**

Total: 2 release tags. Same pattern as slices 3, 4, 5.

## 11. Acceptance tests

1. **Azure ScanEventGridTopics returns topics** — paginated
   list response is walked.
2. **Topic with diagnostic settings to App Insights →
   HasLogAxis = true**.
3. **Topic with diagnostic settings to Log Analytics
   workspace → HasLogAxis = true**.
4. **Topic without diagnostic settings → HasLogAxis = false**.
5. **Topic with inputSchema = "CloudEventSchemaV1_0" →
   HasTraceAxis = true** (CloudEvents schema enables
   trace-context extension).
6. **Topic with inputSchema = "EventGridSchema" →
   HasTraceAxis = false**.
7. **Topic with inputSchema = "CustomEventSchema" →
   HasTraceAxis = false**.
8. **Topic with publicNetworkAccess = "Enabled" → snapshot
   Detail records the flag**.
9. **Topic with disableLocalAuth = true → snapshot Detail
   records the AAD-only flag**.
10. **Two-way ScanEventSources dispatcher returns Service Bus
    namespaces + Event Grid topics**.
11. **Two-way dispatcher partial-scan: Service Bus fails →
    Event Grid still surfaces**.
12. **Two-way dispatcher partial-scan: Event Grid fails →
    Service Bus still surfaces**.
13. **Two-way dispatcher: both fail → error mentions both
    surfaces**.
14. **Webhook routes eventgrid-diagnostics-enable to azure**.
15. **Webhook routes eventgrid-cloudevent-schema-enforce to
    azure**.
16. **Discovery summary Azure event_source_count surfaces
    non-zero when topics exist**.
17. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.145 when no Event Grid rows
    trigger recommendations.

## 12. Threat model

**No new IAM beyond slice 1.** The existing Reader role on
the Azure subscription covers `Microsoft.EventGrid/topics/read`
+ the diagnostic settings child read.

**Azure ARM rate limits.** The Azure scanner already uses the
existing rate limiter from slice 1 Service Bus (and from
v0.89.118 metrics). Event Grid topics add 1 list call per
subscription + 1 diagnostic settings call per topic. For a
fleet of 500 topics, that's 501 API calls per scan — well
within Azure's per-subscription ARM rate limit.

**Cost surface.** Azure ARM read operations are free. No new
operator-facing cost decisions per the no-money brief.

**Two-way dispatcher partial-scan posture.** When Service
Bus fails (RBAC propagation lag on a connection that predates
v0.89.101) AND Event Grid succeeds (slice 6's broader
read-only RBAC), the operator still sees Event Grid topics.
Same in the other direction. Pinned by tests 11 + 12.

**CloudEvents schema enforcement is BREAKING.** The
`eventgrid-cloudevent-schema-enforce` recommendation flags
that switching `inputSchema` BREAKS existing subscribers.
The reasoning text emphasizes coordination with subscribers
before merging — Squadron drafts the PR but the operator's
review catches the breakage risk.

**No span content logging.** Slice 6 reads topic metadata
only. CloudEvent payloads stay invisible to Squadron. PII
surface stays at zero.

## 13. Slice 7+ candidates

- **Azure Event Hubs** — third Azure surface. Slice 7.
- **OCI Notification Service** — second OCI surface. Slice 7.
- **Event Grid Domains** — multi-tenant Event Grid.
  Slice 8+.
- **Event Grid System Topics** — auto-created by Azure
  services. Slice 8+.
- **Per-subscription filter rule inspection** — does the
  filter drop CloudEvents with traceparent?
- **Per-event CloudEvents payload validation** — requires
  consumer-side substrate.
- **Private endpoint configuration validation** — deeper
  network access analysis.

---

**Strategic frame:**

Slice 6 brings Azure into architectural parity with AWS + GCP
on the event source tier:

> "Squadron covers SEVEN event source surfaces across four
> clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks (2 surfaces)
> - Azure: Service Bus + Event Grid (2 surfaces — slice 7 adds
>   Event Hubs)
> - OCI: Streaming (1 surface — slice 7 adds Notification
>   Service)"

After slice 6, the Azure event distribution chain is fully
visible:

1. **Event Grid topic** without diagnostic settings (this
   slice `eventgrid-diagnostics-enable`) — operator has no
   per-event delivery audit
2. **Event Grid topic** with proprietary schema (this slice
   `eventgrid-cloudevent-schema-enforce`) — events lose
   cross-vendor interoperability + W3C trace context
3. **Service Bus namespace** without diagnostic settings
   (slice 1 `servicebus-diagnostics-enable`) — downstream
   queue has no audit
4. **Azure Functions / Logic Apps** without trace primitive
   (serverless + orchestration tiers)
5. **Azure Functions cold-start regression** (substrate's
   three diagnostics) — workload-health view

Five layers. One control plane.

The Tuesday LinkedIn drumbeat narrative gains: "Your Event
Grid topic accepts events in the proprietary EventGridSchema
format. The W3C-standard CloudEvents 1.0 schema includes a
`traceparent` extension that propagates trace context to
subscribers. Squadron flagged the topic for migration — but
warns that the schema change BREAKS existing subscribers.
Coordinate before merging."
