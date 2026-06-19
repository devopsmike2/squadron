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
- **SSO/RBAC.** Squadron ships Bearer-token auth (see [Authentication](./auth.md))
  but every token has full API access — there's no concept of
  "read-only" or scoped roles yet. SSO/OIDC is best handled by a
  reverse proxy in front of Squadron today.
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
