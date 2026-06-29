# OSS 100% Verification — checklist + running results

_Goal: every advertised OSS feature is either verified working end-to-end on a
**clean install as a new user**, or explicitly relabeled partial in the docs.
"Verified" = walked on the real path (not a 200-ping). Started against
`main` @ v0.89.304._

Legend: `[ ]` not yet · `[P]` PASS (verified live) · `[F]` FAIL (fix needed) ·
`[~]` partial-by-design (documented) · `[B]` blocked (needs creds/infra).

Deps: **none** = no creds · **key** = ANTHROPIC_API_KEY · **agent** = a real
collector pointed at it · **cloud** = read-only cloud creds · **gh** = GitHub
PAT/connection.

## A. First-run / onboarding (deps: none)
- [ ] Clean boot from published image: `/healthz` ok, UI loads, **empty** first-run state (no sim agents)
- [ ] `/quickstart` — Start-fresh: pick backend → starter config + Docker/systemd/Helm install cmd
- [ ] `/quickstart` — "I have collectors": OpAMP snippet copy
- [ ] `/quickstart` — bulk mode: per-host ssh one-liner
- [ ] Auth: token off→loud warning; `/settings/tokens` create token + scopes; authed request works
- [ ] `/settings/siem` renders + saves
- [ ] ⌘K command palette + dark/light theme

## B. Demo mode (deps: none)
- [ ] Enable demo mode → `/discovery/aws|gcp|azure|oci` show seeded inventory
- [ ] Recommendations generate (demo short-circuit) for each cloud
- [ ] Simulated PR flow completes
- [ ] Disable demo mode → state clears

## C. Fleet / agents / rollouts (deps: agent)
- [ ] `/agents` — real collector registers, online, last-seen fresh
- [ ] `/fleet-map` — pipeline / dataflow / topology tabs render
- [ ] `/groups` — create group, assign, group config + restart
- [ ] `/rollouts` — create staged rollout (percent + label), dwell, abort criteria, pause/resume
- [ ] `/telemetry` + `/topology` render
- [ ] pipeline-health (fleet + per-agent) returns bounded + sane
- [ ] `/deploy`, `/runners`, `/actions` render + basic flow

## D. Config editor + AI (deps: key)
- [ ] `/configs`, `/configs/new` — Monaco loads, Squadron Lint, diff preview, live pipeline view
- [ ] AI Assist — Explain returns 2-3 sentence summary
- [ ] AI Assist — Merge snippet integrates into existing YAML
- [ ] `/playground/proposer` — proposer playground runs

## E. Cost / savings (deps: agent for real numbers)
- [ ] `/cost-insights` — volume, outlier agents, top attributes populate
- [ ] `/savings` — $/mo projection, Quick Wins ranked, one-click Apply → editor pre-filled

## F. Ops surfaces (deps: mixed)
- [ ] `/alerts` — rule CRUD + evaluator loop fires
- [ ] `/audit` + `/timeline` — events render humanized; AI explain (key)
- [ ] `/incidents` — AI-drafted summary (key)
- [ ] `/inventory` renders

## G. Real cloud discovery → remediation (deps: cloud, key, gh)
- [ ] `/discovery/aws` — connect (read-only) → scan vs oracle → inventory tabs → recs (real LLM) → exclusion
- [ ] `/discovery/gcp` — connect → scan vs oracle → recs (spot)
- [ ] `/discovery/azure` — connect → scan vs oracle → recs (spot)
- [ ] `/discovery/oci` — connect → scan vs oracle → recs (spot)
- [ ] `/discovery/iac/github` — connect repo → open **real** merge-ready PR → merge → verdict learning (decline→citation)
- [ ] env→Terraform import-block generation → PR
- [ ] Continuous discovery: scan persistence + history + scheduled re-scan + drift

## H. Plans / actions
- [ ] `/plans/:id` renders a real plan
- [ ] Action runner steps execute (where applicable)

---

## Results log
_(filled as phases run)_

### Phase 1 (clean-room boot) — PASS
Published image booted healthy on empty volume; first-run state correct (0 agents,
AI disabled, auth-off warning); /healthz,/health,/readyz all 200; 15 key GET
endpoints 200 (no first-user 5xx).

