# Event source tier slice 5 — GCP Cloud Tasks (second GCP surface)

**Status:** design doc, locked for slice 5 implementation.
Continues the widening pass by adding GCP Cloud Tasks as the
second GCP event source surface alongside Pub/Sub.

**See also:**
[Event source tier slice 1](./event-source-tier-slice1.md),
[Event source tier slice 3](./event-source-tier-slice3.md),
[Event source tier slice 4](./event-source-tier-slice4.md).

## 1. Problem

Slices 3 + 4 added AWS SNS + SQS to the event source tier,
completing the canonical AWS pub/sub fan-out chain. The
widening pass continues with GCP — the second cloud — by
adding Cloud Tasks alongside Pub/Sub.

GCP's two event source primitives serve different patterns:

- **Pub/Sub** (slice 1, v0.89.99-103): pub/sub fan-out for
  loosely-coupled message distribution. Topic publishes →
  multiple subscriptions deliver.
- **Cloud Tasks** (this slice): guaranteed sequential HTTP
  delivery for queue-based work distribution. Queue holds
  tasks → single worker pulls or push delivery to an HTTP
  endpoint, with retry semantics on failure.

The canonical GCP pub/sub-with-retry architecture is **Pub/Sub
→ Cloud Tasks → HTTP target**: a Pub/Sub topic fans out to
many subscribers; each subscriber adds work items to a Cloud
Tasks queue; the queue drives an HTTP endpoint (Cloud Run /
Cloud Functions / external service) with retry-on-failure
semantics.

Without Cloud Tasks coverage, Squadron sees the front of GCP's
event distribution layer (Pub/Sub) but misses the durable
retry layer. The "where did my message get retried before
giving up?" diagnostic surfaces a real failure mode operators
hit when:

- A Cloud Tasks queue has `maxAttempts = 0` (or unset)
  → on HTTP target failure, the task is dropped silently
  after the first attempt. Equivalent to SQS without a
  redrive policy.
- A Cloud Tasks queue has no Stackdriver Logging
  configuration → the operator has no per-task delivery
  status audit trail.

Slice 5 adds Cloud Tasks coverage with detection axes that
mirror the slice 4 SQS pattern: retry config presence (trace
axis proxy) + Stackdriver Logging configuration (log axis).

### Why Cloud Tasks now?

1. **Architectural parity with SQS on AWS.** Cloud Tasks is
   the GCP equivalent of SQS — both serve guaranteed delivery
   with retry semantics on HTTP failures. After slice 5, the
   AWS + GCP event source coverage is symmetric.
2. **The retry config gap.** A Cloud Tasks queue without
   `maxAttempts` is the GCP equivalent of an SQS queue
   without a redrive policy. Same operational failure mode:
   silent task loss on HTTP target failure.
3. **The scanner pattern is established.** Slice 1 SNS + slice
   4 SQS established the AWS pattern. GCP's existing Pub/Sub
   scanner from slice 1 (v0.89.101) provides the GCP-side
   SDK + signing pattern.
4. **Operator urgency.** Cloud Tasks queues for production
   webhook delivery are common; queues without `maxAttempts`
   set are the canonical "we silently dropped a payment
   notification" production failure.

### What slice 5 does NOT address

- **Non-GCP event source widening** — slices 6-7 add Azure +
  OCI surfaces.
- **GCP Eventarc** — the newer event bus pattern similar to
  EventBridge. Slice 8+ candidate when adoption justifies.
- **Per-task message body inspection.** Squadron reads queue
  metadata only.
- **Per-task execution-time correlation.** The substrate's
  three diagnostics (cold-start, sampling, error rate) cover
  serverless. Cloud Tasks are queue infrastructure; per-task
  HTTP delivery latency is slice 8+ candidate.

## 2. Non-goals (slice 5)

- **GCP Eventarc** — newer event bus pattern. Slice 8+.
- **Azure Event Grid / Event Hubs / OCI Notification Service**
  — slices 6-7.
- **Per-task message body inspection.** Slice 8+.
- **Per-queue depth alerting.** Slice 8+ may add anomaly
  detection on task creation / completion rates using the
  MetricQuerier substrate.
- **Per-queue IAM policy inspection** for fine-grained
  permissions. Slice 8+.
- **Cloud Tasks App Engine targets** — older GCP pattern.
  Slice 5 supports both HTTP and App Engine targets; the
  retry config detection works uniformly. No per-target-type
  recommendation kinds in slice 5.
- **Auto-fix.** Squadron remains a recommender.

## 3. Detection surface — GCP Cloud Tasks

