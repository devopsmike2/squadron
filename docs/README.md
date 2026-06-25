# Squadron documentation

Welcome to the Squadron docs. Squadron is an open-source control plane for
OpenTelemetry fleets — agent management over OpAMP, a built-in telemetry
backend, safe staged rollouts, and an operator UI, all in a single self-hosted
binary.

If you're new, start with [Getting started](./getting-started.md). If you
already have Squadron running and want to understand a specific subsystem,
jump straight to that page.

## Table of contents

- [Getting started](./getting-started.md) — install Squadron, connect your
  first collector, push your first config.
- [Deployment guide](./deployment.md) — the four supported deployment
  shapes (single VM, Docker Compose, Kubernetes, OpenShift), the
  required and optional components, and the production checklist.
- [Concepts](./concepts.md) — agents, groups, configs, and the drift model.
- [Rollouts](./rollouts.md) — safe staged deploys with canary selection,
  auto-abort criteria, preview/diff, and the recipe + template cookbook.
- [Action runner steps in plans](./action-runner-steps-in-plans.md) —
  v0.89.14 operator runbook for embedding signed runner verbs (restart
  a service, rotate a secret, drain a pool member) as steps inside a
  multi-step plan, with shared approval and audit.
- [Proposer learning loop](./proposer-learning-loop.md) — v0.89.17 +
  v0.89.18 operator runbook for the per-group feedback loop that
  feeds prior approved/rejected AI proposals back into the next
  proposal as in-context few-shot examples. Covers the per-group
  toggle, the selection policy, the audit field, and the worked
  example.
