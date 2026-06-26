# Trace coverage — operator guide

This is the operator-facing runbook for the v0.89.73 through
v0.89.78 trace integration arc that just closed slice 1.
Squadron now consumes its own OTLP receiver stream as a discovery
signal: the Discovery dashboard at `/discovery` gains a TRACE
COVERAGE panel showing what percentage of your inventoried
resources have actually emitted spans in the last 24 hours, and
every Inventory row (Compute, Databases, Kubernetes, across all
four clouds) gains a "Last seen" column.

The strategic frame: every recommendation Squadron made before
this arc was at the configuration-primitive layer ("enable
Container Insights"). Slice 1 of trace integration answers the
adjacent question that operators care about more: "is telemetry
actually flowing." Slice 2 will turn the visibility into
recommendation kinds; slice 1 ships the visibility.

For a first test, the walkthrough takes about 20 minutes — most
of it spent confirming your OTel collectors point at Squadron's
endpoint. For a production deployment with mature OTel coverage
already in place, the trace coverage panel populates in seconds.

## What this is good for

- A team that already has OTel collectors deployed and wants to
  know which discovered resources are actually emitting (closing
  the gap between "primitive enabled" and "spans flowing").
- A team running multi-cloud OTel and wanting one dashboard that
  reads coverage across AWS, GCP, Azure, and Oracle Cloud
  uniformly.
- An auditor who needs to correlate "Squadron flagged this
  cluster as instrumented in scan X" with "Squadron received N
  spans from that cluster in audit Y" — both signals live in the
  same audit timeline now.

## What this is NOT (slice 1)

Read this carefully. Slice 1 is intentionally narrow:

- **No recommendation generation for trace gaps.** Slice 1 SHOWS
  you that EC2 instance `i-0abc` has Container Insights enabled
  but has emitted zero spans in 24h. It does NOT yet draft a
  Terraform PR to fix the gap. Recommendation kinds in the
  `trace-emission-*` family are slice 2.
- **No span quality analysis.** Slice 1 sees spans arriving and
  counts them. It does not yet assess whether resource
  attributes are complete or whether parent-child context
  propagation is unbroken. Slice 3.
- **No metrics or logs integration.** Same OTLP receiver
  ingests metrics and logs, but the inventory correlation rules
  differ per signal type. Metrics integration is slice 2, logs
  is slice 3.
- **No cross-cloud span propagation correlation.** A span chain
  that crosses AWS Lambda then SQS then a GCP Cloud Function
  carries context across cloud boundaries; Squadron sees the
  spans but does not yet correlate the chain. Slice 4+.
- **No per-service-version detection.** When a deployment bumps
  to a new version and emission stops, Squadron does not yet
  flag the regression. Slice 2 candidate.

The [trace integration slice 1 design doc](./proposals/trace-integration-slice1.md)
§2 and §13 track slice 2+ candidates.

## How the matching works

Squadron receives spans on the existing OTLP HTTP endpoint
(port 4318) and gRPC endpoint (port 4317). Both receivers are
part of the standard `all-in-one` build — no separate setup.

For each incoming span batch, Squadron extracts the resource
attributes (`host.id`, `cloud.resource_id`,
`k8s.cluster.name`, etc.) and projects them into a
`resource_key` using a six-tier fallback chain. The discovery
side projects the SAME key from inventory snapshot fields. The
two sides join on equality.

The six tiers in priority order:

1. **`cloud.resource_id` matches verbatim.** If your OTel SDK
   sets the full ARN-shaped identifier (some SDKs do, most do
   not), Squadron keys on that. Match confidence: **strong**.
2. **`host.id` + `cloud.account.id`.** Most common AWS / GCP /
   Azure / OCI hosts emit this pair via the OTel host detector.
   Key shape: `<provider>:<account>:<host_id>`. Strong.
3. **`k8s.cluster.name` + `cloud.account.id`.** For workloads
   running on managed Kubernetes. Key shape:
   `<provider>:<account>:k8s:<cluster_name>`. Strong.
4. **`db.system` + `db.name` + `cloud.account.id`.** For
   database workloads using the OTel DB instrumentation. Key
   shape: `<provider>:<account>:db:<db_system>:<db_name>`. Strong.
5. **`host.name` alone.** Useful when no cloud-aware host
   detector is enabled. Match confidence: **weak** — the
   discovery dashboard surfaces a caveat indicator on this row.
6. **`service.name` alone.** Last-resort fallback. Operators
   should not see weak-match-via-service-name in production
   except on misconfigured exporters. Match confidence: **weak**.

When the discovery dashboard says "67% coverage with 12% weak
matches" for a provider, it means 67% of inventoried resources
have at least one span in the last 24h, and 12% of those joins
fell back to `host.name` or `service.name`. The remaining
matches were strong.

## Prerequisites

- A Squadron deployment on v0.89.78 or later.
- At least one discovered resource via one of the four cloud
  scanners (AWS / GCP / Azure / OCI). Without inventory there's
  nothing to compute coverage against.
- (Optional but valuable) An OTel-instrumented workload pointing
  at Squadron's OTLP endpoint. Slice 1 shows coverage as a gap
  view; the gap is only meaningful when there's a denominator.

The trace integration is ON by default. To disable (for example
during a load test):

```sh
export SQUADRON_TRACEINDEX_DISABLED=true
```

The receiver still accepts traces; the index just doesn't
update. Discovery dashboard's trace coverage panel reports
zero in this state.

## Step 1 — Point your collectors at Squadron's OTLP endpoint

If you're already running OTel collectors emitting elsewhere
(Tempo, Honeycomb, Datadog), add Squadron as a SECOND export
destination. Squadron does not require exclusive ownership of
your trace stream.

Sample collector config addition:

```yaml
exporters:
  otlphttp/squadron:
    endpoint: https://your-squadron-host:4318
    headers:
      Authorization: Bearer ${SQUADRON_OTLP_TOKEN}

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo, otlphttp/squadron]
```

Squadron currently accepts unauthenticated OTLP requests on the
receiver port; auth tokens are roadmap work. Operators with
strict posture should put Squadron's receiver behind a TLS-
terminating reverse proxy.

## Step 2 — Run a discovery scan to populate the denominator

Open the Squadron UI, navigate to Discovery, pick one of your
connected clouds, and run a scan. The scan populates the
inventory tables that the trace coverage view joins against.

Without scans, the trace coverage panel shows "Run a discovery
scan to populate the trace coverage view."

## Step 3 — Open the Discovery dashboard

Navigate to `/discovery`. Look for the **Trace coverage** panel
below the existing instrumentation coverage ring.

The panel shows:

- **Large number**: overall percentage of inventoried resources
  with at least one span in 24h.
- **Subtext**: `N of M inventoried resources have emitted spans
  in the last 24h.`
- **Coverage ring**: green ≥ 80%, yellow 50-80%, red < 50%.
- **Per-provider chips**: small badges showing per-cloud
  coverage. Each chip is color-coded the same way.
- **Caveat indicator** (yellow icon): appears next to any chip
  where weak matches exceed 20%. Hover for explanation.

## Step 4 — Drill into a per-cloud Inventory tab

Click the "View details" link on one of the provider cards (or
navigate to `/discovery/aws`, `/discovery/gcp`, etc.). The
Inventory tab now shows a new **Last seen** column on every
sub-tab (Compute, Databases, Kubernetes).

Values:

- "2m ago" / "1h ago" / "3d ago" — relative time in compact form
- "1w ago" / "2w ago" — for older but recent emissions
- A literal date (YYYY-MM-DD) for emissions older than 30 days
- "never" — never emitted; a yellow indicator invites investigation

The "never" rows are the actionable signal. They are resources
Squadron has discovered (so the scanner can see them) but has
received zero spans from. Three common causes, in rough
prevalence order:

1. **OTel SDK not deployed.** The most common reason. The
   primitive bit (Container Insights, Cloud Logging, etc.) was
   turned on, but no SDK or collector ever got deployed to emit
   spans. Slice 2 will draft Terraform PRs to fix this; slice
   1 surfaces it for manual investigation.
2. **SDK deployed but exporter misconfigured.** Spans are
   generated but the OTLP exporter points at the wrong endpoint
   or fails auth. Check the collector logs and confirm the
   endpoint matches your Squadron deployment.
3. **Resource attribute mismatch.** Spans are flowing but the
   `host.id` / `cloud.account.id` / `host.name` they carry
   don't align with what Squadron expects. Toggle the host
   detector and cloud detector on your OTel SDK; on EC2 the
   correct detector set is `host`, `os`, `ec2`. The dashboard's
   weak-match caveat usually correlates with this failure mode.

## Step 5 — Reading the audit signal

Open the Timeline page. Filter by event type for the new trace
integration events:

- **`trace_index.background_flushed`** — fires every 30 seconds
  (default flush interval) when there are rows to write. Payload
  carries `rows_written`, `rows_evicted`, `duration_ms`,
  `interval_s`. Useful for confirming the index is healthy.
  Empty cycles do NOT emit; this keeps the timeline clean during
  low-traffic windows.
- **`discovery.trace_coverage.requested`** — fires once per
  cache miss on the `/api/v1/discovery/trace_coverage` endpoint.
  Cache TTL is 30 seconds, so a rapid burst of dashboard refreshes
  yields one audit event, not N.

No span content in either payload. Squadron's existing DuckDB
store keeps full span data; audit captures only the meta-shape.

## Step 6 — (Optional) Tune the row cap

The traceindex has a hard cap (default 100,000 rows) with LRU
eviction. This prevents a high-cardinality attribute set (a span
emitting a unique service name per request) from blowing up the
index.

To change the cap:

```sh
export SQUADRON_TRACEINDEX_MAX_ROWS=500000
```

The flush audit event's `rows_evicted` field is non-zero when
the cap is being hit; that's the signal to raise it. If
operators routinely see eviction in steady state, investigate
the cardinality of your service.name attribute first — high
service.name churn is the most common cause and usually
indicates a misconfigured exporter or auto-instrumentation
labeling pods rather than services.

## Troubleshooting matrix

| Symptom | Likely cause | Remedy |
|---|---|---|
| Trace coverage panel shows 0% for all providers | No spans received yet | Confirm collectors point at Squadron's OTLP endpoint; check `trace_index.background_flushed` audit events fire |
| Trace coverage panel shows 0% but `/v1/traces` requests are arriving | Resource attributes too sparse — slice 1 can't extract a key | Enable cloud-aware host detector in your OTel SDK (`host`, `os`, cloud detector for your provider) |
| Most rows show "never" but you know your service emits | host.id / cloud.account.id mismatch with what scanner sees | Verify host.id is the instance ID (i-... for EC2); for GCE use numeric ID |
| Weak match percentage is high (> 20%) | Many spans key on host.name or service.name | Enable the cloud-aware host detector; spans without `cloud.account.id` always fall to weak match |
| `rows_evicted` consistently non-zero | Index cap is being hit | Either raise `SQUADRON_TRACEINDEX_MAX_ROWS` or audit `service.name` cardinality |
| Trace coverage shows N% strong + M% weak but doesn't add to 100 | Some inventory rows have NO match at all — strong+weak ≤ 100 | This is correct — the gap is the uncovered portion |
| Last seen column shows "1w ago" for actively-running services | Bug or stale traceindex | Restart Squadron; the in-memory cache reseeds on receiver activity |
| Audit shows `trace_index.background_flushed` but discovery summary doesn't update | Discovery dashboard cache is 30s; refresh after the next cycle | Click the dashboard's refresh button or wait |

## What this means for the universal observability claim

Slice 1 of trace integration shifts Squadron from discovery +
recommendation to discovery + RECONCILIATION. The model up to
v0.89.77 was "Squadron tells you what's wrong, you fix it."
Now: "Squadron tells you what's wrong, you fix it, Squadron
verifies the fix actually closed the gap."

Universal observability claim grows another dimension:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps AND
> verifies telemetry is actually flowing.

Slice 2 SHIPPED in v0.89.79 through v0.89.83. Read on for the
slice 2 operator workflow.

---

# Slice 2 — recommendation kinds (v0.89.79 through v0.89.83)

Slice 1 of trace integration shipped VISIBILITY: the dashboard
panel, the per-Inventory-row `Last seen` column. Slice 2 ships
ACTION: 12 new recommendation kinds the proposer drafts when a
resource has its observability primitive enabled but Squadron's
traceindex has seen no spans from it in the last 24 hours.

This is the loop the Tuesday LinkedIn drumbeat post implicitly
promised: Squadron doesn't just tell you what's wrong, it drafts
the IaC PR that closes the gap, you review + merge + verify, and
the verdict learning loop teaches the proposer from your
decision.

## The 12 new recommendation kinds

One per provider per tier:

```
trace-emission-aws-compute     trace-emission-gcp-compute
trace-emission-aws-db          trace-emission-gcp-db
trace-emission-aws-k8s         trace-emission-gcp-k8s

trace-emission-azure-compute   trace-emission-oci-compute
trace-emission-azure-db        trace-emission-oci-db
trace-emission-azure-k8s       trace-emission-oci-k8s
```

Each kind targets a specific Terraform pattern that deploys the
cloud-native auto-instrumentation agent for that provider's
tier. The proposer's reasoning explains which pattern was
picked and why.

## When a recommendation fires

The detection rule is:

```
inventory_row.primitive_enabled == true
  AND inventory_row.last_seen_at is null OR > 24h ago
  AND inventory_row.last_excluded_at is null
```

The 24h staleness threshold is intentionally permissive in
slice 2. Slice 3 may tune per workload type (a daily batch job
won't emit continuously; a web service silent for an hour
probably has a real issue).

For each tier:

- **Compute**: `primitive_enabled` = "has otel* tag" — the slice
  1 detection rule. An EC2 with the `otel-collector` tag but no
  spans in 24h fires `trace-emission-aws-compute`.
- **Database**: `primitive_enabled` = per-cloud database axis —
  `PerformanceInsightsEnabled` for RDS, `QueryInsightsEnabled`
  for Cloud SQL, etc.
- **Kubernetes**: `primitive_enabled` = per-cloud observability
  addon — ADOT for EKS, Managed Prometheus for GKE, Azure
  Monitor for AKS, Ops Insights for OKE.

## The three failure modes the recommendation acknowledges

The recommendation's reasoning text always names three possible
causes for the gap. Slice 2 ALWAYS drafts the IaC PR for case
(a). If your situation is actually (b) or (c), decline the PR
and the verdict learning loop records it.

1. **SDK not deployed.** The most common cause. The Terraform
   PR installs the cloud-native auto-instrumentation agent
   (the ADOT Collector via the AWS-managed SSM package for EC2, AzureMonitorAgent
   extension for Azure VMs, etc.). Merge if this is your case.

2. **SDK deployed but exporter misconfigured.** The agent is
   running but pointed at the wrong OTLP endpoint, has a
   broken authentication token, or has been throttled by the
   collector. Check the agent's exporter configuration before
   merging — the PR is wrong for this case. Decline with a
   note like "SDK already deployed; checking exporter config."

3. **SDK running but attribute mismatch.** The agent is
   emitting fine, but with `host.name` or `cloud.resource_id`
   values that don't match Squadron's expectation. Squadron
   sees spans, just not attributed to this inventory row.
   Decline with a note like "spans flowing under different
   host.name — investigating resource detector config."

The verdict learning loop records decline reasons and feeds
them into the next proposal cycle. After 3-5 declines in a
scope with the same reason, the proposer down-weights the
recommendation kind for similar inventory rows.

## The dashboard sub-indicator

Below the existing TRACE COVERAGE panel ring + per-provider
chips, slice 2 adds a sub-indicator line:

> ⚠ N resources have the primitive enabled but no recent
> emission — see Recommendations on each provider for the
> drafts.

The count is the sum of pending trace-emission recommendations
across all 4 providers. The line is hidden entirely when the
count is zero. Clicking the link takes you to the AWS
Recommendations tab; the per-provider Recommendations tabs each
have a filter chip "Show only trace-emission" that filters the
list to just these kinds.

## The Recommendations filter chip

Each per-provider page's Recommendations tab gains a chip:

> [ Show only trace-emission ]

Click it to filter the list to only `trace-emission-*` kinds.
Click again to clear the filter. The chip is yellow when
active (matching the slice 1 weak-match-caveat color), slate
otherwise.

The chip currently only renders on the AWS Recommendations tab.
The GCP/Azure/OCI Recommendations tabs are stubs awaiting their
own chunk-5 follow-on. The dashboard sub-indicator deep-links
to AWS for now.

## Per-cloud Terraform patterns (the IaC picker)

The new `internal/proposer/iacpicker` package picks which
Terraform pattern to extend in your IaC repo. The picker reads
the existing repo content (when available) and decides whether
to extend an existing block or introduce a new one. Falls back
to a documented default when it can't parse the repo or finds
no related block.

For Azure AKS — the most contested tier — the picker uses a
deterministic three-way disjunction:

1. **If the existing `azurerm_kubernetes_cluster` block has
   `oms_agent`** → extend that block (legacy Container Insights
   path wins, because mixing two observability addons in the
   same cluster is a known failure mode).
2. **Else if it has `azure_monitor_profile`** → extend that.
3. **Else** → introduce `monitor_metrics.annotations_allowed`
   (the §5 newer-default).

Comments in HCL (`#`, `//`) are ignored, so a commented-out
`oms_agent` doesn't false-positive into the legacy branch.

The picker's `Reasoning` field captures the picker's decision
in 1-2 sentences for the proposer's prompt context. You'll see
it in the recommendation reasoning text on the
Recommendations tab.

The 12 per-cloud Terraform patterns the picker emits — copy
these into your IaC review checklist:

- **AWS EC2** (`trace-emission-aws-compute`):
  `aws_ssm_association` with the `AWS-ConfigureAWSPackage` doc
  installing the ADOT Collector via the AWS-managed
  `AWSDistroOTel-Collector` package (auto-selects arm64/amd64). This
  is the ADOT Collector, not the CloudWatch Agent — the latter does
  not emit OpenTelemetry traces.
- **AWS RDS** (`trace-emission-aws-db`):
  `performance_insights_retention_period = 731` on
  `aws_db_instance` (LTR tier).
- **AWS EKS** (`trace-emission-aws-k8s`):
  `aws_eks_addon` with `addon_name = "adot"`.
- **GCP GCE** (`trace-emission-gcp-compute`):
  `google_compute_instance.metadata` enabling `enable-osconfig`
  + `google-logging-enabled` + `google-monitoring-enabled`.
- **GCP Cloud SQL** (`trace-emission-gcp-db`):
  `insights_config { record_application_tags = true;
  record_client_address = true }` on
  `google_sql_database_instance`.
- **GCP GKE** (`trace-emission-gcp-k8s`):
  `google_gke_hub_feature` with `name = "servicemesh"`.
- **Azure VM** (`trace-emission-azure-compute`):
  `azurerm_virtual_machine_extension` with type
  `AzureMonitorLinuxAgent`.
- **Azure SQL** (`trace-emission-azure-db`):
  `azurerm_mssql_database_extended_auditing_policy` with
  `log_monitoring_enabled = true`.
- **Azure AKS** (`trace-emission-azure-k8s`):
  `monitor_metrics.annotations_allowed` per the picker
  decision rule above.
- **OCI Instance** (`trace-emission-oci-compute`):
  cloud-init script in `oci_core_instance.metadata.user_data`.
  ⚠ Cloud-init only runs on first boot — the recommendation
  flags this as upgrade-during-maintenance. You'll need to
  re-launch or migrate to a fresh instance with the new
  user_data.
- **OCI Autonomous DB** (`trace-emission-oci-db`):
  `oci_database_management_managed_database_group`.
- **OCI OKE** (`trace-emission-oci-k8s`):
  OCI Service Operator deployed via `kubernetes_manifest`.

## Webhook routing for the new kinds

The IaC webhook handler now recognizes the `trace-emission-*`
prefix and extracts the provider from the kind itself
(`trace-emission-{provider}-{tier}`). All 12 kinds route to the
correct provider's audit scope without any wiring changes on
your end. The kind-prefix detection sits at the top of the
switch — more specific than the existing single-segment prefixes
(`gce-`, `vm-`, `compute-`, etc.) — so the routing stays
deterministic even when kinds share a vague provider hint.

If you've set up a SIEM consumer to filter on
`recommendation_kind`, the prefix `trace-emission-` gives you a
single regex for the new arc:

```
recommendation_kind ~= "^trace-emission-"
```

## Workflow — first trace-emission recommendation

1. Open the Discovery dashboard at `/discovery`. Confirm the
   TRACE COVERAGE panel renders. If the sub-indicator line
   below the per-provider chips shows a non-zero count, you
   have at least one resource that will trigger a
   trace-emission recommendation.
2. Click into the relevant per-provider page (AWS for now;
   GCP/Azure/OCI Recommendations tabs ship in their own
   chunk-5 follow-on).
3. Click the "Show only trace-emission" filter chip.
4. Review each draft recommendation. The reasoning text names
   all three failure modes; pick yours.
5. For case (a): click Open PR. The webhook listener records
   the open. When you merge the PR, the
   `recommendation.pr_merged` audit fires and the proposer's
   verdict learning loop records the merge as positive signal
   for the kind in this scope.
6. For case (b) or (c): click Don't propose this again (chunk
   4 of #531 slice 2 affordance). Leave a decline reason; the
   verdict learning loop records the decline.
7. Wait 24h+ and re-run discovery. If your fix worked, the
   `Last seen` column should show a recent timestamp on the
   target inventory row, and the count in the dashboard
   sub-indicator should drop.

## Troubleshooting

- **A recommendation says "SDK not deployed" but I just deployed
  the agent yesterday.** The 24h window may not have elapsed yet
  with the agent live, OR the agent is emitting under a
  different `host.name` / `cloud.resource_id` than Squadron
  expects. Check the audit timeline for spans from your scope —
  if Squadron is receiving them but the inventory's `Last seen`
  is still empty, you're in case (3) attribute-mismatch.
- **The dashboard sub-indicator says 5 but the per-provider
  Recommendations tab shows 0.** The proposer hasn't run a
  discovery scan since the inventory row aged out of recent
  emission. Click "Run discovery" on the per-provider page to
  force a scan; the recommendations should populate.
- **The iacpicker recommended `monitor_metrics` but my AKS
  cluster has `oms_agent` already.** Check the picker's
  Reasoning field — if it says "fallback to default; couldn't
  parse repo content," your IaC content wasn't supplied to
  the picker. Either the connect-IaC-repo wizard didn't run
  through for this repo, or the picker hit an HCL parse
  failure. The audit timeline shows `iacpicker.fallback_used`
  when this happens. Decline the PR and check your IaC repo
  connection.
- **I declined a `trace-emission-aws-k8s` recommendation but it
  keeps coming back.** The verdict learning loop down-weights
  after 3-5 declines with the same reason in the same scope.
  One decline isn't enough signal yet. Add a meaningful decline
  reason and the loop incorporates it faster.
- **I'm getting trace-emission recommendations on resources I
  know don't need spans (cron jobs, ephemeral CI runners,
  etc.).** Click Don't propose this again on each. The
  exclusion list persists per (scope, recommendation_kind,
  resource_id). Slice 3 may add workload-aware staleness
  thresholds.

