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
- [Trace coverage — operator guide](./trace-coverage-operator-guide.md) —
  v0.89.73 through v0.89.78 operator runbook for the trace
  integration arc (design at
  [proposals/trace-integration-slice1.md](./proposals/trace-integration-slice1.md)).
  First arc that consumes Squadron's own OTLP receiver stream
  as discovery signal, transforming the recommendation surface
  from "did you turn on the primitive" to "is telemetry
  actually flowing." Discovery dashboard gains a TRACE COVERAGE
  panel; per-provider Inventory tabs gain a Last seen column.
  **Slice 1 SHIPPED in v0.89.78.** Squadron's claim grows: "scans
  AWS, GCP, Azure, AND Oracle Cloud across COMPUTE, DATABASE,
  AND KUBERNETES for observability gaps AND verifies telemetry
  is actually flowing."
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
