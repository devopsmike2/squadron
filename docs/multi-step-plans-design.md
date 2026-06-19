# Multi step plans — design

**Status:** design + storage shipped in v0.69. Engine sequencing and UI
follow in subsequent releases. This doc captures the protocol so the
engine work, the proposer changes, and the UI all reference the same
contract.

## The problem

Today's rollout engine handles one config change at a time. Each
rollout has its own approval gate, its own audit arc, its own rollback
chain.

The cost spike workflow exposes the gap. When an attribute spikes
costs, the operator (or the AI proposer) often needs more than one
change to actually fix it:

1. Drop the noisy attribute in the collector config.
2. Rotate the downstream Splunk index so the previous data ages out.
3. Update the alert rule so the new cost baseline doesn't fire false
   positives.

Today, each of those becomes a separate rollout. The approver
approves three things instead of one. The audit timeline shows three
unrelated arcs. A rollback halfway through requires three manual
abort + rollback clicks. The cognitive load defeats the JARVIS
framing — "the deputy proposed one fix" is the right shape; "the
deputy proposed three rollouts that you have to babysit" is not.

Multi step plans give the proposer a way to package N sequenced
rollouts as one approvable unit, and the engine a way to advance,
audit, and roll them back as a unit.

## The model

A **plan** is a sequence of rollouts grouped by a shared `plan_id`.

- `plan_id` is a string. Empty means "this rollout is standalone,"
  exactly as v0.4–v0.68 behaved. Backwards compatible by design.
- `plan_step_index` is a 0 based int. Step 0 is the first rollout in
  the plan; step 1 follows step 0; etc.

A plan is identified by its `plan_id`. The rollouts that make up the
plan are discoverable by `SELECT * FROM rollouts WHERE plan_id = ?
ORDER BY plan_step_index ASC`.

A standalone rollout (empty `plan_id`, `plan_step_index` zero) behaves
exactly as it did before v0.69. The engine treats it the same.

There is no separate `plans` table. The plan is fully described by the
set of rollouts that share its id; reifying it as a table would create
a second source of truth and a migration headache for a feature that
benefits from being a pure grouping. If a future release wants
plan level metadata (an AI generated plan summary, an operator name
for the plan, plan level metrics), that can live on a `plans` table
that joins on `plan_id` without changing the rollouts contract.

## Approval semantics

A plan is approved or rejected as a unit. The approval gate sits on
**step 0**.

- When the proposer creates a plan, only step 0 has `require_approval`
  set; steps 1..N have it unset.
- The approver approves or rejects step 0. The engine advances step 0
  the same way it would a standalone rollout.
- When step 0 reaches `succeeded`, the engine looks for the next step
  in the plan (`plan_id = ? AND plan_step_index = ? + 1`) and starts
  it. Subsequent steps don't reapprove — the operator already approved
  the plan when they approved step 0.
- If the approver rejects step 0, the engine marks every step in the
  plan as `rejected`. The audit timeline shows one rejection event
  with the full plan attached, not N rejections.

The per group `require_approval` policy applies at step 0. A group
flagged `require_approval=true` forces step 0 into `pending_approval`;
the rest of the plan inherits the gate transitively.

## Failure + rollback semantics

A plan that fails mid sequence rolls back as a unit.

- If step N fails (aborts or transitions to `rolled_back`), the engine
  marks steps N+1..end as `cancelled` — they never run, because the
  plan's prerequisite chain broke.
- The engine also walks backwards through steps 0..N-1 (the steps
  that already succeeded) and creates rollback rollouts for each, in
  reverse order. Step N-1's rollback fires first; step 0's last.
- Each rollback inherits the same plan grouping (same `plan_id`, new
  `plan_step_index` values offset into a reserved negative range so the
  rollback rollouts are distinguishable in the timeline). Reserved
  range: `plan_step_index < 0` means rollback step; `-1` is the
  rollback of the highest succeeded forward step, `-2` the next, etc.

This makes the audit timeline tell the full story without the operator
opening individual rollouts: "Plan X step 0 ok, step 1 ok, step 2
aborted, rollback of step 1 ok, rollback of step 0 ok."

If the operator clicks Roll back on a plan step that succeeded, the
engine treats the click as "roll back the whole plan starting from
this step." The single click triggers the same backwards walk as an
automatic failure. The v0.60 RolledBackFromID field still applies —
each generated rollback rollout points back at the forward step it
undoes.

## Audit events

The existing rollout level audit events (`rollout.created`,
`rollout.approved`, `rollout.rejected`, `rollout.aborted`,
`rollout.succeeded`, `rollout.rolled_back`, `rollout.rollback_completed`)
all continue to fire per step.