## What slice 2 is NOT

Read this carefully. Slice 2 is bounded the same way slice 1
was:

- **Squadron does NOT deploy the SDK itself.** The
  recommendation drafts the IaC PR. The operator reviews and
  merges. The operator's CI applies. Squadron remains a
  recommender, not an actor.
- **Per-language SDK customization is slice 3.** Slice 2 ships
  the cloud-native generic auto-instrumentation paths (ADOT,
  Ops Agent, Azure Monitor Agent, OCI APM agent). Per-language
  deep customization (Python asyncio, Go net/http middleware,
  etc.) ships later.
- **Service mesh sidecar injection is slice 3+.** Istio /
  Linkerd / Cilium ambient have their own propagation paths.
- **Helm chart deployment via the kubernetes Terraform provider
  is a slice 2 candidate but not in scope.** Slice 2 ships
  pure-IaC patterns.
- **Span quality recommendations are slice 3.** Bad context
  propagation, missing resource attributes, etc.
- **Cross-cloud trace correlation is slice 4+.** A span chain
  that crosses AWS Lambda then SQS then GCP Cloud Function
  carries context across cloud boundaries; that's a separate
  arc.
- **Workload-aware staleness thresholds are slice 3.** Today
  all kinds use the same 24h window.

## The universal claim grows a third verb

After slice 2, Squadron's positioning reads:

> Squadron scans AWS, GCP, Azure, AND Oracle Cloud across
> COMPUTE, DATABASE, AND KUBERNETES for observability gaps,
> verifies telemetry is actually flowing, AND drafts the IaC
> PRs that close the gaps it finds.

Three verbs. One control plane. Squadron has gone from
discovery + recommendation (the v0.85 era) to discovery +
recommendation + reconciliation (today). The Tuesday LinkedIn
drumbeat narrative — "make the postmortem about the proposal
the operator turned down" — is now operator-visible at every
layer of the stack.

## Cross-references

- [Trace integration slice 1 design doc](./proposals/trace-integration-slice1.md) —
  the locked spec this runbook operationalizes.
- [Unified Discovery dashboard slice 1](./proposals/unified-discovery-dashboard-slice1.md) —
  the dashboard the new TRACE COVERAGE panel sits on.
- [Database tier slice 2](./proposals/database-tier-slice2.md) —
  one of the tiers whose Inventory tab gains a Last seen column.
- [Kubernetes tier slice 2](./proposals/kubernetes-tier-slice2.md) —
  same.
- [Audit log](./audit-log.md) — full catalog of event types
  including the two new trace integration events.
