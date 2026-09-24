# Positioning

Internal source-of-truth for how Squadron is described in
external surfaces (README, landing page, demo video, conference
talks, social posts). When external copy drifts from this, drift
this doc first.

> **Repositioned per ADR 0036 (2026-08-09), ratified.** Squadron
> now leads with **governance, compliance, and air-gapped
> deployability**, not telemetry cost or fleet size. The teardown
> established that the cost/fleet lane is crowded and priced toward
> zero (Edge Delta free at any scale; Bindplane acquired by
> Dynatrace), while the four-part governance bundle Squadron already
> ships is packaged by no surveyed competitor. Cost, fleet, and the
> discovery→AI-rec→Terraform loop are retained as the **free
> on-ramp and breadth, not the pitch.**

## Who Squadron is for

**Regulated and air-gapped operators who must produce
change-control and audit evidence for their observability
infrastructure, and can't buy a SaaS control plane to do it.**

- Utilities under NERC CIP, finance, healthcare (HIPAA), and
  gov/FedRAMP, anyone whose auditors ask "who changed this
  collector config, who approved it, and prove the record wasn't
  altered."
- Runs inside a regulated network: self-hosted or fully
  air-gapped, no outbound SaaS dependency permitted.
- Already carries a change-management and audit burden; wants
  tooling that *produces the evidence* instead of adding a second
  system to reconcile.
- Has a compliance budget and a formal evaluation process (this is
  a slower, higher-value, stickier sale than the SMB on-ramp).

The **Southern Company OpenShift pilot** (NERC CIP, air-gapped) is
the ready-made design partner and the path to a named reference.

**Who Squadron is also for (the free on-ramp, not the pitch):**
- Solo and small-team OTel operators who want AI-assisted config
  editing, cost insight, and safe rollouts for free. This is the
  breadth that gets Squadron installed and trusted, it is not what
  we lead with, and it feeds the governance sale over time.

**Who Squadron is NOT for:**
- Teams that don't use OpenTelemetry. (We don't translate from
  Fluentd, Logstash, or vendor-specific agents.)
- Operators who want Squadron to be their telemetry backend. (We
  ingest and explore; Honeycomb / Datadog / Tempo / Loki are the
  telemetry warehouse.)
- Teams whose only need is "cut my telemetry bill" with no
  governance or air-gap requirement, the free OSS core serves them
  well, but they are not the commercial ICP.

## The one-liner

> **An open-core governance & compliance control plane for
> observability infrastructure, approval-gated config rollout with
> a tamper-evident audit trail, deployable self-hosted or
> air-gapped inside a regulated network.**

Alternates by context:
- Short headline: **"Change control and tamper-evident audit for
  your OpenTelemetry collectors, air-gap ready."**
- Compliance-audience headline: **"Produce the change-management
  and audit evidence your auditors ask for, for the collectors you
  already run."**
- Technical-audience headline: **"Approval-gated collector rollout
  with an offline-verifiable, hash-chained audit trail."**

## The four things we lead with (the moat)

The uncontested bundle, no surveyed vendor packages all four. In
rough order of "what we say first":

1. **Approval-gated, change-windowed config rollout.** N-of-M
   multi-approver governance with per-group approver-role rules and
   change windows on every collector config change. (Found in zero
   surveyed products for collector config.)
2. **Tamper-evident, offline-verifiable audit.** Hash-chained,
   keyed (HMAC) audit of every config change and admin action, with
   an offline verifier CLI, the record proves it wasn't altered,
   without phoning home. (Also found in zero surveyed products.)
3. **Air-gapped / fully self-hostable control plane.** Runs inside
   a regulated network with no outbound SaaS dependency; the
   forthcoming offline install bundle ships the image, deps, and
   installer with zero public-registry egress.
4. **Open-core.** Apache-2.0 core, source-available enterprise; the
   plane is inspectable and self-hostable, the neutral-broker
   position an acquired incumbent can't hold.

## The compliance output IS the product

