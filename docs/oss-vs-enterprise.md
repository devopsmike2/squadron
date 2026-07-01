# What's in OSS vs Enterprise

Squadron's open-source core is **free for any size fleet and self-hostable
forever** under Apache 2.0. The principle is simple: **breadth and the core
loop are OSS; depth, scale, governance, and support are the future commercial
tier.** Nothing below moves out of OSS — the commercial tier adds capabilities
on top.

## In OSS today (free, self-hosted)

**Discover & remediate**
- Multi-cloud discovery across **AWS, GCP, Azure, and OCI** — inventory across
  compute, databases, Kubernetes, serverless, object stores, load balancers,
  and event sources.
- AI recommendations for un-instrumented / under-observed resources (bring your
  own `ANTHROPIC_API_KEY`).
- Merge-ready Terraform **pull requests** to your IaC repo — HCL-aware merge,
  `terraform validate` gate, and verdict learning (a decline teaches future
  scans).
- `env -> Terraform` import-block generation for un-managed resources.

**Run your OTel fleet**
- OpAMP control plane: agents, groups, live fleet map.
- Staged rollouts with per-stage dwell and **auto-abort** on drift / drop-rate.
- Config editor: Monaco + AI Assist + Squadron Lint + live pipeline view.

**See & control cost**
- Cost Insights + Savings: $/month projection from observed ingest, Quick Wins
  ranked by dollars saved.
- Alerts, audit log, incident drafting, demo mode.

**Operate**
- Single instance, embedded store, Bearer-token auth + scopes.

## Planned for the commercial tier (depth, scale, governance, support)

- **Identity & access**: SSO (SAML/OIDC), SCIM provisioning, full RBAC,
  multi-team / multi-tenancy. (OSS ships Bearer tokens + scopes.)
- **Rollout governance**: approval chains, change windows, policy guardrails on
  what AI may propose.
- **Compliance**: long-term + tamper-evident audit retention, SOC 2 evidence
  exports, access reviews.
- **Scale & HA**: clustered control plane, Postgres / managed store backends,
  10k+ agent fleets, multi-region.
- **Advanced detection**: signals that require paid telemetry layers (AWS Lambda
  Insights, Azure Application Insights) plus anomaly / ML detection. The
  add-on-backed cold-start + error-rate regression detectors ship in the
  **enterprise edition**; within it, `commercial_detectors.enabled` (default off)
  is a per-scan cost/safety switch. On an **OSS build the switch is inert** — the
  entitlement is the build edition, not the flag — and OSS instead surfaces the
  gap by recommending the add-on (`lambda-insights-enable` /
  `azfunc-appinsights-enable`). See
  [Enabling commercial-tier detection](./detection-coverage.md#enabling-commercial-tier-detection)
  for the operator flow + the per-cloud add-on / RBAC prerequisites.
- **Cost at org scale**: showback / chargeback, budgets + forecasting,
  multi-backend rate management.
- **Deployment options**: air-gapped, bring-your-own / on-prem LLM.
- **Support**: SLAs, managed SaaS, onboarding services.

## How the boundary is enforced

The OSS / enterprise line is a **build-time** boundary, not a runtime license
check. The open core defines extension-point interfaces and compiles **no-op
providers**; the enterprise edition supplies the real providers, injected at
build time behind an edition build tag. So the entitlement is *which code is
compiled in* — an OSS binary cannot be turned into an enterprise binary by
flipping a config flag. Runtime switches like `commercial_detectors.enabled`
are **cost/safety** toggles that only take effect inside the enterprise edition.

Confirm which edition a running instance is via the startup log
(`squadron build edition`) or the `squadron_build_info{edition=...}` metric on
`/metrics`. The full model — build tags, targets, and the contract every paid
feature follows — is in [docs/build.md](build.md) and
[docs/architecture/oss-enterprise-separation.md](architecture/oss-enterprise-separation.md).

## The promise

The SMB / single-team experience stays free. If you outgrow it into
organization-scale identity, governance, compliance, or scale needs, that's
where the commercial tier comes in — it never takes away what's already free.

> Internal note for maintainers: the rationale + GTM sequencing behind this
> split lives in `docs/oss-ga-and-enterprise-boundary.md`.