API: `cloudtasks.googleapis.com/v2/projects/*/locations/*/queues`
(List + Get). Required GCP permissions:
`cloudtasks.queues.list`, `cloudtasks.queues.get`.

Detection axes:

| Axis                          | Source                                                                  | Recommendation kind                |
|-------------------------------|-------------------------------------------------------------------------|-------------------------------------|
| Has retry config              | `retryConfig.maxAttempts > 0` (or `-1` for unlimited; slice 5 treats both as configured) | `cloudtasks-retry-policy-enable`    |
| Stackdriver Logging configured | `stackdriverLoggingConfig.samplingRatio > 0` (sampling ratio above zero indicates Logging is on) | `cloudtasks-logging-enable`         |
| Rate limits configured        | `rateLimits.maxDispatchesPerSecond > 0` AND `rateLimits.maxConcurrentDispatches > 0` | informational only                  |
| Queue state                   | `state == "RUNNING"` (PAUSED / DISABLED noted in detail)                | informational only                  |
| Purge time                    | `purgeTime` present (queue has been purged at some point)               | informational only                  |

The trace + log axis mapping for Cloud Tasks mirrors the
slice 4 SQS pattern:

- **Trace axis proxy** = retry config is configured (so failed
  tasks get retried; without it, failures are silent). A queue
  with `maxAttempts > 0` OR `maxAttempts = -1` (unlimited)
  passes the trace axis.
- **Log axis** = Stackdriver Logging is sampling at a positive
  ratio. Cloud Tasks' Stackdriver Logging integration is the
  canonical "is task delivery being audited?" signal —
  parallel to the SNS delivery feedback role / SQS DLQ
  reachability patterns.

The `maxAttempts = 0` edge case: GCP's Cloud Tasks API
returns 0 when retry config is unset OR explicitly set to no
retries. Either way, the failure mode is the same — tasks
get dropped on first failure. Slice 5 treats both as missing
retry config; the recommendation reasoning text explains
that the operator may have intentionally set 0.

## 4. Storage schema

NO migration. The existing `event_source_instance` table from
v0.89.100 has the right shape. Slice 5 adds rows with
`provider = "gcp"` and `surface = "cloudtasks"`.

Schema stays at v15.

## 5. Scanner contract

The existing GCP scanner from slice 1 (v0.89.101) has
`ScanEventSources` returning Pub/Sub topics. Slice 5 extends
the dispatcher to fan out across BOTH surfaces with
partial-scan posture (mirrors the slice 4 AWS pattern):

```go
func (s *Scanner) ScanEventSources(ctx context.Context, scope scanner.ScanScope) ([]scanner.EventSourceInstanceSnapshot, error) {
    var all []scanner.EventSourceInstanceSnapshot
    
    topics, pubsubErr := s.ScanPubSubTopics(ctx, scope)
    if pubsubErr == nil {
        all = append(all, topics...)
    }
    
    queues, ctErr := s.ScanCloudTasksQueues(ctx, scope)
    if ctErr == nil {
        all = append(all, queues...)
    }
    
    // Partial-scan posture: error only when BOTH failed
    if pubsubErr != nil && ctErr != nil {
        return all, fmt.Errorf("all gcp event source surfaces failed: pubsub=%v cloudtasks=%w", pubsubErr, ctErr)
    }
    
    return all, nil
}
```

New file `internal/discovery/gcp/cloudtasks.go` implements
`ScanCloudTasksQueues`.

The Cloud Tasks API:
- `cloudtasks.projects.locations.queues.list` returns paginated
  queues per (project, location)
- Cloud Tasks is regional — queues live per-location.
  Squadron walks the configured location list (or all
  locations with queues if not specified) per the existing
  GCP scanner location-iteration pattern from
  `internal/discovery/gcp/workflows.go` (v0.89.96)
- The Go SDK package
  `cloud.google.com/go/cloudtasks/apiv2` exposes the
  `Queue` resource shape including `RetryConfig`,
  `StackdriverLoggingConfig`, `RateLimits`, `State`,
  `PurgeTime`. Add to go.mod.

## 6. API surface

Existing per-provider scan + inventory endpoints handle the
event_sources field generically. Slice 5 populates more rows
under provider=gcp, surface=cloudtasks.

Discovery summary endpoint's `event_source_count` for GCP
starts increasing by the queue count.

## 7. UI

The DiscoveryGCP Event sources sub-tab from slice 1 renders
rows generically by Surface field. Slice 5 Cloud Tasks rows
render under Surface = "cloudtasks". No UI changes.

