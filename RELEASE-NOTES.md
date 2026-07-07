# Squadron — Release Notes

Operator-facing release notes for the Squadron open core (Apache-2.0). This file
consolidates the **v0.89.x pre-GA train into the GA v1.0 candidate**, grouped by
theme; the granular per-patch history lives in the git tags (`v0.89.x`). The
enterprise pack keeps its own notes in the private `squadron-enterprise` repo.

---

## GA v1.0 (candidate)

The principle (see [docs/oss-vs-enterprise.md](docs/oss-vs-enterprise.md)):
**breadth and the core loop are OSS and free for any fleet size; depth, scale,
governance, and support are the enterprise tier.** The enterprise features below
are *reserved seams* in this repo — inert (404 / no-op) until the private
enterprise wire files are compiled in; the OSS test suite proves the inertness
(see [docs/build.md](docs/build.md) "Editions contract").

### What ships in OSS

**Discover & remediate**
- Multi-cloud discovery — AWS, GCP, Azure, OCI (compute, databases, Kubernetes,
  serverless, object stores, load balancers, event sources).
- AI recommendations for un-/under-observed resources (bring your own
  `ANTHROPIC_API_KEY`); merge-ready Terraform PRs (HCL-aware merge, `terraform
  validate` gate, verdict learning); `env → Terraform` import-block generation.

**Run your OTel fleet**
- OpAMP control plane: agents, groups, live fleet map.
- Staged rollouts with per-stage dwell + auto-abort on drift / drop-rate.
  Rollout engine hardened for scale: **delta-push + targeted retry** (a
  percent-stage no longer re-pushes the whole prefix — ~50k→1k pushes at 100
  stages; ADR 0021), context-aware ack waits, concurrent stage pushes, and
  storage version-CAS concurrency guards.
- Config editor: Monaco + AI Assist + Squadron Lint + live pipeline view.

**See & control cost**
- Cost Insights + Savings ($/month projection, dollar-ranked Quick Wins).
- Alerts, incident drafting, demo mode.

**Audit & evidence (OSS breadth)**
- Append-only audit log with a single-tenant **CSV/JSON evidence export**
  (`/audit/events?format=`), an Export button, and an actor/event-type
  access-review filter panel (ADR 0020). Exports are themselves audited.
- Opt-in **anonymous usage reporting** (off by default; aggregate counts only,
  no identifiers; ADR 0022 / `docs/usage-reporting.md`).

**Multi-tenancy correctness (OSS breadth)**
- `tenant_id` scoping throughout (single implicit `default` tenant in OSS,
  byte-identical to pre-tenancy behavior). Per-owner background tenanting.
- **Per-tenant trace-index LRU eviction** (ADR 0024): one tenant can no longer
  evict another tenant's trace-index rows — a multi-tenant isolation fix.

**Operate**
- Frictionless install (Helm, `deploy/agent` one-command onboarding, doctor),
  single instance + embedded store, Bearer-token auth + scopes.

### Enterprise-reserved (inert seams in OSS → 404 / no-op)

| Capability | OSS-inert seam |
|---|---|
| SSO (OIDC login + SCIM provisioning), full RBAC, multi-team/tenant isolation | `/auth/oidc/*`, `/api/v1/scim/v2/*`, `/api/v1/rbac/*`, `/api/v1/tenants/*` → 404; identity/authz/tenant providers |
| Compliance audit **export** (cross-tenant, streamed CSV/NDJSON) + **access review** (per-actor/-resource/-tenant, ADR 0020/0022) | `/api/v1/audit-export/*`, `/api/v1/audit-review/*` → 404 |
| Per-tenant **usage/billing** (chargeback/showback, ADR 0023) | `/api/v1/usage/*` → 404 |
| **Differentiated per-tenant trace budgets** (ADR 0024) | `traceBudgetProvider` → nil (uniform global cap in OSS) |
| Add-on-backed detectors (Lambda/App Insights), approval chains, change windows, SIEM export, tamper-evident retention, scale/HA, support | build-tag providers + `commercial_detectors.enabled` (inert in OSS) |

### Reliability & CI (this train)
- Backend correctness/robustness sweeps (exp-histogram dead-letter, observation
  connection-scoping, cross-table write atomicity, `rows.Err()` sweep, OTLP DoS
  bound, OpAMP/SIEM data races).
- Ingest hardening: DuckDB bulk writes (~50k/s), byte-budget queue bounds.
- CI: GitHub Actions bumped to their Node-24-native majors; frontend lockfile-
  exact installs; the enterprise composed build is verified on every PR.

---

_Per-patch detail: `git tag -l 'v0.89.*'` and the commit log. Architecture +
the OSS/enterprise boundary: [docs/oss-vs-enterprise.md](docs/oss-vs-enterprise.md),
[docs/build.md](docs/build.md)._