New plan level events:

- `plan.created` — fires once when the proposer creates a plan. Payload
  carries the `plan_id`, the step count, and the AI reasoning if the
  plan came from the proposer.
- `plan.approved` — fires when step 0 transitions out of
  `pending_approval`. Payload mirrors `rollout.approved` plus the
  `plan_id`.
- `plan.rejected` — fires when step 0 is rejected. Payload carries
  the rejection notes and the list of cancelled step ids.
- `plan.completed` — fires when the final step reaches `succeeded`.
  Payload carries the plan's total duration.
- `plan.rolled_back` — fires when the backwards rollback walk
  completes, either from a mid sequence failure or an operator click.
  Payload carries the failing step's id and the list of rolled back
  step ids.

SIEM consumers can subscribe to `plan.*` to track plan level outcomes
without sifting through the per step events. The per step events stay
so SIEM rules that don't care about plans don't change.

## API surface (preview)

The engine work in subsequent releases will add:

- `POST /api/v1/rollouts/plans` — creates a plan from a list of step
  inputs. Wraps the existing `POST /api/v1/rollouts` Create logic
  N times under one transaction with the shared `plan_id` assigned.
- `GET /api/v1/rollouts/plans/:id` — returns the plan envelope:
  metadata plus the ordered list of rollouts.
- `POST /api/v1/rollouts/plans/:id/approve` and `/reject` — convenience
  wrappers around the step 0 approval routes that the UI uses so it
  doesn't have to figure out which step is the gate.

Scope: `rollouts:write` for create, `rollouts:approve` for the
approval routes. Same vocabulary as the existing single rollout
routes; no new scope.

## Storage contract (shipped in v0.69)

The `rollouts` table gains two columns:

- `plan_id TEXT` — nullable.
- `plan_step_index INTEGER NOT NULL DEFAULT 0` — defaults to 0 so
  existing rows migrate cleanly. The combination of empty `plan_id`
  + step 0 is the standalone rollout shape.

Both fields round trip through the storage layer (sqlite + memory),
the service layer (`services.Rollout`, `toStorageRollout`,
`toServiceRollout`), and the `RolloutInput` shape.

No engine logic uses these fields yet. A v0.69 Squadron stores a
plan id if a future client populates it, and surfaces it back through
the API, but no advancement happens between steps and no plan level
events fire. That's deliberate — the engine work needs the storage
contract stable first, and shipping the storage as its own release
gives the design doc time to bake before the engine commits to a
behavior that's hard to change later.

## Out of scope for v0.69

- Engine sequencing (step N+1 starts after step N reaches succeeded).
- Plan level audit events.
- The `POST /api/v1/rollouts/plans` endpoint.
- UI rendering of plans as grouped timelines.
- The AI proposer producing multi step plans instead of single rollouts.

Each of those is its own follow on release. The cleanest order:

1. **v0.70 — engine sequencing.** The advancement logic + the
   `plan.*` audit events. Tests prove a 3 step plan auto advances and
   a mid plan failure walks rollbacks correctly.
2. **v0.71 — API.** The plan create + approve endpoints. The UI gets
   a Plans tab on the Rollouts page.
3. **v0.72 — Proposer plan output.** The AI proposer learns to emit
   multi step plans for cost spikes that need more than one fix. This
   is the JARVIS payoff: the deputy proposes a plan, the operator
   approves once, the engine handles the rest.

## Why this is the right shape

The grouping field approach beats the alternatives:

- **A separate plans table** creates a second source of truth and
  requires the engine to do a join on every advancement check. The
  grouping field lets the engine answer "is there a next step?" with
  a single index lookup.
- **A linked list (next_rollout_id on each rollout)** is fragile — a
  broken link is silent, and reordering means rewriting pointers. The
  step index is order preserving by design and survives partial
  inserts.
- **Reifying plans as their own first class entity** (with their own
  state machine, their own CRUD, their own audit arc) would double
  the surface area and force the UI to render two views (rollouts
  and plans) for what's conceptually one. The grouping field lets
  the existing rollout list view filter or group as it likes without
  a new page.

The grouping field model also matches how operators reason about
the workflow. "These three rollouts are the cost spike fix"
naturally maps to "these three rollouts share a plan id." No
mental model upgrade required.

## See also

- [Rollouts](./rollouts.md) — the single rollout protocol plans build
  on.
- [Action runner design](./action-runner-design.md) — the parallel
  protocol for execution side actions; plans don't subsume action
  runs (those stay separate), but the audit grouping pattern is
  reused.
- [Roadmap post v0.52](./roadmap-post-v0.52.md) — Move 3 framing.