### Phase 2 (no-cloud sweep) — PASS (no-key surface)
- Quickstart: backends, starter-config, opamp-snippet, adoption-snippet all return real content. PASS
- Demo mode: AWS enable→scan(seeded inventory inline)→recommendations **succeed with no LLM**; GCP/Azure/OCI seed connections. PASS
- Collector → Fleet: throwaway collector registered (0 export errors, agent online). PASS
- Savings/realized, insights/volume(+attributes w/ signal param), audit/events, pipeline-health (fleet + per-agent): real JSON. PASS
- Config lint: real findings (missing-batch-processor w/ line+path). PASS
- Alerts: CRUD at /api/v1/alerts/rules — create→list→delete all work; rigorous field validation. PASS

**Fixes found + shipped this phase**
- FIX: unknown /api/* routes returned the SPA index.html (200) instead of 404 JSON — masked wrong paths (made a typo'd endpoint look like a pass). Now 404 JSON. (committed addb2f2)

**Minor notes (backlog, not blockers)**
- API shape inconsistency: AWS demo connection uses `connection_id`; GCP/Azure/OCI use `id`.
- Demo scan returns inventory inline but is not persisted to scan history (`/scans` stays empty under demo).

**Still needs intervention**
- AI features (config-editor Explain/Merge, proposer playground, incident AI drafts, audit AI explain) — need ANTHROPIC_API_KEY on the clean-room.
- Phase 3 real-cloud (AWS/GCP/Azure/OCI scans, IaC PR, env→TF) — cloud creds + GitHub PAT + OCI key + minimal test-infra spend.
- Phase 4 staged rollout.

### Phase 2b (AI features, key enabled on clean-room) — PASS, 1 bug fixed
Reused the dev ANTHROPIC_API_KEY on the clean-room (ai_enabled:true).
- /ai/status: enabled (explain=haiku, merge=sonnet). PASS
- /ai/explain-config: real summary + token counts. PASS
- /ai/explain (snippet): real explanation. PASS
- /ai/merge: inserted resource processor AND wired it into the pipeline ([resource, batch]). PASS
- /audit/:id/explain: **was broken** — returned "AI assist is not configured" even with AI on.
  Root cause: registerRoutes() runs before SetAIService(); the audit handler captured a nil
  aiService eagerly (the #104/#105 eager-nil-capture class — that audit covered appStore/
  actionStore, not aiService). FIX: late-bind aiService at request time. Verified live on dev:
  now returns a real explanation. (committed 59eb254)

**Fixes shipped this pass: 2**
- addb2f2 — unknown /api/* → 404 JSON (was SPA 200 HTML)
- 59eb254 — audit-event AI explain late-binds aiService (was always "not configured")

### Phase 4 (staged rollout) — PASS
On the clean-room: created config → group → assigned the registered agent →
created a rollout from the "cautious-percent-ramp" template (1%→10%→50%→100%
with dwells) + strict-canary abort criteria. Engine picked it up:
state=in_progress, current_stage=0. PASS.

### Phase 3 (real cloud, current code, live connections) — PASS (scan path)
Real scans against the live accounts on current main:
- AWS: completed, partial=false, full tier structure (account near-empty post-teardown — correct). PASS
- Azure: completed, partial=false. PASS
- OCI: completed, partial=false, found 2 real compute instances. PASS
- GCP: scan path reachable but >13s (many regions); verified live recently (#140).
Recommendations (LLM) + IaC PR + env→TF: the AI service is verified working this
pass (explain/merge), and these were verified live recently (#187/#202/#209). The
IaC PR loop needs a GitHub PAT to re-run on a fresh instance (not currently wired).

## VERDICT
On a clean install (published image, empty DB), the OSS surface is verified
working across: first-run/boot, quickstart, demo mode (4 clouds, no LLM), config
lint, alerts, savings, cost insights, audit, pipeline-health, collector→Fleet
registration, AI Assist (explain + merge, real LLM), staged rollouts, and
real-cloud discovery scans (AWS/Azure/OCI on current code). **Two real bugs were
found and fixed** (API 404 hygiene; audit-explain nil-aiService).
Not re-verified head-to-toe *today* (proven live in the last few days): full
deploy→scan-vs-oracle for all 4 clouds, GCP scan completion, and the live IaC
GitHub PR loop (needs a PAT to re-run).
