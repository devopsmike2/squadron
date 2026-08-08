# Positioning

Internal source-of-truth for how Squadron is described in
external surfaces (README, landing page, demo video, conference
talks, social posts). When external copy drifts from this, drift
this doc first.

## Who Squadron is for

**Solo and small-team operators running OpenTelemetry collectors
who think their cloud bill is too high and don't have an
observability team to fix it.**

- 1–3 engineers wearing many hats
- 10–200 collectors in production
- Telemetry going to a SaaS backend (Datadog, Honeycomb, New
  Relic, Grafana Cloud, SigNoz, or similar)
- Bill is painful enough that the CFO has noticed
- No formal procurement process; downloads and tries OSS tools

**Who the OSS core is NOT for (today):**
- Teams that need SSO/RBAC, multi-tenancy, or tamper-evident
  cross-tenant audit — or that are heading toward multi-region HA
  and SOC 2. That buyer is served by **Squadron Enterprise**:
  SSO/OIDC + SCIM, resource-aware RBAC, per-tenant isolation, and
  tamper-evident audit ship today; multi-region HA and formal
  SOC 2 support are on the Enterprise roadmap. (Route them to
  Enterprise, not a third-party tool.)
- Teams that don't use OpenTelemetry. (We don't translate from
  Fluentd, Logstash, or vendor-specific agents.)
- Operators who want Squadron to also be their telemetry backend.
  (We ingest and explore, but Honeycomb / Datadog / Tempo are far
  better tools for serious observability.)

## The one-liner

> **The open-source OpenTelemetry control plane that pays for
> itself.**

Alternates by context:
- Short headline: **"Cut your OpenTelemetry bill — without
  changing your backend."**
- Technical-audience headline: **"AI-assisted cost optimization
  for your OpenTelemetry collectors."**
- Developer-friendly: **"OTel control plane that ships with a
  CFO mode."**

## The three things we lead with

In rough order of "what we say first" on any new surface:

1. **Cost optimization in dollars, not bytes.** Squadron projects
   your $/month spend per backend, ranks fixes by $ saved, and
   one-clicks you into the rollout flow that applies them.
2. **AI-assisted config editing.** Claude explains every
   recommendation in plain English and merges suggested snippets
   into your existing collector configs. Lint + diff + staged
   rollout catch the LLM's mistakes before they reach production.
3. **Minutes to first agent.** The Quickstart wizard generates
   the collector config for your backend (Datadog/Honeycomb/etc.)
   AND the install command, OR generates the OpAMP snippet to
   paste into your existing collector configs. Bulk-mode handles
   adoption across a hostname list.

## The three things we say after

When operators are still reading:

4. **Safe rollouts with auto-abort.** Stages, dwell, abort
   criteria (drift, drop rate, error logs). The grown-up
   deployment story shipped as OSS.
5. **Modern UX.** Fleet Map, Cost Insights, real-time
   recommendation panel, ⌘K palette. Most competitors run on
   2018-era admin UIs.
6. **Self-instrumented.** Squadron's own audit events, rollout
   engine, AI service emit OTel traces. Operators can debug
   Squadron with the same tools they'd use for anything else.

## What we explicitly do NOT claim

- "Replaces your observability backend" — false. We're the
  control plane, not the telemetry warehouse.
- "Zero-trust multi-tenant enterprise platform (in OSS)" — the OSS
  core is single-tenant; multi-tenancy ships in Squadron Enterprise.
  SOC 2 support is on the roadmap. Don't oversell the OSS tier as
  the enterprise one.
- "AI that automatically optimizes your fleet" — every AI action
  is user-initiated. We never apply changes without an operator
  clicking through the rollout flow.
- "Drop-in enterprise replacement (from OSS alone)" — the OSS core
  isn't the enterprise tier. Org-scale readiness — SSO/RBAC,
  multi-tenancy, and tamper-evident audit today; multi-region HA,
  validated scale beyond ~1,000 agents, and SOC 2 on the roadmap —
  is **Squadron Enterprise**. Operators evaluating at 5k-agent
  scale should know that's the Enterprise path, and that scale
  past ~1,000 agents is still being validated.

## Tone

- Engineer-to-engineer. Read like a senior engineer recommending
  a tool to a peer, not like a sales page.
