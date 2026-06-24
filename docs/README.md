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
  SHIPPED in v0.89.107.** Squadron's claim grows a sixth
  tier: "scans AWS, GCP, Azure, AND Oracle Cloud across
  COMPUTE, DATABASE, KUBERNETES, SERVERLESS, ORCHESTRATION,
  AND EVENT SOURCES for observability gaps, verifies
  telemetry is actually flowing, validates the spans
  Squadron receives are healthy, AND drafts the IaC PRs
  that close the gaps it finds."
- [Orchestration tier — operator guide](./orchestration-tier-operator-guide.md) —
  v0.89.94 through v0.89.98 operator runbook for the
  orchestration tier slice 1 arc (design at
  [proposals/orchestration-tier-slice1.md](./proposals/orchestration-tier-slice1.md)).
  Fifth tier alongside compute / database / kubernetes /
  serverless. Three surfaces across three clouds: AWS Step
  Functions, GCP Workflows, Azure Logic Apps (Standard +
  Consumption tiers). OCI orchestration deferred to slice 2
  because Resource Manager + Process Automation are
  shape-different from the AWS/GCP/Azure trio. New
  Orchestration Inventory sub-tab on AWS/GCP/Azure pages;
  hidden conditional on OCI. 6 new recommendation kinds:
  `stepfunc-{xray-active,logging-enable}`,
  `workflows-{trace-enable,logging-enable}`,
  `logicapps-{appinsights-enable,diagnostics-enable}`.
  Discovery summary + trace_coverage endpoints gain
  `orchestration_count` and `orchestration_pct`. Dashboard
  TRACE COVERAGE chip breakdown adds ORCH column.
  **Slice 1 SHIPPED in v0.89.98.** Squadron's claim grows a
  fifth tier (qualified — 3 clouds in slice 1, full 4-cloud
  in slice 2 when OCI's primitives get their own treatment).
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