## 8. Recommendation kinds

2 new kinds:

```
cloudtasks-retry-policy-enable
cloudtasks-logging-enable
```

The `cloudtasks-` prefix is NEW. Webhook routing extends:

```
cloudtasks-       → gcp
```

The 1 new prefix extends the existing kind-prefix switch in
`internal/api/handlers/iac_github_webhook.go`.

Reasoning template for `cloudtasks-retry-policy-enable`:

> "This Cloud Tasks queue has `maxAttempts = 0` (or
> equivalently, retry config unset). When a task's HTTP
> target returns a non-2xx response, the task is dropped
> SILENTLY after the first attempt — no retry, no
> dead-letter queue, no operator-visible audit signal.
> Equivalent to an SQS queue without a redrive policy.
>
> This Terraform PR configures `retryConfig.maxAttempts = 5`
> (operator tunes) on the queue. If your team intentionally
> wants no-retry semantics (single-attempt-or-drop), decline
> the recommendation; the verdict learning loop records."

Reasoning template for `cloudtasks-logging-enable`:

> "This Cloud Tasks queue has `stackdriverLoggingConfig.samplingRatio`
> at 0 (or unset). Without Stackdriver Logging, the operator
> has no per-task delivery audit trail — successful and
> failed dispatches both flow into the void.
>
> This Terraform PR configures
> `stackdriverLoggingConfig.samplingRatio = 1.0` (full
> sampling). For high-throughput queues where full sampling
> is expensive, operators can tune the ratio."

Terraform pattern for `cloudtasks-retry-policy-enable`:

```hcl
resource "google_cloud_tasks_queue" "<name>" {
  # ... existing fields ...
  
  retry_config {
    max_attempts       = 5    # operator tunes
    min_backoff        = "10s"
    max_backoff        = "300s"
    max_retry_duration = "0s"  # unlimited retry duration; bounded by max_attempts
    max_doublings      = 5
  }
}
```

Terraform pattern for `cloudtasks-logging-enable`:

```hcl
resource "google_cloud_tasks_queue" "<name>" {
  # ... existing fields ...
  
  stackdriver_logging_config {
    sampling_ratio = 1.0  # full sampling; operator tunes for high-throughput
  }
}
```

## 9. Slice 5 contract

**In:**

1. GCP `ScanCloudTasksQueues` implementation populating
   `event_source_instance` with surface=cloudtasks.
2. GCP `ScanEventSources` dispatcher extension to fan out
   across Pub/Sub + Cloud Tasks with partial-scan posture.
3. GCP IAM template extension:
   `cloudtasks.queues.list`, `cloudtasks.queues.get`.
4. 2 new recommendation kinds:
   `cloudtasks-retry-policy-enable` +
   `cloudtasks-logging-enable`.
5. Webhook routing extends with `cloudtasks-` → gcp.
6. iacpicker emitters for both Terraform patterns.
7. Operator runbook section (extend
   docs/event-source-tier-operator-guide.md).
8. README index entry updated to mention Cloud Tasks
   coverage.
9. Acceptance tests covering Cloud Tasks detection, both
   axes, two-way dispatcher partial-scan posture (both
   directions), cold-start parity.

**Out:**

- Azure Event Grid / Event Hubs / OCI Notification Service
  (slices 6-7).
- GCP Eventarc (slice 8+).
- Per-task message body inspection.
- Per-queue depth alerting.
- Per-queue IAM policy inspection.
- Auto-fix.

## 10. Implementation chunks

- **Chunk 1: GCP Cloud Tasks scanner + dispatcher extension.**
  ~600-800 lines. **v0.89.144.**
- **Chunk 2: Proposer prompt + iacpicker + webhook routing +
  runbook update + README index.** ~600-800 lines.
  **v0.89.145.**

Total: 2 release tags. Same pattern as slices 3 + 4.

## 11. Acceptance tests

1. **GCP ScanCloudTasksQueues returns queues** — paginated
   list response across locations is walked.
2. **Queue with maxAttempts > 0 → HasTraceAxis = true**.
3. **Queue with maxAttempts = -1 (unlimited) → HasTraceAxis = true**.
4. **Queue with maxAttempts = 0 → HasTraceAxis = false**.
5. **Queue with stackdriverLoggingConfig.samplingRatio > 0 →
   HasLogAxis = true**.
6. **Queue with stackdriverLoggingConfig.samplingRatio = 0 →
   HasLogAxis = false**.
7. **Queue without stackdriverLoggingConfig at all →
   HasLogAxis = false**.