- Concrete numbers wherever possible ("$33/month saved by
  dropping the `processor` attribute" beats "significant savings").
- Honest about gaps. Acknowledging what we don't do builds
  trust. The "What's NOT in v0.X" sections in every docs page
  are the model.
- Light irreverence is fine. Avoid corporate buzzwords:
  no "harness the power of AI", no "synergize", no "platform
  for the next-generation cloud-native observability stack".
- Skip emojis unless asked. We're a developer tool.

## Audience-specific framings

When writing for a specific channel, lead with the angle that
audience cares about most:

| Audience | Open with | Avoid |
|---|---|---|
| **Hacker News** | OSS + technical depth + AI-as-tool not magic | Marketing copy, growth-hacky CTAs |
| **r/devops** | The cost-pain story + concrete savings + 5-min setup | Enterprise speak |
| **CNCF Slack** | OTel-native + OpAMP correctness + community fit | Anything that sounds vendor-locked |
| **Twitter/X** | One-line value claim + short demo gif | Long threads, "🧵" formatting |
| **Conference CFPs** | The non-obvious technical insight (e.g. how we made AI-merged YAML safe via lint+staged-rollout) | Talks that are just feature lists |

## Frequently asked questions (FAQ)

The questions to expect when operators evaluate Squadron, and the
honest answers we use.

**Q: How is this different from Bindplane?**
A: Squadron is the OSS-first, OTel-native option — AI-assisted,
cost-first dashboards, modern UX, minutes to set up, and free for
any fleet size. Bindplane leans on a larger curated processor
library and a longer enterprise track record. If you're a small
team with a painful telemetry bill, Squadron will probably feel
more like "the tool I wanted." When you need org-scale governance —
SSO/RBAC, multi-tenancy, and tamper-evident audit today,
multi-region HA and SOC 2 on the roadmap — that's Squadron
Enterprise, not a third-party tool.

**Q: Do you store my telemetry?**
A: Yes — Squadron has a built-in OTLP receiver + DuckDB store,
mostly to power Cost Insights (we need to see what bytes are
flowing where). But your collectors can also continue to ship
straight to your real backend (Datadog/Honeycomb/etc.) — Squadron
just needs to see SOMETHING to compute costs. The data Squadron
stores stays on your box; we never phone home.

**Q: What does the AI feature send to Anthropic?**
A: Only when you click an AI button. The content is documented
per-action in `docs/ai-assist.md`: YAML snippets, your config
contents (for the Explain Config feature), and short context
strings. No API keys, no agent labels, no telemetry data, no
audit log. The AI is off by default; you opt in by setting
`ANTHROPIC_API_KEY`.

**Q: Does Squadron need an Anthropic API key to work?**
A: No. Every AI feature is optional. Without a key, the AI
affordances are hidden in the UI and everything else works
normally. The Quickstart wizard, Cost Insights, Savings
dashboard, Recommendations, and Rollouts all work fine without
AI.

**Q: How accurate are the "$/month" projections?**
A: They're estimates from observed bytes × configured per-GB
rates. The default rates are conservative starters for the major
backends; for accurate projections, tune the rates in
`squadron.yaml` against your actual invoice. The Savings page
shows every rate in the assumptions footer.

**Q: Will this replace my observability backend?**
A: No, and you shouldn't want it to. Squadron is the control
plane (managing your collectors, optimizing what they send).
Datadog / Honeycomb / Loki / Tempo are still your telemetry
warehouse and query UI. The two roles complement each other.

**Q: What's the commercial story?**
A: The OSS core is Apache 2.0 and stays free. Squadron Enterprise
is the commercial tier for org-scale readiness: SSO/OIDC + SCIM,
resource-aware RBAC, per-tenant multi-tenancy, and tamper-evident
cross-tenant audit ship today; multi-region HA, a managed Postgres
store, validated scale beyond ~1,000 agents, and formal SOC 2
support are on the roadmap. That's what enterprise buyers ask for
and SMB operators don't need — the SMB buyer is well-served by the
free OSS forever.

**Q: How big a fleet has Squadron been tested at?**
A: We've validated 1,000 agents on a single Squadron instance —
documented numbers in `docs/scale-testing.md`. Beyond that we
don't have published data yet; validated scale past ~1,000 agents
is on the Squadron Enterprise roadmap. If you're evaluating at
5k+ agents, that's the Enterprise conversation, not a third-party
tool.

## Don't say

Words and phrases that should not appear in Squadron marketing
copy. They're either inaccurate, buzzword-filled, or condescending.

- "Revolutionary"
- "Best-in-class" (let operators decide)
- "Enterprise-grade" (in OSS copy — don't brand the free OSS tier
  as enterprise; org-scale features live in Squadron Enterprise)
- "Harness the power of AI"
- "Next-generation"
- "Magical / magic / 🪄"
- "Unlock" (as in "unlock value")
- "Painless" (operators have a high bar for what's painless)
- "Solution" (it's a tool, not a solution)
- "Cloud-native" (everything's cloud-native; the word means nothing)