- [Discovery proposer feedback loop](./discovery-proposer-learning.md) —
  v0.89.28 operator runbook for the discovery-side feedback loop
  (#643 slice 1) that reads `recommendation.pr_merged` events and
  stops the proposer from re-proposing recommendations the
  operator has already merged. Covers the per-connection flag,
  the connection × account × region scope tuple, the new
  `discovery_proposal.created` audit event, the branch-name
  backward-compat note, and the worked example.
- [GCP discovery — first-time setup](./discovery-gcp-first-time-setup.md) —
  v0.89.45 through v0.89.49 operator runbook for the GCP arc
  (design at [proposals/gcp-discovery-slice1.md](./proposals/gcp-discovery-slice1.md)).
  First non-AWS discovery arc. Adds GCP Compute Engine scanning
  via Service Account JSON credentials sealed via credstore.
  Mirrors AWS slice 1's wizard / inventory / recommendations
  structure at `/discovery/gcp`. Same proposer feedback loop,
  same Checks API integration, same Don't propose this again
  affordance — just on a different cloud. **Slice 1 SHIPPED in
  v0.89.49.** Squadron's positioning shifts to "the universal
  observability control plane that scans your AWS AND GCP
  fleets."
- [Azure discovery — first-time setup](./discovery-azure-first-time-setup.md) —
  v0.89.50 through v0.89.54 operator runbook for the Azure arc
  (design at [proposals/azure-discovery-slice1.md](./proposals/azure-discovery-slice1.md)).
  Second non-AWS discovery arc. Adds Azure Virtual Machines
  scanning via Service Principal client_secret credentials
  sealed via credstore. Mirrors AWS and GCP slice 1's wizard /
  inventory / recommendations structure at `/discovery/azure`.
  Same proposer feedback loop, same Checks API integration,
  same Don't propose this again affordance. **Slice 1 SHIPPED
  in v0.89.54.** Squadron's positioning is now "the universal
  observability control plane that scans AWS, GCP, AND Azure
  fleets" — the three-cloud claim is concretely defensible.
- [OCI (Oracle Cloud) discovery — first-time setup](./discovery-oci-first-time-setup.md) —
  v0.89.55 through v0.89.59 operator runbook for the OCI arc
  (design at [proposals/oci-discovery-slice1.md](./proposals/oci-discovery-slice1.md)).
  Third non-AWS discovery arc. Adds Oracle Cloud Compute
  Instance scanning via API signing key credentials (RSA
  private key sealed via credstore). Mirrors the AWS / GCP /
  Azure slice 1 wizard / inventory / recommendations structure
  at `/discovery/oci`. Same proposer feedback loop, same Checks
  API integration, same Don't propose this again affordance.
  **Slice 1 SHIPPED in v0.89.59.** Squadron now covers 4
  clouds — the strongest universal observability claim a
  single OSS control plane can defensibly support: "scans
  AWS, GCP, Azure, AND Oracle Cloud fleets."
- [Unified Discovery dashboard](./proposals/unified-discovery-dashboard-slice1.md) —
  v0.89.60 through v0.89.62 design + delivery for the
  cross-cloud aggregate view at `/discovery`. Aggregates
  connection / instance / coverage counts + the 10 most
  recent recommendations across all four clouds (AWS, GCP,
  Azure, OCI) into a single landing screen, so an operator
  with multi-cloud fleets sees Squadron's universal-
  observability claim in one screen instead of after four
  clicks. Backend aggregation endpoint at
  `GET /api/v1/discovery/summary` (30s in-memory cache);
  frontend page at `/discovery` with a coverage ring +
  four-card responsive grid + recent recommendations table.
  **Slice 1 SHIPPED in v0.89.62.** The four-cloud claim is
  now operator-visible in one glance; per-provider pages
  remain for wizards / deep-dive surfaces.
- [GitHub webhook listener](./webhook-listener.md) — v0.89.23 +
  v0.89.24 operator runbook for the PR-merged webhook that closes
  the recommendation lifecycle in audit. Covers generating the
  secret, configuring the GitHub repo webhook, verifying the
  loop end-to-end, reading the audit signal, and the
  troubleshooting matrix.
- [Event source tier — operator guide](./event-source-tier-operator-guide.md) —
  v0.89.99 through v0.89.107 operator runbook for the event
  source tier arc (slice 1 design at
  [proposals/event-source-tier-slice1.md](./proposals/event-source-tier-slice1.md),
  slice 2 design at
  [proposals/event-source-tier-slice2.md](./proposals/event-source-tier-slice2.md)).
  Sixth tier alongside compute / database / kubernetes /
  serverless / orchestration. Four surfaces across four
  clouds: AWS EventBridge, GCP Pub/Sub, Azure Service Bus,
  OCI Streaming. **Slice 1 (v0.89.99-v0.89.103)** ships
  per-cloud detection of trace axis + log axis primitives
  at the event source level; 7 recommendation kinds
  (`eventbridge-{xray-enable,schemas-discover,logging-enable}`,
  `pubsub-{trace-enable,schema-attach}`,
  `servicebus-diagnostics-enable`,
  `streaming-logging-enable`); Discovery summary +
  trace_coverage endpoints gain `event_source_count` and
  `event_source_pct`; dashboard TRACE COVERAGE chip
  breakdown adds EVT column. **Slice 2
  (v0.89.104-v0.89.107)** ships per-message propagation
  detection — does the source's CONFIG preserve trace
  context end-to-end? 5 new recommendation kinds reusing
  the slice 1 webhook prefixes:
  `eventbridge-rule-preserves-trace`,
  `pubsub-{schema-includes-traceparent,subscription-preserves-attrs}`,
  `servicebus-policy-preserves-traceparent`,
  `streaming-config-preserves-headers`. Event sources
  sub-tab gains a Propagation column + notes side panel
  on all four provider pages. Trace coverage endpoint
  gains `propagation_pct`; dashboard EVT chip gains a
  `(prop N%)` suffix when event sources exist. **Slice 2
  SHIPPED in v0.89.107.** **Slice 3 (v0.89.137-v0.89.139)**
  widens AWS event source coverage by adding SNS as a
  second AWS surface alongside EventBridge. 2 new
  recommendation kinds: sns-subscriptions-attach
  (audit-only — fires on orphan topics with zero confirmed
  subscriptions) + sns-delivery-logging-enable (Terraform:
  per-protocol IAM delivery feedback role attachment).
  1 new webhook prefix: sns- → aws. ScanEventSources
  dispatcher extends with partial-scan posture (EventBridge
  failure no longer blocks SNS surfacing, and vice versa).
  **Slice 3 SHIPPED in v0.89.139.**
  **Slice 4 (v0.89.140-v0.89.142)** continues the widening
  pass by adding AWS SQS as the third AWS event source
  surface. Completes the canonical AWS pub/sub fan-out
  architecture: EventBridge | SNS → SQS → consumer. 2 new
  recommendation kinds: sqs-redrive-policy-enable (Terraform:
  DLQ + redrive policy targeting it) catches the single most
  common AWS messaging production failure; +
  sqs-deadletter-queue-attach (audit-only — fires on queues
  with dangling DLQ ARN references). 1 new webhook prefix:
  sqs- → aws. ScanEventSources dispatcher extends from
  two-way (EB+SNS) to three-way (EB+SNS+SQS) with partial-scan
  posture across all three. **Slice 4 SHIPPED in v0.89.142.**
  **Slice 5 (v0.89.143-v0.89.145)** continues the widening pass
  by adding GCP Cloud Tasks as the second GCP event source
  surface. Architectural parity with the slice 4 AWS SQS
  pattern — both serve guaranteed delivery with retry
  semantics. 2 new recommendation kinds:
  cloudtasks-retry-policy-enable (Terraform: retry_config block
  with exponential backoff) catches the canonical Cloud Tasks
  production failure (silent task drop on HTTP target failure); +
  cloudtasks-logging-enable (Terraform: stackdriver_logging_config
  with sampling_ratio = 1.0). 1 new webhook prefix: cloudtasks-
  → gcp. ScanEventSources dispatcher extends from one-way
  (Pub/Sub only) to two-way (Pub/Sub + Cloud Tasks) with
  partial-scan posture both directions. **Slice 5 SHIPPED in
  v0.89.145.** AWS now has 3 event source surfaces (EventBridge
  + SNS + SQS); GCP now has 2 (Pub/Sub + Cloud Tasks); slices
  6-7 will catch up Azure + OCI.
  **Slice 6 (v0.89.146-v0.89.148)** continues the widening pass
  by adding Azure Event Grid as the second Azure event source
  surface. Event Grid is Azure's fan-out distribution layer for
  cloud events (CloudEvents 1.0 schema). 2 new recommendation
  kinds: eventgrid-diagnostics-enable (Terraform:
  azurerm_monitor_diagnostic_setting with 4 Event Grid log
  categories) mirrors the Service Bus pattern; +
  eventgrid-cloudevent-schema-enforce (Terraform: input_schema
  = "CloudEventSchemaV1_0") is a BREAKING CHANGE for existing
  subscribers — the reasoning text emphasizes coordination
  before merging. 1 new webhook prefix: eventgrid- → azure.
  ScanEventSources dispatcher extends from one-way (Service Bus
  only) to two-way (Service Bus + Event Grid) with partial-scan
  posture both directions. NO IAM extension (existing Reader
  role covers). **Slice 6 SHIPPED in v0.89.148.** AWS: 3 event
  source surfaces; GCP: 2; Azure: 2; OCI: 1 — slice 7 closes
  the widening pass at 3-2-2-2.
  **Slice 7 (v0.89.149-v0.89.151)** closes the cross-cloud
  event source widening pass by adding OCI Notification
  Service (ONS) as the second OCI event source surface
  alongside Streaming. ONS serves the pub/sub fan-out pattern
  — the analog of AWS SNS + GCP Pub/Sub on the alert
  distribution side. 1 new recommendation kind:
  `ons-logging-enable` (Terraform: `oci_logging_log` routing
  topic delivery events to a log group, parameterized via
  `var.default_log_group_id`) mirrors the slice 1 Streaming
  `streaming-logging-enable` pattern via a shared OCI Logging
  `/logs` detection helper. 1 new webhook prefix: `ons-` → oci.
  `ScanEventSources` dispatcher extends from one-way (Streaming
  only) to two-way (Streaming + Notifications) with
  partial-scan posture both directions. IAM extension:
  `read ons-topics in compartment` added to the OCI scanner
  policy template; existing slice 1 Logging read policy covers
  the per-topic detection call. **Slice 7 SHIPPED in
  v0.89.151.** The cross-cloud widening pass closes at
  **3-2-2-2 / 9 surfaces across 4 clouds** — AWS 3
  (EventBridge + SNS + SQS), GCP 2 (Pub/Sub + Cloud Tasks),
  Azure 2 (Service Bus + Event Grid), OCI 2 (Streaming +
  Notification Service).
  **Slice 8 (v0.89.152-v0.89.154)** brings Azure to parity
  with AWS on the event source tier by adding Event Hubs as
  the third Azure surface alongside Service Bus and Event
  Grid. Event Hubs is Azure's big-data event ingestion
  primitive — a partitioned log analogous to Kafka,
  distinct from the messaging primitives. 2 new
  recommendation kinds: `eventhubs-diagnostics-enable`
  (Terraform: `azurerm_monitor_diagnostic_setting` with the
  5 Event Hubs log categories — ArchiveLogs,
  OperationalLogs, AutoScaleLogs, KafkaCoordinatorLogs,
  KafkaUserErrorLogs) mirrors the Service Bus + Event Grid
  diagnostic settings pattern; +
  `eventhubs-capture-enable` (Terraform: `azurerm_eventhub`
  with `capture_description` block enabling Capture on ONE
  hub) is operator-prescriptive — the operator picks WHICH
  hub to enable Capture on during PR review based on
  durability-critical streams. 1 new webhook prefix:
  `eventhubs-` → azure. `ScanEventSources` dispatcher
  extends from two-way (Service Bus + Event Grid) to
  three-way (Service Bus + Event Grid + Event Hubs) with
  combinatorial partial-scan posture mirroring the slice 4
  AWS three-way pattern. NO IAM extension beyond what slice
  1 + slice 6 already covered. **Slice 8 SHIPPED in
  v0.89.154.** Cross-cloud count after slice 8: **3-2-3-2 /
  10 surfaces across 4 clouds** — Azure now matches AWS at
  3 surfaces.
  **Slice 9 (v0.89.155-v0.89.157)** brings OCI to parity
  with AWS + Azure on the event source tier by adding Queue
  Service as the third OCI surface alongside Streaming and
  Notification Service. OCI Queue Service is the
  transactional FIFO message queue primitive analogous to
  AWS SQS — distinct from ONS pub/sub fan-out (one consumer
  per message vs. many-consumer fan-out) and from Streaming
  partitioned log analytics intake. 1 new recommendation
  kind: `queues-logging-enable` (Terraform: `oci_logging_log`
  routing queue delivery events to a log group via
  `var.default_log_group_id`) mirrors the slice 1 Streaming
  `streaming-logging-enable` and slice 7 ONS
  `ons-logging-enable` patterns through the same OCI Logging
  `/logs` detection helper structure. 1 new webhook prefix:
  `queues-` → oci. `ScanEventSources` dispatcher extends
  from two-way (Streaming + Notifications) to three-way
  (Streaming + Notifications + Queues) with combinatorial
  partial-scan posture mirroring the slice 8 Azure three-way
  pattern. IAM extension: `read queues in compartment` added
  to the OCI scanner policy template; existing slice 1
  Logging read policy covers the per-queue detection call.
  **Slice 9 SHIPPED in v0.89.157.** Cross-cloud count after
  slice 9: **3-2-3-3 / 11 surfaces across 4 clouds** — only
  GCP at 2 surfaces remains for slice 10+ to close the
  widening pass at 3-3-3-3 / 12 surfaces.
  **Slice 10 (v0.89.158-v0.89.160) CLOSES THE
  CROSS-CLOUD EVENT SOURCE WIDENING PASS** by adding GCP
  Pub/Sub Lite as the third GCP surface alongside Pub/Sub
  and Cloud Tasks. Pub/Sub Lite is GCP's partitioned-log
  primitive — the structural analog of AWS Kinesis Data
  Streams and Azure Event Hubs. Distinct from full Pub/Sub
  in that Lite trades managed routing + global delivery for
  cost efficiency at high volume — operators self-manage
  partition capacity via reservations. Zone-pinned by
  design. 2 new recommendation kinds:
  `pubsublite-logging-enable` (Terraform:
  `google_logging_project_sink` filtering on the topic's ID
  with destination defaulting to a BigQuery dataset) mirrors
  the slice 1 Pub/Sub pattern via the Cloud Logging sink
  primitive; + `pubsublite-reservation-attach` (Terraform:
  creates a NEW `google_pubsub_lite_reservation` resource
  AND updates the topic's `reservation_config` reference)
  is the FIRST event source tier recommendation that
  creates a billable resource — reasoning text emphasizes
  the cost implication so PR reviewers see it explicitly;
  default sizing is conservative (4 publish + subscribe
  units) but operators MUST validate against ACTUAL peak
  throughput before merging. 1 new webhook prefix:
  `pubsublite-` → gcp. `ScanEventSources` dispatcher
  extends from two-way (Pub/Sub + Cloud Tasks) to three-way
  (Pub/Sub + Cloud Tasks + Pub/Sub Lite) with combinatorial
  partial-scan posture; zone-pinned per-zone partial-scan
  handling inside `ScanPubSubLiteTopics`. IAM extension:
  `pubsublite.topics.list` + `pubsublite.topics.get` +
  `pubsublite.reservations.list` added to the GCP scanner
  role. **Slice 10 SHIPPED in v0.89.160. THE CROSS-CLOUD
  EVENT SOURCE WIDENING PASS IS COMPLETE** at **3-3-3-3 /
  12 surfaces across 4 clouds** — AWS 3 (EventBridge + SNS
  + SQS), GCP 3 (Pub/Sub + Cloud Tasks + Pub/Sub Lite),
  Azure 3 (Service Bus + Event Grid + Event Hubs), OCI 3
  (Streaming + Notification Service + Queue Service). Every
  cloud carries every primitive pattern (queue, pub/sub
  fan-out, partitioned-log intake). Future event source
  work shifts from per-cloud breadth (3-3-3-3 complete) to
  per-axis depth — consumer lag detection, cross-surface
  correlation, substrate-level cost modeling.

  **DLQ Configuration Analysis slice 1 (v0.89.162-v0.89.166)
  ships the FIRST per-axis depth slice** across all 4 clouds'
  queue tier surfaces. Two detection rules per cloud (missing
  DLQ + retry-count band `[2, 50]`), 7 recommendation kinds
  routed via existing per-cloud webhook prefixes (NO new
  prefix routing). Establishes two HONEST FRAMING patterns:
  §3.1 managed-primitive-absence (GCP Cloud Tasks has no
  managed DLQ primitive) + §3.2 scanner-coverage-gap (Azure
  Service Bus per-queue walk deferred to future slice). Both
  patterns are load-bearing for slice 12+ substrate-dependent
  depth work where Squadron repeatedly hits detection rules
  it cannot prove from its current scan view. NO new API
  calls, NO IAM extension, NO storage migration; additive
  Detail bag keys only preserve cold-start parity. Design
  doc at
  [proposals/dlq-configuration-analysis-slice1.md](./proposals/dlq-configuration-analysis-slice1.md);
  runbook close in
  [event-source-tier-operator-guide.md](./event-source-tier-operator-guide.md).

  **Consumer Lag Detection slice 2 (v0.89.167-v0.89.171)
  ships the SECOND per-axis depth slice** with identical
  shape to DLQ slice 1 (4 chunks + design doc). Two
  detection rules: backlog depth ≥ 1000 + consumer
  silence ≥ 300s (combined signal — both together is the
  firing condition). 6 recommendation kinds across all 4
  clouds; AWS SQS reads
  `ApproximateNumberOfMessages` +
  `ApproximateAgeOfOldestMessage` from the existing
  GetQueueAttributes response, OCI Queue Service reads
  `runtimeMetadata.visibleMessages` +
  `runtimeMetadata.timeStateLastChanged` from the existing
  queue list response, GCP Cloud Tasks reuses §3.1 honest
  framing, Azure Service Bus reuses §3.2 inherited scanner-
  coverage-gap. The honest-framing patterns are now applied
  ACROSS two per-axis-depth slices — the per-axis-depth
  horizon's predictable shape is established: AWS + OCI
  ship real detection; GCP + Azure ship honest framing.
  All recommendation kinds route via existing per-cloud
  webhook prefixes — NO new prefix routing. NO new API
  calls, NO IAM extension, NO storage migration; additive
  Detail bag keys only preserve cold-start parity. Design
  doc at
  [proposals/consumer-lag-detection-slice2.md](./proposals/consumer-lag-detection-slice2.md);
  runbook close in
  [event-source-tier-operator-guide.md](./event-source-tier-operator-guide.md).

  **Poison-Message Rate Analysis slice 3 (v0.89.172-v0.89.176)
  ships the THIRD per-axis depth slice** with the identical
  4-chunk + design-doc shape (CLOSES at v0.89.176 with the
  OCI Queue Service chunk). Poison-message RATE is the
  leading-indicator axis between DLQ presence (slice 1,
  structural) and consumer lag (slice 2, temporal): a
  spiking rate signals schema drift, downstream outages, or
  a code regression on one message shape before messages
  reach the DLQ. Two additive Detail keys per surface
  (`poison_rate_per_hour` + `poison_rate_high_band`); 4
  recommendation kinds (`sqs-poison-rate-monitor-add`,
  `cloudtasks-poison-rate-monitor-add`,
  `servicebus-poison-rate-monitor-add`,
  `queues-poison-rate-monitor-add`). Unlike slices 1 + 2
  (AWS + OCI real detection, GCP + Azure honest framing),
  slice 3 is UNIFORM: all four clouds ship §3.3
  substrate-metric-dependence honest framing because every
  per-queue poison rate needs a time-series metric delta the
  single-pass scanner does not query. §3.3 is therefore the
  cleanest deferral to close — a future substrate
  MetricQuerier slice retires all four clouds at once
  (recommended next arc, mirroring the cold-start latency
  slice 1 -> slice 2 MetricQuerier build). All kinds route
  via existing per-cloud webhook prefixes — NO new prefix
  routing. NO new API calls, NO IAM extension, NO storage
  migration; additive Detail bag keys only preserve
  cold-start parity. Design doc at
  [proposals/poison-message-rate-slice3.md](./proposals/poison-message-rate-slice3.md);
  runbook close in
  [event-source-tier-operator-guide.md](./event-source-tier-operator-guide.md).

  **Poison-Rate Substrate Integration slice 4 (v0.89.177+)
  closes the §3.3 deferrals that slice 3 shipped as honest
  framing.** Slice 3 left every cloud's poison-rate axis at
  the absent sentinel (`poison_rate_per_hour = -1`); slice 4
  builds the per-cloud MetricQuerier integration that actually
  reads the metric, one cloud per chunk, mirroring the
  cold-start latency arc's per-cloud substrate build. **Chunk
  1 (v0.89.177) makes AWS SQS real:** Squadron reads the DLQ's
  `NumberOfMessagesSent` SUM over a trailing 1-hour window via
  CloudWatch `GetMetricStatistics` (reusing the cold-start
  substrate's rate limiter + throttle-retry) and overwrites
  the two poison-rate Detail keys with the measured rate for
  every source queue whose DLQ is reachable in-account.
  Real-zero (`0`) is now distinguished from absent (`-1`, which
  strictly means "not measured"). GCP Cloud Tasks, Azure
  Service Bus, and OCI Queue Service stay on §3.3 honest
  framing until chunks 4.2 / 4.3 / 4.4 land. NO new IAM
  (`cloudwatch:GetMetricStatistics` is namespace-agnostic and
  already granted), NO new webhook prefix; the enrichment
  overwrites existing Detail keys and is a no-op when
  CloudWatch is unwired, preserving cold-start parity. Design
  doc at
  [proposals/poison-rate-substrate-slice4.md](./proposals/poison-rate-substrate-slice4.md);
  runbook close in
  [event-source-tier-operator-guide.md](./event-source-tier-operator-guide.md).

  **Chunk 2 (v0.89.178) makes GCP Cloud Tasks real:** Squadron
  reads each queue's FAILED `task_attempt_count`
  (`response_code != "OK"`) SUM over a trailing 1-hour window
  via Cloud Monitoring `timeSeries.list` and overwrites the two
  poison-rate Detail keys with the measured failed-attempt
  rate. Cloud Tasks has no DLQ primitive, so the rate is
  measured on the queue itself (no reachability gate, unlike
  AWS SQS). Same real-zero (`0`) versus absent (`-1`) contract.
  Azure Service Bus and OCI Queue Service stay on §3.3 honest
  framing until chunks 4.3 / 4.4 land. NO new IAM
  (`monitoring.timeSeries.list` already granted), NO new
  webhook prefix; the enrichment is a no-op when Cloud
  Monitoring is unwired, preserving cold-start parity. Same
  design doc
  ([proposals/poison-rate-substrate-slice4.md](./proposals/poison-rate-substrate-slice4.md)).

  **Chunk 3a (v0.89.179) makes Azure Service Bus real at
  namespace granularity:** Squadron reads each namespace's
  `DeadletteredMessages` gauge via Azure Monitor and derives
  the poison rate as the `max(Maximum) - min(Minimum)` delta
  (net dead-letter accumulation, not standing backlog) over a
  trailing 1-hour window. This closes §3.3 (real metric) but
  NOT §3.2 (scanner-coverage-gap): the reading is
  namespace-aggregated across all queues/topics. Per-queue
  attribution (the `EntityName`-dimension per-queue walk) is
  split into **chunk 3b** — real metric now, per-queue
  attribution next, so neither release is stretched across both
  a metric path and a scanner extension. Same real-zero (`0`)
  versus absent (`-1`) contract. OCI Queue Service stays on
  §3.3 until chunk 4.4. NO new IAM (`microsoft.insights` metrics
  already granted), NO new webhook prefix; the enrichment is a
  no-op without an access token, preserving cold-start parity.
  Same design doc
  ([proposals/poison-rate-substrate-slice4.md](./proposals/poison-rate-substrate-slice4.md)).

  **Chunk 4 (v0.89.181) makes OCI Queue Service real — CLOSING
  the substrate arc.** Squadron reads each queue's dead-letter
  depth gauge (`MessagesInDlq`) via OCI Monitoring
  `summarizeMetricsData` and derives the rate as the `max-min`
  delta (net accumulation) over a trailing 1-hour window (the
  same gauge-delta shape as Azure). With this release every cloud
  reads a real poison-rate metric — §3.3 substrate-metric-
  dependence honest framing is fully retired (AWS DLQ
  `NumberOfMessagesSent` sum, GCP failed `task_attempt_count`
  sum, Azure per-queue `DeadletteredMessages` delta, OCI
  `MessagesInDlq` delta). Honest caveat: the exact OCI metric
  name should be confirmed against OCI's Monitoring reference — a
  mismatch degrades SAFELY to the absent sentinel (`-1`), never
  false data. NO new IAM across the entire arc, NO new webhook
  prefixes; every chunk preserves cold-start parity. Design doc
  ([proposals/poison-rate-substrate-slice4.md](./proposals/poison-rate-substrate-slice4.md)).

  **Consumer-Lag Substrate Integration slice 5 (v0.89.182+)** is a
  parallel substrate arc that closes the consumer-lag slice-2
  honest-framing deferrals (GCP Cloud Tasks §3.1, Azure Service Bus
  §3.2) by reading the backlog metric from the same MetricQuerier
  substrate. **Chunk 1 (v0.89.182) makes the GCP Cloud Tasks backlog
  real:** Squadron reads `cloudtasks.googleapis.com/queue/depth` (a
  gauge) via Cloud Monitoring with the `ALIGN_MAX` aligner — the
  peak backlog over a trailing 1-hour window — and overwrites
  `lag_backlog_depth` + `lag_backlog_depth_high` (threshold 1000,
  matching AWS + OCI). The consumer-silence half stays honest-framed
  (no clean Cloud Tasks oldest-task-age metric). Azure Service Bus
  backlog lands in chunk 2 (`ActiveMessages` per-queue via the
  EntityName split). NO new IAM, NO new webhook prefix; unwired path
  is a byte-identical no-op (cold-start parity). Design doc at
  [proposals/consumer-lag-substrate-slice5.md](./proposals/consumer-lag-substrate-slice5.md).

  **Cost-Correlation Substrate slice 6 (v0.89.183+)** begins the
  cost-correlation work under explicit read-only guardrails.
  **Chunk 1 (v0.89.183) ships the money-touching plumbing in
  isolation** — a read-only `CostQuerier` interface, a `CostResult`
  shape (integer micro-USD money, never float), and a thread-safe
  `CostBudgetGovernor` that caps per-account spend on charged
  cost-reporting APIs at a default $1/30-day-window ceiling and
  rejects calls that would exceed it (`ErrCostBudgetExceeded`,
  treated as a graceful skip). NO per-cloud billing integration in
  this chunk — per-cloud `QueryCost` bodies fan out from the
  substrate in later chunks, AWS Cost Explorer (~$0.01/call, the
  one surface that materially charges) first. The per-call-cost
  surface is documented upfront in the design doc; cost data will
  surface in recommendations as a plain figure with no
  editorializing about whether a number is high or low. Design doc
  at
  [proposals/cost-correlation-substrate-slice6.md](./proposals/cost-correlation-substrate-slice6.md).

  **Chunk 2 (v0.89.184) ships the AWS Cost Explorer `QueryCost`
  body** — the first per-cloud cost reader, and the one surface
  that materially charges (~$0.01/call), so the surface that most
  exercises the governor. It reads `UnblendedCost` for a SERVICE
  dimension via `GetCostAndUsage`, gated through the
  `CostBudgetGovernor`, and refuses to issue a charged call
  without BOTH a wired client and a governor (no charged request
  can escape spend accounting). Over-budget returns
  `ErrCostBudgetExceeded` as a graceful skip. Amounts are parsed
  to integer micro-USD (no float drift). It is NOT wired into any
  scan in this chunk — no charged Cost Explorer request fires
  during a scan until the cost-correlation enrichment chunk
  enables it. Same design doc.

  **Chunk 3 (v0.89.185) ships the AWS SQS cost-correlation
  enrichment** — joins SQS service cost onto DLQ-bearing queue
  snapshots (`service_cost_monthly_micro_usd` + currency + a
  `service_cost_scope="service"` honest label) so a DLQ /
  poison-rate recommendation can carry "Amazon SQS is costing
  ~$X/mo; draining this DLQ reduces wasted spend." Still gated:
  a no-op unless a Cost Explorer client + governor are wired (no
  production wiring by default), at most one charged call per
  scan, and only when a DLQ exists to correlate (zero cost calls
  otherwise). The proposer prompt enforces service-level,
  non-editorializing reporting. Same design doc.

  **Chunk 4 (v0.89.186) adds the Azure Service Bus cost reader** —
  Azure Cost Management `/query` (free per call), governor-gated as
  the opt-in signal, reusing the Azure bearer-token ARM plumbing
  (no new SDK). One read-only POST per scan filtered to the Service
  Bus ServiceName dimension; columns found by name (order-
  independent); micro-USD parsing (no float; non-USD currency
  preserved). Attaches `service_cost_*` to Service Bus namespace
  snapshots, same honest service-level scope as AWS. Cost readers
  now cover AWS SQS + Azure Service Bus; GCP (BigQuery export,
  heavier) and OCI land later. Same design doc.

  **Chunk 3b (v0.89.180) closes §3.2 for Azure — per-queue
  attribution.** The `DeadletteredMessages` metric is split by
  the `EntityName` dimension (one Azure Monitor call,
  `$filter="EntityName eq '*'"`), so `poison_rate_per_hour` now
  carries the WORST-offending queue's rate, plus
  `poison_rate_worst_queue` (the queue name) and
  `poison_rate_measured_queue_count`. Using the metric dimension
  the §3.2 gap named is cleaner than a separate ARM per-queue
  enumeration. Falls back to the chunk-3a namespace-aggregated
  reading when no per-entity series is returned. This completes
  Azure in the substrate arc (§3.3 in 3a, §3.2 in 3b); OCI is the
  last cloud, on §3.3 until chunk 4.4. NO new IAM, NO new webhook
  prefix; unwired path is a byte-identical no-op (cold-start
  parity). Same design doc
  ([proposals/poison-rate-substrate-slice4.md](./proposals/poison-rate-substrate-slice4.md)).

  Squadron's claim
  grows a sixth tier: "scans AWS, GCP, Azure, AND Oracle
  Cloud across COMPUTE, DATABASE, KUBERNETES, SERVERLESS,
  ORCHESTRATION, AND EVENT SOURCES for observability gaps,
  verifies telemetry is actually flowing, validates the
  spans Squadron receives are healthy, AND drafts the IaC
  PRs that close the gaps it finds."
- [Orchestration tier — operator guide](./orchestration-tier-operator-guide.md) —
  v0.89.94 through v0.89.136 operator runbook for the
  orchestration tier arc. Slice 1 (design at
  [proposals/orchestration-tier-slice1.md](./proposals/orchestration-tier-slice1.md))
  shipped AWS Step Functions + GCP Workflows + Azure Logic
  Apps. Slice 2 (design at
  [proposals/orchestration-tier-slice2.md](./proposals/orchestration-tier-slice2.md))
  closes the qualification by adding OCI Resource Manager
  (Stacks + Jobs) as the closest OCI orchestration primitive.
  Honest framing: Resource Manager is infrastructure
  orchestration (Terraform-as-a-service), not workflow
  orchestration like Step Functions / Workflows / Logic Apps;
  OCI Process Automation (BPMN) is the semantically closer
  match but deferred to slice 3. Slice 2 detection:
  compartment-level OCI Logging with service=resourcemanager
  source mapping. 1 new recommendation kind:
  resmgr-logging-enable; 1 new webhook prefix: resmgr- → oci.
  **Slice 1 SHIPPED in v0.89.98. Slice 2 SHIPPED in
  v0.89.136.** Universal claim's orchestration tier is now
  cleanly 4-cloud — no asterisks.
- [Serverless tier — operator guide](./serverless-tier-operator-guide.md) —
  v0.89.89 through v0.89.93 operator runbook for the
  serverless tier slice 1 arc (design at
  [proposals/serverless-tier-slice1.md](./proposals/serverless-tier-slice1.md)).
  Fourth tier alongside compute / database / kubernetes
  across all four clouds. Five surfaces: AWS Lambda, GCP
  Cloud Run, GCP Cloud Functions, Azure Functions, OCI
  Functions. Per-cloud detection of trace axis + OTel distro
  primitives. New Serverless Inventory sub-tab on each
  per-provider Discovery page. 11 new recommendation kinds:
  `lambda-{xray-active,otel-layer,otel-wrapper}`,
  `cloudrun-{trace-enable,otel-sidecar,otel-export-endpoint}`,
  `cloudfunc-{trace-enable,otel-layer}`,
  `azfunc-{appinsights-enable,otel-distro}`,
  `ocifunc-{apm-enable,otel-distro}`. Discovery summary +
  trace_coverage endpoints gain `serverless_count` and
  `serverless_pct`. **Slice 1 SHIPPED in v0.89.93.**
  Squadron's claim grows a fourth tier: "scans AWS, GCP,
  Azure, AND Oracle Cloud across COMPUTE, DATABASE,
  KUBERNETES, AND SERVERLESS for observability gaps,
  verifies telemetry is actually flowing, validates the
  spans Squadron receives are healthy, AND drafts the IaC
  PRs that close the gaps it finds."
- [Workload Health panel — operator guide](./workload-health-panel-operator-guide.md) —
  v0.89.131 through v0.89.133 operator runbook for the
  Workload Health dashboard panel arc (design at
  [proposals/workload-health-panel-slice1.md](./proposals/workload-health-panel-slice1.md)).
  Polish arc that consolidates the substrate's three
  serverless diagnostics (cold-start latency + sampling
  rate + error rate) into a single dashboard panel between
  TRACE COVERAGE and SPAN QUALITY at `/discovery`. 3-column
  health grid with `Cold-start P95 exceeded` /
  `Sampling too aggressive` / `Error rate spike`. Each
  column is clickable, deep-linking to the per-provider
  Recommendations tab filtered by the corresponding kind
  prefix. Footer line shows the UNION any-issue count
  (resource firing 2 of 3 diagnostics counts as 1).
  Backend endpoint at
  `GET /api/v1/discovery/workload_health` with 30s
  in-memory cache mirroring the v0.89.61 summary pattern;
  cache miss emits `discovery.workload_health.requested`
  audit. No new substrate, metrics, or recommendation
  kinds. Hides when serverless_resource_count is zero OR
  all 3 percentages are zero. **Slice 1 SHIPPED in
  v0.89.133.** The dashboard's primary entrypoint now
  reads top-to-bottom: coverage → workload health →
  span quality.
- [Error rate correlation — operator guide](./error-rate-correlation-operator-guide.md) —
  v0.89.126 through v0.89.130 operator runbook for the error
  rate correlation slice 1 arc (design at
  [proposals/error-rate-correlation-slice1.md](./proposals/error-rate-correlation-slice1.md)).
  Third diagnostic running on the cold-start latency
  substrate; the architectural bet now demonstrated three
  ways (cold-start, sampling rate, error rate). Per-resource
  detection: current 24h error rate vs baseline 7d error
  rate. Fires `span-quality-error-rate-spike` when ratio >
  2.0x AND current invocations >= 1000 AND current errors
  >= 50 AND not excluded. Near-zero baseline guard substitutes
  0.01% as the comparison baseline when the actual baseline
  is below it, avoiding spurious large ratios on tiny
  absolute counts (surfaced via `baseline_adjusted` flag).
  Per-cloud error metrics: AWS `Errors`, GCP Cloud Run
  `request_count{5xx}` + Cloud Functions
  `execution_count{status!=ok}`, Azure `FunctionErrors`,
  OCI `function_invocation_count{result=error}` — all reuse
  existing cold-start IAM via the same `MetricQuerier`
  interface from v0.89.113. Storage v14 → v15 migration adds
  `error_rate_observation` table mirroring
  `cold_start_observation`. New per-resource endpoint at
  `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/error_rate`
  exposes current + baseline windows + ratio + 3 gate flags.
  Per-Serverless-row "Error rate (24h)" column on all 4
  provider tables. iacpicker emits resource-exhaustion case
  (case 3) Terraform patterns per-cloud (memory + concurrency
  raise). 3-failure-mode reasoning explicitly notes cases (1)
  recent deploy regression + (2) downstream dependency
  failure as the MORE COMMON causes that should be DECLINED;
  case (3) resource exhaustion is what the PR targets.
  Together with cold-start latency + sampling rate, completes
  the natural serverless health diagnostic suite. **Slice 1
  SHIPPED in v0.89.130.** Universal claim's MEASURES verb
  gains a third sub-diagnostic.
- [Sampling rate analysis — operator guide](./sampling-rate-operator-guide.md) —
  v0.89.121 through v0.89.125 operator runbook for the
  sampling rate analysis slice 1 arc (design at
  [proposals/sampling-rate-analysis-slice1.md](./proposals/sampling-rate-analysis-slice1.md)).
  Closes the span quality slice 1 §13 deferral; second
  diagnostic running on the cold-start latency substrate
  (proves the architectural bet that the substrate
  compounds). Per-resource detection: observed span count
  from Squadron's traceindex 24h window vs expected
  invocation count from the cloud-native metric API over
  the same window. Fires
  `span-quality-sampling-too-aggressive` (reuses
  `span-quality-` webhook prefix) when ratio < 5% AND
  invocations >= 1000. Per-cloud invocation metrics: AWS
  `Invocations`, GCP Cloud Run `request_count` + Cloud
  Functions `execution_count`, Azure `FunctionInvocations`,
  OCI `function_invocation_count` — all reuse the existing
  cold-start IAM. 24h-window counter added to the Quality
  observer (parallel to slice 1's 1h window). New
  per-resource endpoint at `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/sampling`
  exposes the underlying gate flags. SPAN QUALITY dashboard
  panel grows from 5-column to 6-column grid; QualityDot
  tooltip extends to all 6 percentages. Per-Serverless-row
  "Sampling rate (24h)" column on all 4 provider tables.
  iacpicker emits `OTEL_TRACES_SAMPLER_ARG=0.5` env var
  injection per-cloud. **Slice 1 SHIPPED in v0.89.125.**
  Universal claim's MEASURES verb gains a second
  sub-diagnostic; the "where did my trace go?" chain now
  has 5 layers (event source primitive → event source
  config → W3C trace context → cold-start latency →
  sampling rate).
- [Cold-start latency — operator guide](./cold-start-latency-operator-guide.md) —
  v0.89.112 through v0.89.120 operator runbook for the
  cold-start latency analysis arc. Slice 2 (design at
  [proposals/cold-start-latency-slice2.md](./proposals/cold-start-latency-slice2.md))
  extends the MEASURES verb from slice 1's AWS Lambda
  coverage to all 4 clouds via the existing MetricQuerier
  substrate. Per-cloud implementations: GCP Cloud
  Monitoring V3 (Cloud Run `request_latencies` + Cloud
  Functions `execution_times`), Azure Monitor REST
  (`FunctionExecutionDuration` filtered by
  `IsAfterColdStart`; falls back to unfiltered with
  informational note on older runtimes), OCI Monitoring
  (`function_duration` P95 cross-referenced with
  `cold_start_count` counter; skips detection when
  `cold_start_count = 0`). Detection thresholds (1.5x ratio
  + 500ms floor + 50 baseline samples) pinned identical
  across all 4 clouds. 4 new recommendation kinds reusing
  existing webhook prefixes: `cloudrun-cold-start-baseline`,
  `cloudfunc-cold-start-baseline`,
  `azfunc-cold-start-baseline`,
  `ocifunc-cold-start-baseline`. Per-cloud Terraform
  patterns target `minScale` (Cloud Run),
  `min_instance_count` (Cloud Functions Gen 2), Premium
  Plan migration OR `WEBSITE_USE_PLACEHOLDER=0` (Azure
  Functions), `WARMUP_DELAY` (OCI Functions). All 4
  DiscoveryX Serverless tables now show the Cold-start P95
  (24h) column with the same amber state. Per-cloud rate
  limiters: GCP 60 RPM, Azure 12000 RPH, OCI 10 TPS — all
  well under per-cloud quotas. Cost surface for the new 3
  clouds: essentially \$0 for typical fleets. **Slice 1
  SHIPPED in v0.89.116. Slice 2 SHIPPED in v0.89.120.**
  Universal claim's fifth verb drops the qualification
  asterisk — MEASURES is now uniformly 4-cloud, matching
  the other four verbs. Slice 1 (design at
  [proposals/cold-start-latency-slice1.md](./proposals/cold-start-latency-slice1.md))
  introduced the `MetricQuerier` interface + AWS CloudWatch
  GetMetricStatistics implementation for the Lambda
  `InitDuration` metric + `cold_start_observation` storage
  table (v13 → v14 migration). The per-resource endpoint
  `GET /api/v1/discovery/{provider}/inventory/serverless/{id}/cold_start`
  + the proposer prompt + the AWS-side iacpicker for
  `aws_lambda_provisioned_concurrency_config` all shipped in
  slice 1.
- [Span quality — operator guide](./span-quality-operator-guide.md) —
  v0.89.84 through v0.89.111 operator runbook for the span
  quality arc. Slice 1 (design at
  [proposals/span-quality-slice1.md](./proposals/span-quality-slice1.md))
  inspects every incoming OTLP span on the hot path for three
  pathology classes: orphan spans (broken context propagation),
  spans missing required resource attributes, and spans with
  placeholder values in required attributes. SPAN QUALITY
  panel on the Discovery dashboard sits next to TRACE COVERAGE
  with 3 columns; each Inventory row gets a Quality dot
  indicator. Three slice-1 recommendation kinds:
  `span-quality-{orphan-trace,missing-resource-attrs,attribute-mismatch}`.
  Slice 2 (design at
  [proposals/span-quality-slice2.md](./proposals/span-quality-slice2.md))
  closes the slice 1 W3C trace context parsing deferral. Two
  new pathology detectors at the same Quality observer hot
  path: malformed traceparent (header value doesn't match
  W3C `00-{32hex}-{16hex}-{2hex}` format) and missing
  traceparent on child (span has non-zero parent_span_id but
  no traceparent attribute). Two new recommendation kinds
  reusing the slice 1 webhook prefix:
  `span-quality-traceparent-{missing,malformed}`. SPAN
  QUALITY panel grows from 3-column to 5-column grid; QualityDot
  tooltip shows all 5 percentages. Honest denominators —
  malformed_pct uses spans_with_traceparent; missing-on-child
  uses child_spans. Per-span hot-path overhead measured at
  ~30ns marginal (under the 100ns budget). **Slice 1 SHIPPED
  in v0.89.88. Slice 2 SHIPPED in v0.89.111.** Universal
  claim doesn't grow with slice 2 — it makes the existing
  span quality claim more rigorous by completing the
  three-layer "where did my trace go?" diagnostic
  (event source primitive → event source config → W3C
  trace context).
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  v0.89.73 through v0.89.83 operator runbook for the trace
  integration arc. Slice 1 (design at
  [proposals/trace-integration-slice1.md](./proposals/trace-integration-slice1.md))
  consumed Squadron's own OTLP receiver stream as discovery
  signal, transforming the recommendation surface from "did
  you turn on the primitive" to "is telemetry actually
  flowing." Discovery dashboard gained a TRACE COVERAGE panel;
  per-provider Inventory tabs gained a Last seen column. Slice
  2 (design at
  [proposals/trace-integration-slice2.md](./proposals/trace-integration-slice2.md))
  turned the visibility into 12 new proposer-drafted
  recommendation kinds: `trace-emission-{aws,gcp,azure,oci}-{compute,db,k8s}`.
  New `internal/proposer/iacpicker` package picks which
  Terraform pattern to extend in the operator's IaC repo.
  Dashboard sub-indicator surfaces pending recommendation
  counts; per-provider Recommendations tab gains a "Show only
  trace-emission" filter chip. Webhook routing extends to the
  new kind prefix. **Slice 1 SHIPPED in v0.89.78. Slice 2
  SHIPPED in v0.89.83.** Squadron's claim grows a third verb:
  "scans AWS, GCP, Azure, AND Oracle Cloud across COMPUTE,
  DATABASE, AND KUBERNETES for observability gaps, verifies
  telemetry is actually flowing, AND drafts the IaC PRs that
  close the gaps it finds."
- [GitHub Checks API back-signal](./checks-api.md) — v0.89.42
  through v0.89.44 operator runbook for the inverse of the
  webhook listener: Squadron writes check run state to
  Squadron-opened PRs so operators see "what Squadron is
  seeing" inside GitHub's PR review surface. Status lifecycle
  ties to existing webhook events (in_progress on PR open,
  success on merge, failure on close-without-merge, neutral on
  operator exclude). **Slice 1 SHIPPED in v0.89.44** — covers
  the PAT scope upgrade, verifying the loop end-to-end,
  reading the three new audit event types, and the
  troubleshooting matrix. Design doc is at
  [proposals/checks-api-back-signal.md](./proposals/checks-api-back-signal.md).
- [Alerts](./alerts.md) — rule-based alerts on telemetry, fleet state, and
  rollout health.
- [Audit log](./audit-log.md) — every state change in Squadron is recorded.
  How to filter, what's in the payload, how to use it for post-mortems.
- [Authentication](./auth.md) — opt-in Bearer-token auth, bootstrap
  flow, token management, recovery path.
- [Self-monitoring](./self-monitoring.md) — emit Squadron's own state
  changes as OTel traces into your existing observability stack.
- [squadronctl CLI](./squadronctl.md) — command-line client for
  scripting Squadron from CI pipelines and terminals.
- [Operating Squadron](./operating.md) — environment variables, the
  production checklist, backup considerations, upgrade notes.
- [API reference](./api-reference.md) — REST endpoints with curl examples.

## What Squadron is good at

- **Pushing configs to a fleet without a deploy pipeline.** Squadron speaks
  OpAMP, so updates land in seconds. Drift is detected automatically and
  surfaces in the UI before it bites you.
- **Safe staged rollouts.** Percent or label-based canary selection, dwell
  per stage, auto-abort on drift or error-rate spike, automatic rollback to
  the previous config. Pause / resume mid-rollout if you need to think.
- **Self-contained.** Single Go binary, embedded SQLite + DuckDB. No
  Postgres, no Redis, no Kafka. You can run Squadron on a $5 VPS and a
  modest fleet will fit comfortably.
- **Operator-first UI.** Modern React app with a command palette, live
  updates over SSE, dark mode, keyboard shortcuts, and a real audit timeline.

## What Squadron isn't (yet)

- **Multi-tenant.** Everything is global to a Squadron instance. Run one
  per team or per environment for now.
- **SSO.** Squadron ships Bearer-token auth with a scope
  vocabulary so tokens can be narrowed to read only or to a
  specific surface (see [Authentication](./auth.md) for the full
  scope list — agents:read, rollouts:write, rollouts:approve,
  incidents:write, etc.). What's not built in is SSO/OIDC; that's
  best handled by a reverse proxy in front of Squadron today.
- **A Kubernetes operator.** OpAMP works fine with collectors deployed
  via Helm/manifest; a CRD-based operator that pushes configs into the
  cluster is on the roadmap.
- **A managed service.** Squadron is self-hosted. A hosted Squadron Cloud
  will follow the OSS core.

## Getting help

- File issues at <https://github.com/devopsmike2/squadron/issues>.
- Read the source — it's small and the comments explain why, not just what.
- Inspect Squadron's own audit log (`/api/v1/audit/events`) when something
  unexpected happens; most state transitions are recorded with enough
  context to reconstruct what occurred.
