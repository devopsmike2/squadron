# Squadron OSS — Working-Features Audit

_Audit date: 2026-06-29 · Against `main` @ v0.89.289 (running stack confirmed at this rev)._

Purpose: an honest, confidence-tiered map of what works in the OSS build so we can point
engineers at the parts that give a good first impression — and set expectations on the
parts that are partial or opt-in.

Tiers: **Verified live** = exercised end-to-end against real infrastructure in recent
testing. **Works** = covered by tests and/or prior live runs, not re-verified head-to-toe
in the latest pass. **Partial / honest limits** = runs but has documented gaps.
**Opt-in / setup-gated** = needs a key or external resource first.

---

## TL;DR — what to tell engineers

Two strong, demoable stories:

1. **Cloud discovery -> AI remediation -> real GitHub PR.** Connect a cloud account
   (AWS / GCP / Azure / OCI), scan it, get AI recommendations for un-instrumented or
   under-observed resources, and have Squadron open a merge-ready Terraform PR against
   your IaC repo. Most thoroughly live-verified path.
2. **OTel fleet control plane.** Agents connect over OpAMP, appear in Fleet, and receive
   config via staged rollouts with auto-abort; the config editor has AI Assist + lint +
   diff preview; the Savings / Cost-Insights dashboards project spend in dollars.

**Fastest credible first run (no cloud account):** start the container, enable Demo mode,
walk Discovery -> Recommendations -> (simulated) PR. Then connect one cloud read-only for
the real thing.

---

## 1) Verified live (high confidence)

- **Multi-cloud discovery scan — AWS, GCP, Azure, OCI.** All four connectors validated,
  scanned real accounts, inventory checked against an independent oracle. Tiers: compute,
  databases, Kubernetes, serverless, object stores, load balancers, event sources,
  orchestration (coverage varies per cloud — see section 3).
- **AI recommendations via the real LLM.** Ranked, plain-English recs with merge-ready
  Terraform (e.g. EC2 Graviton, OCI Object Storage access-logging). Async; needs key.
- **IaC GitHub remediation loop.** Connect a repo, Squadron opens a real merge-ready
  Terraform PR (HCL-aware merge + `terraform validate` gate); merge/close webhook feeds
  verdict learning (decline -> future citation). Real PRs opened + merged.
- **env -> Terraform import blocks.** Generates `import {}` blocks for un-managed
  resources and opens a PR (OCI verified live to a real PR).
- **OTel agent deploy + OpAMP -> Fleet.** Deployed collector auto-registers and shows
  online in Fleet; config injection wired.
- **OCI object-store + load-balancer observability detection** verified against a real
  tenancy (covered vs uncovered correct).
- **Demo mode** — seeds inventory + short-circuits scan/recs so the full Discovery ->
  Recommendations flow demos with no cloud account (all four clouds).
- **First-run onboarding** — prod-like single-port image (:8080) serving UI + API; new-
  user happy path validated, early friction fixed.

## 2) Works (test-covered / previously verified live)

Worth a quick smoke test before a high-stakes demo.

- **Quickstart wizard** — start-fresh + adopt-existing paths; bulk per-host one-liners.
- **Config editor** — Monaco + AI Assist (Explain / Merge snippet), Squadron Lint, diff
  preview, live pipeline view.
- **Staged rollouts** — percent/label stages, dwell, abort criteria, auto-abort.
- **Fleet map / Agents / Groups** — agent overview, grouping, group config + restart.
- **Savings + Cost Insights** — $/month projection from ingest x backend rates; Quick
  Wins ranked by $ saved with one-click Apply.
- **Alerts** — rule CRUD + evaluator loop.
- **Audit log + AI explain** — per-event narration (opt-in AI).
- **Incidents drafter** — AI-drafted summaries (opt-in AI).
- **Continuous discovery** — scan persistence, history, scheduled re-scans, drift (opt-in).
- **Cross-cloud verdict learning** (opt-in flag).
- **Recommendations exclusion + placement-map** — placement now covers AWS + GCP/Azure/OCI
  core tiers + the four event-source surfaces + SQS (as of v0.89.289).

## 3) Partial / honest limitations

