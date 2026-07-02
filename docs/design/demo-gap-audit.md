# Demo Gap Audit — every claimed capability vs. what the demo actually shows

Status: **for sign-off** · Owner: Squadron · Date: 2026-07-02

> Purpose. Before building anything, enumerate every capability Squadron
> claims, verify what the code actually does, and state exactly what the
> one-click demo shows for it today — fully, partially, or nothing — plus the
> precise fix to reach full-capability. This is the gate. Nothing gets built
> until the target state below is signed off.

---

## 0. Verdict in one paragraph

**The product is real; the demo is a facade over it.** Every headline
capability — fleet management, per-agent config/logs/metrics/traces, cost
insights, savings, the cost-spike→AI-proposal→rollout→action→incident loop,
multi-cloud discovery→recommendations→Terraform, alerts, timeline, audit — is
implemented and production-grade in the codebase. But the one-click demo seeds
**exactly four rows** (one group, one config, one agent, one cost spike) and
**zero telemetry**. The result: roughly a third of the surface is convincingly
alive, a third renders empty panels ($0/mo, no logs, no metrics, blank
charts), and the flagship AI loop is dark unless you supply an Anthropic API
key. A first-time operator sees a well-designed shell, not a working platform.

**The good news, verified in code:** the machinery to make Squadron genuinely
*alive* already exists in-tree. `cmd/fleetsim` drives a live OpAMP fleet of
1000+ synthetic agents; `cmd/otlpsim` pushes real OTLP metrics/logs/traces
into the ingest path, and — by design — the two compose on matching
deterministic agent IDs (`otlpsim/main.go` header: "Run fleetsim for a live
OpAMP fleet and otlpsim for its telemetry and the data attributes to the same
simulated agents"). We are not building a simulator from scratch. We are
**wiring simulators that already exist into the demo path** and filling the
seed gaps that don't need a simulator.

---

## 1. What the demo seeds today (ground truth, code-verified)

`internal/demoseed/demoseed.go`, `Seed()`:

| Row | Store call | Result |
|---|---|---|
| 1 group `demo-web-prod` (require_approval=true) | `CreateGroup` | ✅ |
| 1 config `cfg-demo-web-prod-baseline` (hashing.rounds=12) | `CreateConfig` | ✅ |
| 1 agent `demo-web-canary-1` (online, effective_config set) | `CreateAgent` | ✅ |
| 1 cost spike `spike-demo-*` (+312%, critical) | `CreateCostSpikeEvent` | ✅ |

Plus `POST /demo/enable` registers the demo AWS discovery connection so
discovery scan/recs short-circuit to canned data (`internal/discovery/demo`).

**Not seeded, and therefore empty in the UI:** any OTLP telemetry (DuckDB
`metrics_sum`/`metrics_gauge`/`logs`/`traces`/`otlp_batches`), any rollout, any
action/runner, any incident draft, any alert rule, any audit/timeline trail
beyond the single cost-spike event. The demo-collector container that *does*
run exports to a `debug` sink (`examples/collector/demo-collector.yaml` lines
41/45/49), so it produces nothing Squadron can ingest.

---

## 2. The gap matrix

Legend: ● full · ◐ partial · ○ empty/dark. "Fix class" maps to the build plan
in §4: **[T]** telemetry ingest, **[F]** fleet scale-up, **[S]** direct seed,
**[AI]** AI fallback, **[PR]** discovery/IaC parity, **[U]** UI polish.

| Surface | Claimed | Real in code | Demo today | Gap | Fix |
|---|---|---|---|---|---|
| Dashboard — fleet size/status/drift | Mission-control glance | ✅ | ● (1 agent) | Trivial fleet | F |
| Dashboard — cost spike banner + $ | Bytes & $ at a glance | ✅ (DuckDB-derived) | ○ $0 | No telemetry | T |
| Dashboard — recent activity | Audit stream | ✅ | ○ | No audit trail seeded | S |
| Dashboard — fleet health sparklines | Queue/drops trend | ✅ | ○ | No self-metrics | T |
| Agents — paginated/virtualized list | 200+ rows, filters | ✅ | ◐ (1 row) | Nothing to page/filter | F |
| Agents — drift/status filters | Show drifted/offline % | ✅ | ◐ (all synced/online) | No variety | F |
| Agent detail — Overview | Metadata + volume + health + recs | ✅ | ◐ (meta only; $0, no health, no recs) | No telemetry | T |
| Agent detail — **Config** | Effective vs intended + pipeline DAG + send | ✅ | ● | **none — ships today** | — |
| Agent detail — **Logs** | Query/filter/search agent logs | ✅ | ○ | DuckDB `logs` empty | T |
| Agent detail — **Metrics** | Time-series charts per metric | ✅ | ○ | DuckDB metrics empty | T |
| Agent detail — Traces | Spans per agent | ✅ | ○ | DuckDB traces empty | T |
| Fleet Map — Pipeline | Per-agent collector DAG | ✅ | ◐ (1 agent) | Weak at n=1 | F |
| Fleet Map — Data Flow | Exporter endpoints + $/dest | ✅ | ◐ (endpoint stub, $0) | No byte volume | T |
| Fleet Map — Fleet topology | Agents×groups graph | ✅ | ◐ (1:1) | Weak at n=1 | F |
| Groups — list/CRUD/policy | Multi-group management | ✅ | ◐ (1 group) | Weak at n=1 | F/S |
| Telemetry — SquadronQL explorer | Ad-hoc SQL over telemetry | ✅ (UI works) | ○ (0 rows) | No telemetry | T |
| Configs — editor + versioning + lint | Monaco + versions + lint | ✅ (versions via audit) | ◐ (1 version; no history UI; lint at rollout time) | Version UI + inline lint | S/U |
| Configs — AI Explain / Merge | Plain-English + snippet merge | ✅ | ○ (503 without key) | LLM-gated | AI |
| Rollouts — staged canary | Stages/guardrails/approval/rollback | ✅ (full state machine) | ◐ (only if proposer fires; then approval works, stages stall) | No mid-flight rollout; no telemetry to advance | S/AI/T |
| **AI Proposer** — spike→draft | Cost spike → drafted rollout | ✅ | ○ **dark without ANTHROPIC_API_KEY** | Bridge no-ops; no fallback | AI |
| Actions / Runners | Signed dispatch + runner exec + audit | ✅ (full API+engine) | ○ (nothing seeded) | Loop never exercised | S |
| Cost Insights — volume/outliers/attrs | Where bytes go, by agent/attr | ✅ (DuckDB-derived) | ○ | No telemetry | T |
| Savings — $ spend + quick wins | $/mo + ranked recs w/ Apply | ✅ (5 recipes, derived) | ○ ($0, no recs) | No telemetry to derive from | T |
| Incidents — drafter inbox + publish | AI postmortem drafts | ✅ (CRUD + AI + publishers) | ○ | No drafts seeded | S |
| Alerts — rules + evaluator | Threshold rules → webhook | ✅ (CRUD + 5s evaluator) | ○ | No rules seeded | S |
| Timeline — merged swimlanes | Audit+deploy+spike on one axis | ✅ | ◐ (spike only) | No operational events | S |
| Audit — event log + explain | Full state-change trail | ✅ | ◐ (spike only) | Same as timeline | S |
| Discovery AWS — scan→recs→PR | Inventory + recs + merge-ready TF PR | ✅ | ● scan+recs; ◐ PR (needs real GitHub) | PR needs preview mode | PR |
| Discovery GCP/Azure/OCI | Same, all four clouds | ✅ scan+recs; TF preview | ◐ (no "Open PR" wiring) | PR parity for 3 clouds | PR |
| env→Terraform import blocks | Generate import{} blocks | ✅ (all 4 clouds) | ● | none | — |
| Inventory dashboard | Multi-cloud gap view | ✅ | ● | none | — |
| Quickstart | Onboarding wizard | ✅ | ● | none | — |
| **Ask Squadron** — conversational AI | The "JARVIS" surface | ✅ (endpoint + context bag) | ○ **dark without key** | LLM-gated, no demo mode | AI |

Tally: **7 surfaces already full ●**, ~11 partial ◐, ~13 empty/dark ○. Every ○
and most ◐ trace to one of four root causes: (a) no telemetry ingest, (b)
single-agent fleet, (c) unseeded operational state, (d) AI gated on a live key.

---

## 3. Target state — "live simulated production"

The demo should feel like logging into a real Squadron instance running a
healthy-but-imperfect production fleet. Concretely, with one click ("Enable
demo environment") and no cloud account, no agent install, no API key:

- **A living fleet.** ~300–500 agents across 4–6 groups (web, api, workers,
  data, edge), realistic version spread, ~5% offline, ~12% config-drifted.
  Driven by `fleetsim` against the local OpAMP server.
- **Continuous telemetry.** `otlpsim` pushes metrics/logs/traces for those same
  agents into the ingest path, so DuckDB fills: per-agent Logs/Metrics/Traces
  tabs populate, Cost Insights shows real bytes and $, Savings derives real
  quick-wins, the SquadronQL explorer returns rows, Data Flow shows $/destination.
  Runs continuously (low rate) so charts keep moving while you explore.
- **A cost story that closes.** The seeded +312% spike is backed by real
  telemetry whose attribution (one noisy attribute eating ~25% of trace bytes)
  the recommendation engine actually detects. The proposer produces a drafted
  rollout — **with a deterministic fallback so it works with no API key** — that
  lands in `pending_approval`. Approving it advances stages against the live
  fleet. An action step dispatches to a seeded demo runner. An incident draft is
  produced. Every step lands in the audit/timeline swimlanes.
- **Discovery that closes on all four clouds.** AWS/GCP/Azure/OCI each scan
  canned inventory, generate recs, and offer a **preview PR** (rendered
  Terraform + branch name, no real GitHub token needed).
- **Ask Squadron answers.** A demo mode returns compelling grounded answers over
  the seeded fleet/cost/rollout state, so the headline conversational surface is
  live without a key.
- **Free exploration first; optional guided flythrough second.** The existing
  coach-mark tour engine stays, but as an *optional* narrated path over an
  already-alive product — not the product itself.

Design principle, non-negotiable going forward: **any capability we claim that
the demo cannot show end-to-end is a defect, not a "future tour."**

---

## 4. Build plan (phased, each phase independently shippable)

**Phase T — Telemetry ingest (unblocks the most surfaces).**
Wire `otlpsim` into the demo path so DuckDB fills for the seeded fleet.
Point the demo-collector's exporter at Squadron instead of `debug`. Seed a
noisy-attribute profile so cost attribution + recommendations derive real
findings. *Unblocks: Logs, Metrics, Traces, Cost Insights, Savings, Data Flow,
SquadronQL, dashboard cost/health.*

**Phase F — Fleet scale-up.** Run `fleetsim` in the demo path to stand up
300–500 agents across 4–6 groups with realistic status/version/drift spread.
*Unblocks: Agents list/filters, Fleet Map (all three tabs), Groups, dashboard
fleet stats.*

**Phase AI — Deterministic AI fallback.** Give the proposer a seeded,
pre-computed proposal for the demo spike when no key is present, and a demo mode
for Ask Squadron / Explain / Merge with grounded canned answers. *Unblocks: the
flagship loop and the JARVIS surface without external creds.*

**Phase S — Direct operational seed.** Seed a mid-flight rollout, a runner + a
dispatched action, 2–3 incident drafts, 2–3 alert rules (disabled by default),
and a realistic 8-event audit/timeline trail. *Unblocks: Rollouts progression,
Actions/Runners, Incidents, Alerts, Timeline, Audit.*

**Phase PR — Discovery/IaC parity.** Wire GCP/Azure/OCI recommendations through
the same path as AWS; add a `preview=true` mode to the Terraform-import-PR
endpoint so "Open PR" works demo-safe on all four clouds. *Unblocks: Discovery
PR loop across clouds.*

**Phase U — Polish.** Config version-history tab + inline lint; "Enable demo
environment" as one clearly-labeled control that orchestrates T+F+AI+S+PR
idempotently, with a clean teardown.

Suggested order: **T → F → AI → S → PR → U.** T and F together transform the
demo from "empty shell" to "alive"; AI and S close the flagship narrative; PR
and U finish the edges.

---

## 5. Open decisions for sign-off

1. **Scope of first build.** All six phases, or land T+F first (demo goes from
   dead to alive) and iterate? Recommendation: T+F+AI+S as the first
   sign-off-worthy milestone; PR+U immediately after.
2. **Fleet size default.** 300–500 agents is convincing without being heavy on
   a laptop. Confirm the ceiling you want to target.
3. **Continuous vs. snapshot telemetry.** Live `otlpsim` (charts keep moving,
   ~real) vs. a pre-baked telemetry snapshot (instant cold-start, static).
   Recommendation: live at a low rate, with a snapshot fallback for constrained
   machines.
4. **Deterministic-proposal fallback** for the AI loop is the one behavioral
   change to shipping code (not just seed data). Confirm you're comfortable with
   the demo showing a canned-but-realistic proposal when no key is set, clearly
   distinguishable in logs from a live LLM proposal.
