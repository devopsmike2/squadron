# What is Squadron?

Squadron is an **open-source control plane for your OpenTelemetry collector
fleet** — with a governed change loop and a tamper-evident audit trail behind
every move. It sits between your agents and your observability backend and
answers the question nobody else owns: *is my collector fleet configured
safely, is it drifting, and can I prove who changed what?*

This page is the five-minute picture: the gap Squadron fills, the model it's
built around, what it does today, who it's for, why it's open-core, and where
it's heading. For hands-on setup, jump to [Getting
started](getting-started.md); for the feature tour, see the
[README](https://github.com/devopsmike2/squadron#readme).

## The gap it fills

Observability tools own your **data**. IaC tools own **provisioning**. In
between sits a question neither answers: the collectors themselves — their
config, their drift, their health, and the record of every change made to them.

Squadron is a **control plane, not a data plane.** Your telemetry keeps flowing
to Grafana, Prometheus, Mimir, Loki, Tempo, or Datadog exactly as it does
today. Squadron manages the fleet that feeds them. It never becomes another
place your telemetry lands.

## The idea underneath everything: governed change

Most of what Squadron does is one shape applied over and over:

> **proposed → policy-checked → approved by a human when the risk warrants it →
> enforced at the point it happens → recorded in a tamper-evident ledger.**

A config change to a collector runs that loop. A cost-reduction plan runs it. A
discovered, un-instrumented resource becomes a merge-ready Terraform PR that
runs it. The value is that the *same* engine — approval policy, identity, and an
immutable audit trail — sits behind every kind of change, so the record of
"what happened and who signed off" is complete and provable, not scattered
across tools.

## What Squadron does today

- **Multi-cloud discovery → AI-authored Terraform PR.** Read-only connect to
  AWS, GCP, Azure, or OCI; inventory what's un- or under-instrumented; open an
  HCL-aware, `terraform validate`-gated pull request against your IaC repo.
- **OpAMP fleet management.** Collectors register over OpAMP, report status and
  effective config, and appear live on a Fleet Map with pipeline, data-flow, and
  topology views.
- **Pipeline health from collector self-metrics.** Every agent gets a verdict
  (healthy / degraded / broken) with a plain-English signal list — no extra
  agents or scraping infrastructure.
- **AI-assisted config editing.** Explain, merge, and lint config changes, and
  emit whole multi-step cost-reduction plans as a single reversible arc. AI is
  off by default and BYO-key.
- **Safe rollouts.** Staged (percent or label), per-stage dwell, auto-abort on
  drift or error criteria, plus **N-of-M approvals** with rule-based approver
  roles — shipped in the open-source core.
- **Tamper-evident audit + offline verifier.** The audit log is a per-tenant
  hash chain; the bundled `squadron-audit-verify` CLI lets an auditor re-verify
  an exported chain **offline, with zero secrets**.
- **Cost optimization in dollars.** Project spend from observed ingest against
  your backend's per-GB rates, ranked by dollars saved.

See [What you get](https://github.com/devopsmike2/squadron#what-you-get) for the
full capability list.

## Who it's for

Squadron fits teams running OpenTelemetry collectors who want to see the fleet,
change it safely, and prove what happened — without standing up a dedicated
observability platform team. It's designed to work after one `docker run`, not
after a sales call.

It's **not** for you if you don't use OpenTelemetry, or if you want one tool to
be both your control plane *and* your telemetry backend — Squadron does the
first job and leaves the second to Honeycomb, Datadog, Tempo, Loki, or Mimir.

Teams that need SSO/RBAC, real multi-tenancy, cross-tenant tamper-evident audit,
or air-gapped deployment move up to **Squadron Enterprise**.

## Why it's open-core

The split is deliberate and stable:

- **Free, forever:** the breadth and the core loop — discovery, AI
  recommendations, Terraform PRs, the OTel fleet and rollout control plane,
  staged rollouts with N-of-M approvals, and the tamper-evident audit trail with
  its offline verifier.
- **Enterprise:** the depth that org-scale and regulated environments need —
  SSO/OIDC + SCIM, resource-aware and cluster/environment-scoped RBAC,
  per-tenant isolation, cross-tenant audit, self-hosted / **air-gapped**
  deployment with a bring-your-own or on-prem LLM, and support.

The rule of thumb: **breadth and the core loop stay open; depth, scale,
governance, and support are Enterprise.** See [OSS vs
Enterprise](https://github.com/devopsmike2/squadron#whats-oss-vs-enterprise) for
the line-by-line boundary.

## Where Squadron is heading

Today Squadron governs one kind of change — observability collector
configuration — end to end. The direction is to extend the *same* governed-change
engine to more of the high-stakes actions that happen inside an environment:

1. **Deeper config governance** for regulated and air-gapped fleets — the
   current focus: making every governance and compliance claim provably work,
   self-hosted, with no cloud dependency.
2. **Governing the instrumentation itself** — treating what telemetry is
   captured (and who turned it off) as an approval-gated, audited artifact,
   including zero-code/eBPF-instrumented collectors as a first-class governed
   source.
3. **Governed action beyond config** — applying the proposed → approved →
   enforced → recorded loop to other change surfaces (infrastructure changes,
   and eventually the actions autonomous agents take), so Squadron becomes the
   change-authority and immutable ledger for the whole regulated environment.

The throughline is consistent: one shared engine — tamper-evident ledger,
policy-enforced approval, identity — that every new surface reuses. Each step is
gated on the prior one being solid; nothing here softens the self-host / air-gap
posture, which is the point of the product.

## Next steps

- [Getting started](getting-started.md) — install and first run.
- [How Squadron works](how-it-works/index.md) — the loop in depth.
- [Security & the audit trail](how-it-works/security-and-audit.md) — the
  tamper-evident chain and offline verification.
- [Enterprise overview](enterprise/overview.md) — governance, RBAC,
  multi-tenancy, air-gap.