- **Detection coverage is not uniform.** Some axes are real metric-backed detection;
  others are honest-deferred or proxy-based. Authoritative matrix: `docs/detection-coverage.md`.
  Known deferrals: AWS Lambda + Azure Functions cold-start need Lambda Insights /
  Application Insights; OCI queue poison-rate has no native metric.
- **Event-source recommendation breadth.** Classification + placement cover EventBridge,
  Pub/Sub, Service Bus, OCI Streaming, SQS. The longer tail (SNS-with-TF, Cloud Tasks,
  Event Grid, Event Hubs, Pub/Sub Lite, ONS, Queues) + serverless/orchestration kinds are
  incremental adds.
- **Cost projection accuracy depends on configured backend rates** — directional.
- **Recommendation quality is LLM-dependent.** Deterministic Terraform snippets are
  audited; free-form LLM reasoning should be reviewed before merge (by design — every fix
  is a PR gated by your review + CI).

## 4) Opt-in / setup-gated

- **AI features off by default** — set `ANTHROPIC_API_KEY` for Explain / Merge / recs /
  incident drafting.
- **Cloud discovery needs read-only creds** per cloud (`docs/discovery-*-first-time-setup.md`).
  OCI object-store/LB detection also needs `read log-groups`.
- **IaC PRs need a GitHub connection** (PAT + repo); merge-learning needs a reachable
  listener URL + webhook secret (global or per-connection).

---

## Recommended engineer try-it path

1. **Start it.** `docker run -d -p 8080:8080 -p 4320:4320 -p 4317:4317 -p 4318:4318 -v squadron-data:/app/data ghcr.io/devopsmike2/squadron:latest` then open `http://localhost:8080/quickstart`.
2. **No-cloud demo (2 min).** Enable Demo mode -> Discovery -> pick a cloud -> seeded
   inventory + recs -> open a simulated PR.
3. **Real cloud scan (10 min).** Connect one cloud read-only (AWS most battle-tested),
   scan, browse inventory tabs, set `ANTHROPIC_API_KEY`, generate recs.
4. **Close the loop (optional).** Connect a test GitHub repo, set a placement path, open a
   real Terraform PR from a recommendation.
5. **Fleet path (optional).** Point a collector at :4317/:4318 (OTLP) + :4320 (OpAMP),
   watch it appear in Fleet, push a config via a staged rollout.

## Honesty notes for the pitch

- Lead with the discovery -> AI PR loop and the fleet/rollout control plane — the verified,
  differentiated stories.
- Don't claim uniform detection depth across every tier/cloud; cite the coverage matrix.
- Frame Squadron as orchestrator, not executor: it opens PRs and stages rollouts; it never
  runs `terraform apply` or pushes to a branch unattended. True, and a feature.

---

## Addendum — "Works" tier live smoke-test (2026-06-29, v0.89.290)

API-level smoke test of every "Works"-tier surface against the running stack
(~1000 agents, 21h uptime). All endpoints returned 200 with sane payloads:
agents, configs, groups, topology (+ per-agent), alerts/rules, rollouts +
rollout-recipes (templates/abort-criteria), audit/events, insights/volume
(+ per-agent), recommendations (+ per-agent + dismissals), pricing/config,
savings/realized, alerts/cost-spikes, incidents/drafts, quickstart
(backends/opamp-snippet), pipeline-health (fleet + per-agent).

**One real bug found and fixed (v0.89.290): `pipeline-health/fleet` hung
~18s and returned nothing** on a long-running fleet — its window-function
scanned the entire (GC-less, ever-growing) `pipeline_health_samples` table.
Now time-bounded to a 1h freshness window (constant cost w.r.t. uptime);
verified 18s -> 0.14s, 200. With that fix, the fleet/agents/pipeline-health
surfaces move from **Works** to **Verified live**.

Remaining check for pixel-level demo confidence: a browser pass of the headline
visual pages (Savings, Fleet/Pipeline Health, Config editor) — the APIs behind
them are green; the rendering itself was not re-walked in this pass.

Follow-up (not demo-blocking): `pipeline_health_samples` has no retention GC,
so it grows on disk; add a sweep loop like the webhook dedupe GC.