8. **Queue state PAUSED → snapshot Detail records state**.
9. **Queue with purgeTime set → snapshot Detail records
   purgeTime**.
10. **Two-way ScanEventSources dispatcher returns
    Pub/Sub topics + Cloud Tasks queues**.
11. **Two-way dispatcher partial-scan: Pub/Sub fails →
    Cloud Tasks still surfaces**.
12. **Two-way dispatcher partial-scan: Cloud Tasks fails →
    Pub/Sub still surfaces**.
13. **Two-way dispatcher: both fail → error mentions both
    surfaces**.
14. **Webhook routes cloudtasks-retry-policy-enable to gcp**.
15. **Webhook routes cloudtasks-logging-enable to gcp**.
16. **Discovery summary GCP event_source_count surfaces
    non-zero when queues exist**.
17. **Cold-start parity preserved** — proposer prompts
    byte-identical to v0.89.142 when no Cloud Tasks rows
    trigger recommendations.

## 12. Threat model

**Wider GCP IAM permissions.** Slice 5 adds
`cloudtasks.queues.list` and `cloudtasks.queues.get` to the
GCP scanner Service Account policy. Both read-only.
Operators get the in-product policy upgrade path (#590).

**Cloud Tasks API rate limits.** Cloud Tasks has a per-project
queue list rate limit (~10 RPS for listQueues, ~100 RPS for
getQueue). The substrate's existing GCP rate limiter
absorbs.

**Cost surface.** Cloud Tasks API queries are free for read
operations. No new operator-facing cost decisions.

**Two-way dispatcher partial-scan posture.** When Pub/Sub
fails (IAM lag from connections that predate v0.89.46) AND
Cloud Tasks succeeds (slice 5 IAM update applied), the
operator still sees Cloud Tasks queues. Same in the other
direction. Pinned by tests 11 + 12.

**False positives on no-retry-intentional queues.** A queue
intentionally configured with `maxAttempts = 0` for
fire-and-forget semantics fires the recommendation. The
exclusion table + verdict learning loop handle. Runbook
documents.

**No span content logging.** Slice 5 reads queue metadata
only. Task payloads stay invisible to Squadron. PII surface
stays at zero.

## 13. Slice 6+ candidates

- **Azure Event Grid** — second Azure event source surface.
  Slice 6.
- **Azure Event Hubs** — third Azure surface. Slice 6 or 7.
- **OCI Notification Service** — second OCI surface. Slice 7.
- **GCP Eventarc** — newer event bus pattern (slice 8+).
- **Per-queue depth anomaly detection** — slice 8+ may use
  the MetricQuerier substrate to baseline `task_creation_count`
  vs `task_completion_count` for anomaly detection.
- **Per-queue execution-time analysis** — substrate diagnostic
  extension to Cloud Tasks targets.
- **Multi-project Cloud Tasks fan-out coordination** — when
  an operator runs Cloud Tasks across multiple projects.

---

**Strategic frame:**

Slice 5 brings GCP into architectural parity with AWS on the
event source tier:

> "Squadron covers SIX event source surfaces across four
> clouds:
> - AWS: EventBridge + SNS + SQS (3 surfaces)
> - GCP: Pub/Sub + Cloud Tasks (2 surfaces)
> - Azure: Service Bus (1 surface — slices 6 + 7 add Event
>   Grid + Event Hubs)
> - OCI: Streaming (1 surface — slice 7 adds Notification
>   Service)"

After slice 5, the GCP queue-based failure chain is fully
visible:

1. **Pub/Sub topic** without delivery integration (slice 1
   `pubsub-trace-enable`) — operator can't see fan-out
2. **Cloud Tasks queue** without retry config (this slice
   `cloudtasks-retry-policy-enable`) — failed HTTP delivery
   vanishes silently
3. **Cloud Tasks queue** without Stackdriver Logging (this
   slice `cloudtasks-logging-enable`) — no per-task audit
4. **HTTP target / Cloud Run / Cloud Functions** without
   trace primitive (serverless tier) — even if delivery
   succeeds, the consumer doesn't emit
5. **Cloud Run / Cloud Functions cold-start regression**
   (substrate's three diagnostics) — workload-health view

Five layers. One control plane.

The Tuesday LinkedIn drumbeat narrative gains: "Your
production webhook delivery queue has `maxAttempts = 0` —
on a 5xx from the downstream, the task is dropped on first
attempt. Customer-facing webhooks vanish silently. Squadron
just drafted the PR to add retry config with maxAttempts =
5 and exponential backoff."
