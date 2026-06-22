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

## Inline config snippets (v0.78)

The proposer that's coming in v0.79 needs to emit N step plans, but
it doesn't know in advance which `target_config_id` values exist in
storage. v0.78 closes that gap by letting plan steps supply an
inline YAML snippet instead of a `target_config_id`.

The wire shape on `RolloutInput` gains one optional field:

```json
{
  "name": "Step 1 — drop noisy attribute",
  "group_id": "web-prod",
  "inline_config_snippet": "receivers: ...\nprocessors: ...\n",
  "stages": [{"mode": "percent", "percentage": 100}]
}
```

Rules:

- Exactly one of `target_config_id` or `inline_config_snippet`
  must be set per step. Both → ambiguous, rejected. Neither →
  no target, rejected.
- The server lints the snippet first. Error severity findings
  reject the plan create; warnings + infos pass through (same
  posture the existing `HandleCreateRollout` takes).
- After lint passes, the server creates a new `Config` row in
  the step's group with the snippet as content. The config name
  encodes the plan id + step index (`ai-plan-<8-char>-step-<n>`)
  so an operator scanning the Configs page can trace it back.
- The step's `target_config_id` is rewritten to the new config's
  id before the rollout is persisted. From the engine's
  perspective the step is indistinguishable from one created
  against a pre-existing config.
- An audit event `config.created` fires per materialized config
  with the plan id, step index, and `source: "plan_inline_snippet"`
  in the payload so SIEM consumers can correlate config creates
  to plan creates.

Standalone rollout `Create` ignores the field — only `CreatePlan`
interprets it. This keeps the v0.4–v0.77 single-rollout contract
byte-identical.

Failure mode: if a snippet's materialization fails mid-plan
(after K-1 steps were already created), the existing
`CancelPlanFollowers` cleanup runs the same way as the v0.73
partial-failure path. Orphan configs from earlier successful
materializations stay in storage — they're cheap and a future GC
pass can clean them up. The audit trail tells the story.

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

## Action runner steps in plans — v0.80+ candidate arc

As of v0.79 every plan step is a rollout — a config push. Plans
cannot include action-runner calls (verify, notify, page on-call,
integrity check). The dropped seeds from v0.79's stress corpus
reframe are the natural seed corpus for this arc when it ships:

- **drop_attribute_then_verify** — drop the attribute + action
  runner verifies cost dropped >50% within 5 minutes
- **rotate_exporter_then_observe** — switch destination + action
  runner verifies error rate stays below threshold
- **enable_tail_sampling_with_fallback** — enable tail sampling +
  action runner pages on-call if metrics-for-tail-sampled-count
  doesn't appear within 5 minutes
- **multi_attribute_drop_with_integrity_check** — drop attributes +
  action runner verifies metric integrity stays above threshold;
  failure triggers automatic Abort + backwards rollback
- **cleanup_with_downstream_notification** — drop redundant log
  lines + action runner notifies dependent team Slack channel

Rough scope notes for the engine work:

- A new step kind discriminator on the plan steps table:
  `step_kind: "rollout" | "action"`. Action steps carry an
  action request id (v0.54 actions table) instead of a
  target config id.
- Engine logic to dispatch the action via the action runner when
  the step's predecessor reaches succeeded.
- Engine wait state for action completion. Action timeout caps
  per the action runner's existing semantics; a timed-out action
  step counts as failure and triggers the v0.72 backwards walk.
- Audit events: `plan.step_action_dispatched`,
  `plan.step_action_completed`, `plan.step_action_failed`.
- Bridge dispatch: the proposer's `kind: "plan"` schema gains a
  step type discriminator so action steps can be requested at
  proposal time.

Best estimate: 4-6 release arc (engine + storage + audit + UI +
proposer schema + bridge). The v0.79 design philosophy carries
forward — sequence small slices, fail honestly when something
doesn't fit.

## v0.89.14 (#630) — action steps in plans, slice 1

Plan steps gained a third kind alongside the v0.69 default `rollout`
and the v0.79 nested `plan`: `action`. An action step dispatches a
signed action-runner verb mid-plan, lets the runner complete, and
feeds the runner's reported result back into the plan's lifecycle.

### Step kind

```
"kind": "rollout" | "action"
```

Empty kind decodes as `rollout` for backwards compatibility. The
storage layer (`rollouts.step_kind`, default `rollout`) round-trips
the field through every pre-v0.89.14 row unchanged.

### Action-step shape

```json
{
  "kind": "action",
  "name": "Step 1: restart otelcol after config rotation",
  "group_id": "web-prod",
  "action": {
    "runner_id": "ed25519:abc…",
    "action_type": "restart-systemd-service",
    "parameters": { "unit_name": "otelcol.service" },
    "timeout_seconds": 300
  }
}
```

Rules: action steps MUST NOT set `target_config_id`,
`inline_config_snippet`, `stages`, or `abort_criteria`. `runner_id`
and `action_type` are required. `timeout_seconds` defaults to 300
and is clamped to 3600. The plan create handler validates the shape
and emits a precise 400 naming the offending step index.

### Plan-engine state transitions for action steps

| Trigger | Engine action |
| --- | --- |
| Predecessor succeeded → engine promotes the action step Queued → Pending | the same `advancePlan` path rollout steps follow |
| Action step in Pending | dispatch via `services.ActionDispatcher`; sign + persist the action_request with status=pending; set step state = `in_progress` |
| Runner posts `success` | step → `succeeded`; emit `action.executed`; promote next step |
| Runner posts `failure` | step → `aborted` with reason `action_runtime_failure`; trigger backwards walk |
| Runner posts `denied` | step → `aborted` with reason `action_denied`; trigger backwards walk |
| `ExpiresAt` elapses with no terminal result | step → `aborted` with reason `action_timeout`; trigger backwards walk |
| Operator aborts the plan in flight | engine marks the action_request expired via `Cancel`; step → `aborted` on next tick |

### Backwards rollback walk

Action steps in the succeeded prefix are SKIPPED by the walk.
Actions are "did a thing", not "set state X"; reversal is an action-
type property, not a plan property, and Squadron has no automatic
action undo today. The skipped action steps still appear in the
`plan.rolled_back` audit payload's step list so SIEM consumers see
the full arc.

### Audit payload extensions

The existing `action.dispatched` / `action.executed` / `action.failed`
/ `action.denied` event types are reused. Plan-embedded payloads
gain three fields:

- `plan_id` — the plan the dispatched action belongs to.
- `plan_step_index` — the action's position within the plan.
- `plan_step_origin` — `"plan_embedded"` for plan-embedded
  requests, `"standalone"` for the existing v0.53 dispatch path.

No new event types; no new audit targets. Filtering on
`plan_step_origin` lets SIEM consumers distinguish the two paths
without joining tables.

### Cross-references

- [#530 — Action runner steps in plans](./proposals/530-action-runner-steps-in-plans.md)
  — the locked design this section promotes.
- [Action runner design](./action-runner-design.md) — the signed-
  action protocol the plan engine reuses.

## See also

- [Rollouts](./rollouts.md) — the single rollout protocol plans build
  on.
- [Action runner design](./action-runner-design.md) — the parallel
  protocol for execution side actions; plans don't subsume action
  runs (those stay separate), but the audit grouping pattern is
  reused.
- [Roadmap post v0.52](./roadmap-post-v0.52.md) — Move 3 framing.