For this ICP, the deliverable is auditor-ready evidence, mappable
to real control frameworks:
- **NERC CIP** change-management (CIP-010) and access control.
- **SOC 2** CC-series change-management and audit-trail controls.
- **FedRAMP** AU (audit) and CM (configuration management)
  controls.

The audit attestation (hash-chain + offline verify) and the
change-control record (N-of-M approvals + change windows) emit that
evidence directly. That output is why a regulated buyer chooses
Squadron over a plain event-log control plane.

## The breadth we say after (the free on-ramp)

Retained, real, and free, the reason Squadron gets installed, but
not the marquee:

5. **Multi-cloud discovery → AI-rec → Terraform-PR loop.** Find
   collectors and telemetry infra across clouds; Claude explains
   every recommendation and merges snippets into existing configs;
   lint + diff + staged rollout catch mistakes before production.
6. **Cost insight in dollars, not bytes.** $/month per backend,
   fixes ranked by dollars saved, a genuine on-ramp, not the
   pitch.
7. **Safe rollouts with auto-abort, modern UX, self-instrumented.**
   Stages, dwell, abort criteria; Fleet Map, Cost Insights, ⌘K
   palette; Squadron emits its own OTel traces.

## Editions

- **OSS core (Apache-2.0, free forever):** the OTel control plane,
  discovery, AI-assisted config, cost insight, safe rollouts, and
  the *single-tenant* governance + tamper-evident audit primitives.
- **Squadron Enterprise (source-available, commercial):** the
  org-scale governance wedge, SSO/OIDC + SCIM, resource-aware
  RBAC, per-tenant multi-tenancy, cross-tenant tamper-evident audit,
  and N-of-M / policy-as-code approval governance ship today;
  air-gapped install bundle, multi-region HA, managed Postgres,
  validated scale beyond ~1,000 agents, and formal SOC 2 support are
  on the roadmap.

## What we explicitly do NOT claim

- "Replaces your observability backend", false. We're the control
  plane, not the telemetry warehouse.
- "Zero-trust multi-tenant enterprise platform (in OSS)", the OSS
  core is single-tenant; multi-tenancy, SSO/RBAC, and cross-tenant
  audit ship in Squadron Enterprise.
- "AI that automatically optimizes your fleet", every AI action is
  user-initiated; we never apply changes without an operator
  clicking through the rollout flow.
- "Certified compliant / SOC 2 certified", Squadron *produces
  evidence for* NERC CIP / SOC 2 / FedRAMP control families; it is
  not itself a certification, and formal SOC 2 support is on the
  roadmap. Say "maps to" / "produces evidence for," never
  "certified."
- "No competitor has tamper-evident audit", the accurate claim is
  that *no surveyed vendor markets/packages the four-part bundle*;
  absence of public documentation is not proof of absence. Don't
  stake the whole pitch on one competitor's roadmap.
- "Drop-in enterprise replacement (from OSS alone)", org-scale
  readiness is Squadron Enterprise, not the free core.

## Tone

- Engineer-to-engineer, and auditor-credible. Read like a senior
  platform engineer who has survived a NERC CIP audit recommending
  a tool to a peer, not a sales page.
- Concrete about evidence and controls ("emits a hash-chained
  record mappable to CIP-010" beats "compliance-ready").
- Honest about gaps. The "What's NOT in v0.X" sections are the
  model; for compliance buyers, over-claiming certification is
  disqualifying.
- Avoid buzzwords: no "harness the power of AI", no "synergize", no
  "next-generation cloud-native observability stack".
- Skip emojis unless asked. We're an infrastructure tool for
  regulated operators.

## Audience-specific framings

| Audience | Open with | Avoid |
|---|---|---|
| **Regulated / compliance buyer** | Change-control + tamper-evident audit + air-gap; the control-family mapping | Cost-first framing; "certified" |
| **Hacker News** | Open-core + the technical depth of hash-chained offline-verifiable audit + AI-as-tool | Marketing copy, growth-hacky CTAs |
| **r/devops** | Safe approval-gated rollouts + 5-min setup + free breadth | Enterprise speak |
| **CNCF Slack** | OTel-native + OpAMP correctness + neutral open-core plane | Anything vendor-locked |
| **Conference CFPs** | The non-obvious insight (making AI-merged YAML safe via lint+staged-rollout; offline-verifiable audit) | Feature-list talks |

## Frequently asked questions (FAQ)

**Q: How is this different from Bindplane?**
A: Squadron packages the governance + compliance bundle Bindplane
doesn't, true N-of-M multi-approver governance on collector config
and a tamper-evident, offline-verifiable audit of that config,
deployable fully air-gapped, open-core. Bindplane has staged
rollout and a plain event-log audit, a larger curated processor
library, and is now Dynatrace-owned. If you're a regulated operator
who must produce change-control and audit evidence inside an
air-gapped network, Squadron is built for exactly that. The
AI-assisted config, cost insight, and modern UX come along for free.

**Q: Do you store my telemetry?**
A: Squadron has a built-in OTLP receiver + store, mostly to power
Cost Insights. Your collectors can continue shipping straight to
your real backend; Squadron just needs to see something to compute
costs. Stored data stays on your box; we never phone home, which
is the point for an air-gapped deployment.

**Q: What does the AI feature send to Anthropic (or your provider)?**
A: Only when you click an AI button. Content is documented
per-action in `docs/ai-assist.md`: YAML snippets, config contents
(for Explain Config), short context strings. No API keys, no agent
labels, no telemetry data, no audit log. AI is off by default; you
opt in by setting a provider key (in `.env`, an env var, or the UI
Settings → AI field, sealed and write-only). Multi-provider:
Anthropic, OpenAI-compatible, Azure/Gemini/Mistral, or a local
model via base-URL, so an air-gapped site can point at an on-prem
LLM and keep everything inside the network.

**Q: Does Squadron need an AI key to work?**
A: No. Every AI feature is optional; without a key the AI
affordances are hidden and everything else, rollouts, audit,
approvals, discovery, cost, works normally.

**Q: Can I run this fully air-gapped?**
A: Yes, that's a lead use case. Self-hostable today; the offline
install bundle (prebuilt image + vendored deps + installer, zero
public-registry egress) is on the near-term roadmap, hardened
against the exact friction the Southern pilot surfaced.

**Q: Is Squadron "compliant" / SOC 2 certified?**
A: Squadron *produces evidence for* NERC CIP (CIP-010 change
management), SOC 2 CC-series, and FedRAMP AU/CM control families,
the hash-chained audit and N-of-M change record are auditor-ready
output. Squadron itself is not a certification, and formal SOC 2
support is on the Enterprise roadmap.

**Q: What's the commercial story?**
A: The OSS core is Apache-2.0 and free forever, including the
single-tenant governance + tamper-evident audit primitives.
Squadron Enterprise is the commercial tier for org-scale
governance: SSO/OIDC + SCIM, resource-aware RBAC, per-tenant
multi-tenancy, cross-tenant tamper-evident audit, and policy-as-code
approvals today; air-gapped bundle, multi-region HA, managed
Postgres, validated scale beyond ~1,000 agents, and SOC 2 on the
roadmap.

**Q: How big a fleet has Squadron been tested at?**
A: 1,000 agents on a single instance, numbers in
`docs/scale-testing.md`. Validated scale past ~1,000 agents is on
the Enterprise roadmap; at 5k+ that's the Enterprise conversation.

## Don't say

- "Revolutionary"
- "Best-in-class" (let operators decide)
- "Certified" / "SOC 2 certified" (we produce evidence; we are not
  a certification)
- "Enterprise-grade" (in OSS copy, org-scale features live in
  Squadron Enterprise)
- "Harness the power of AI"
- "Next-generation"
- "Magical / magic / 🪄"
- "Unlock" (as in "unlock value")
- "Painless" (operators have a high bar)
- "Solution" (it's a tool)
- "Cloud-native" (the word means nothing)
