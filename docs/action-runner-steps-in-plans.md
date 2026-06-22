# Action runner steps in plans — operator runbook

This is the operator-facing runbook for v0.89.14's slice-1
implementation of action runner steps inside multi-step plans
([#530](./proposals/530-action-runner-steps-in-plans.md)). It
covers the prerequisites you need in place before authoring an
action step, the shape of the step itself, the state machine the
plan engine walks it through, the failure modes you can see, the
audit interpretation, and an end-to-end worked example.

If this is your first encounter with multi-step plans, read
[multi-step-plans-design.md](./multi-step-plans-design.md) first.
If you have not yet installed an action runner, read
[action-runner-design.md](./action-runner-design.md) first. This
runbook assumes both are in place; it walks the operator-side
flow of stitching them together.

For a first test against a sandbox group with one runner, the
walkthrough takes about 15 minutes. For a production plan that
mixes config rotations and runner verbs across two or three
groups, budget 30 minutes plus your team's normal approval
turnaround.

## What we're building

A **plan** with at least one step whose `kind` is `action`. The
plan engine treats the action step the same way it treats a
config-rotation step for sequencing, approval, and audit purposes
— with three operator-visible differences worth knowing up front:

1. **The action dispatches as a single signed request.** There are
   no stages, no canary percentage, no abort-on-error-rate. The
   runner either succeeds, fails, denies, or times out; the
   engine treats each terminal status as the corresponding
   transition. Stages and abort criteria are config-rotation
   concepts; the runner already has its own per-action timeout
   envelope, so the plan layer doesn't re-implement them.
2. **The plan-step-0 approval covers the action.** The standalone
   action-runner path (v0.53) requires a two-phase dry-run +
   execute interaction. Plan-embedded actions skip that — the
   operator approving the plan at step 0 is the same operator
   who would have approved the dry-run, and forcing the
   double-approve adds latency without changing the trust
   model. This is a documented divergence from the standalone
   path, called out in slice-1's design doc §11 and reiterated
   in the proposer's prompt teaching.
3. **Action steps in the succeeded prefix are NOT reversed on a
   later failure.** The plan engine's backwards rollback walk
   skips them. Action reversal is an action-type property
   (does the runner know how to undo a `restart-systemd-service`?
   No.), not a plan property, and Squadron's MVP catalog has no
   automatic-undo verbs. The skipped action steps still show up
   in the `plan.rolled_back` audit payload's step list so SIEM
   consumers see the full arc.

The trust model is the same as everywhere else in Squadron:
Squadron signs the action request, the runner verifies the
signature against a pinned public key and checks the action type
against its declared capability set before doing any privileged
work. Squadron never holds long-lived credentials for the node.
The operator who installed the runner owns the capability set,
not the proposer. Two layers of policy say no before any verb
runs: the plan's approval gate at step 0, and the runner's local
capability check on dispatch.

## What this is good for

- A config rotation that requires a service restart to take
  effect. Push the new config (kind=rollout, step 0), wait for
  the rollout to converge, then restart the collector unit
  (kind=action, step 1). One approval, one audit arc, one
  rollback path for the predecessor on failure.
- A multi-stage cost-spike fix where the second stage is a
  feature-flag toggle on a single host (or a small fleet).
  The proposer's plan output threads kind=rollout and
  kind=action steps in the order they need to execute. The
  operator approves once.
- A maintenance window where the surgical action (drain a load
  balancer pool member, rotate an expired secret) is bracketed
  by a config push on either side. The plan's sequencing makes
  the bracket atomic from the approver's perspective.

## What this is NOT

Read this carefully. The first three are features, not
limitations.

- **The plan engine does not dry-run action steps.** Plan-
  embedded actions dispatch with `phase=execute` directly,
  per the slice-1 trade-off above. If you want a dry-run, run
  the standalone two-phase action-runner path (`POST
  /api/v1/actions/dispatch`) instead of embedding it in a
  plan.
- **Action steps are not auto-reversed on a later failure.**
  The backwards rollback walk skips them. If your plan needs
  reversal-on-failure semantics for the action step, prefer
  the standalone path or stage the action as the LAST step
  (so nothing after it can fail and trigger a walk back over
  it).
- **The runner's capability set is owned by the operator who
  installed the runner.** The proposer can propose any
  `action_type` string; the runner refuses anything outside
  its declared capabilities. There is no mechanism for the
  proposer or for Squadron's plan engine to add a verb to a
  runner's capability set — that's an operator config-file
  edit on the runner host, intentionally out of band.
- **The runner is not addressable by hostname or by group.**
  The plan step names a specific `runner_id` — the public
  half of the runner's ed25519 keypair. A plan that fans an
  action out to N runners is a plan with N steps (one per
  runner_id), or a future slice. Slice 1 is one runner per
  step.
- **`action_request_id` is not user-facing.** The dispatcher
  generates it at dispatch time and writes it to the rollout
  row; the operator never has to construct it. If you need to
  cross-reference an action against the standalone actions
  table for debugging, use the plan_id + plan_step_index pair
  (both are stamped on the audit event payload).
- **The MVP action catalog is small.** Slice 1 ships with
  `restart-systemd-service`, `restart-docker-container`, and
  `run-shell-allowlist`. The proposer prompt teaches this
  set. Other verbs (rotate-secret, kubectl-apply, drain-pool-
  member, verify-metric-threshold) are on the post-MVP roadmap
  and may already exist in your local runner's capability
  declaration; the system will run them if dispatched, but
  the proposer won't propose them yet.
- **GitOps integration is out of scope here.** Action steps
  call a runner, not a Git repo. If your fix is "edit
  Terraform and open a PR," that's the Connect IaC repo
  flow ([discovery-iac-first-time-setup.md](./discovery-iac-first-time-setup.md)),
  not an action step.

## Prerequisites

- A Squadron deployment on v0.89.14 or later. Earlier versions
  silently drop `kind=action` steps with a 400 because the
  validator wasn't installed yet.
- At least one action runner installed, registered, and visible
  in `GET /api/v1/runners`. The runner's `runner_id` (the
  `ed25519:...` string from registration) is what the plan
  step's `action.runner_id` field references.
- The runner's capability set includes the `action_type` you
  intend to dispatch, and the runner's constraints (unit_name
  glob, namespace, etc.) permit the specific parameters you
  plan to send. Squadron checks the action type against the
  registered capability set at dispatch time; constraint
  violations land as `action.denied` from the runner.
- An auth token with BOTH `rollouts:write` AND `actions:write`
  scopes. The plan create handler enforces this conjunction at
  the route layer when any step is `kind=action`; pure
  rollout plans continue to require only `rollouts:write`.
  Auth-disabled deployments (`SQUADRON_DISABLE_AUTH=1`)
  bypass this check the same way they bypass every other scope
  check.

If you're not sure whether your runner is registered correctly,
run `squadronctl runners list` (or `GET /api/v1/runners` against
the same Bearer token you'll use to create the plan). The runner
must show `status: active` and a `last_seen` within the
heartbeat window. If `last_seen` is stale, the dispatcher will
still sign and persist the request, but the runner won't pick it
up; the engine will declare `action_timeout` once the request's
`expires_at` elapses.

## Step 1 — Author the plan with an action step

The plan create endpoint is `POST /api/v1/rollouts/plans`. The
body shape from v0.73 is unchanged; v0.89.14 adds the `kind`
discriminator and the `action` block.

A two-step plan with one rollout followed by one action looks
like this:

```json
{
  "steps": [
    {
      "kind": "rollout",
      "name": "Step 0: rotate otelcol config to drop noisy attribute",
      "group_id": "web-prod",
      "target_config_id": "cfg_abc123",
      "stages": [
        { "percentage": 25, "dwell_seconds": 300 },
        { "percentage": 100, "dwell_seconds": 0 }
      ],
      "require_approval": true
    },
    {
      "kind": "action",
      "name": "Step 1: restart otelcol after config rotation",
      "group_id": "web-prod",
      "action": {
        "runner_id": "ed25519:abc123...",
        "action_type": "restart-systemd-service",
        "parameters": { "unit_name": "otelcol-contrib.service" },
        "timeout_seconds": 300
      }
    }
  ]
}
```

Three rules the handler enforces, with the exact 400 messages
it returns so you can debug from the wire response:

1. **Action steps MUST NOT set the rollout-only fields.** If any
   of `target_config_id`, `inline_config_snippet`, `stages`, or
   `abort_criteria` is non-zero on a `kind=action` step, the
   handler returns
   `plan step N kind=action must not set <field>`. The proposer
   or a template may have populated these from a rollout
   skeleton before flipping `kind`; the validator catches the
   inconsistency before any storage write.
2. **`action.runner_id` and `action.action_type` are required.**
   Empty or whitespace-only values return
   `plan step N action.runner_id is required` /
   `plan step N action.action_type is required`. There is no
   defaulting — naming the runner explicitly is what makes the
   step dispatchable.
3. **`timeout_seconds` defaults to 300 and is clamped to 3600.**
   Anything above the clamp returns
   `plan step N action.timeout_seconds X exceeds maximum 3600`.
   Long-running operations don't belong inside a plan tick
   loop; the runner's own envelope is sized to the same
   ceiling. Zero is allowed and falls back to the 300-second
   default.

The handler also enforces `kind=action` does not appear in
mismatched form with rollout-only fields populated — the
acceptance test `create_plan_rejects_mixed_action_and_rollout_fields`
exercises this. If the response is a 400, the body's `detail`
field names exactly which step index needs fixing.

`require_approval` works on step 0 the same way it does for
rollouts. The plan's approval gate is at step 0; the operator
approves once, and steps 1..N inherit the approval transitively.
A plan where step 0 is the action and step 1 is the rollout is
just as legal as the reverse — the approval semantics don't
care about kind.

## Step 2 — Approve the plan

The plan detail page in the UI grows an Approve button on step 0
the same way it would for a config-only plan. The approver sees
the full step list — action steps render as cards with the
action_type, the target runner_id (truncated to the first 12
chars), and the parameters dictionary — and clicks Approve.

If the auth chain rejects the approver (insufficient scope, or
auth-disabled deployment running without an actor), the UI
surfaces the underlying 403 the same way it does for rollout
approvals. The proposer's action-step prompt teaches the model
to set `require_approval: true` on step 0 by default, so the
approval-gate-skipped case is rare in practice.

Once the approver clicks Approve, step 0 transitions out of
`pending_approval`. From here on the plan engine drives.

## Step 3 — Watch the engine walk the plan

The state transitions for an action step are:

| Trigger | Engine action |
| --- | --- |
| Predecessor succeeded → engine promotes the action step from `queued` to `pending` | Standard `advancePlan` path — same one rollout steps follow |
| Action step in `pending` | Dispatcher signs + persists an `action_request`; step transitions to `in_progress`; `action_request_id` is attached to the rollout row |
| Runner posts `success` | Step → `succeeded`; engine emits `action.executed`; engine calls `advancePlan` to promote the next step |
| Runner posts `failure` | Step → `aborted` with reason `action_runtime_failure`; engine triggers the backwards walk |
| Runner posts `denied` | Step → `aborted` with reason `action_denied`; engine triggers the backwards walk (denial typically means capability-set or constraint rejection by the runner) |
| `expires_at` elapses with no terminal status | Step → `aborted` with reason `action_timeout`; engine triggers the backwards walk |
| Operator aborts the plan in flight | Engine cancels the in-flight `action_request` via the dispatcher; step → `aborted` on the next tick |

A few subtleties worth knowing:

- **The engine polls; it doesn't subscribe.** Each plan tick
  (the engine's existing scan cadence) re-reads the
  `action_request` status. The runner posts terminal results
  to `POST /api/v1/actions/:id/result`; the engine sees them
  on the next tick. Worst-case latency from runner-success to
  step-succeeded is one tick.
- **Engine-side timeout is the dispatch envelope.** The
  `action_request` row carries an `expires_at` the signer set
  to `issued_at + timeout_seconds` at dispatch. The poll path
  checks the wall clock against `expires_at` on every tick;
  once it's past, the step aborts with reason
  `action_timeout`. The runner-side timeout (if the action
  hangs internally) is the runner's concern; if the runner
  reports failure before `expires_at`, the engine treats it
  as `action_runtime_failure`, not timeout.
- **Aborted action steps transition to `rolled_back` on the
  next tick.** This is a bookkeeping move that lets
  `IsTerminal()` return true so the engine stops scanning
  the row each tick. It does NOT mean Squadron rolled the
  action back — it didn't, because there's no auto-undo.
  The audit event recorded at abort time is the source of
  truth for what happened; the `rolled_back` state is the
  finalize marker. Slice-1's docs flag this clearly so SIEM
  consumers and the timeline humanizer don't mis-read it.

The UI's plan detail page lights each step with the standard
rollout-state color (pending = yellow, in_progress = blue,
succeeded = green, aborted/rolled_back = red). Hover on an
action step in any non-terminal state and the tooltip shows
the action_request_id; the same id appears in the audit event
payloads.

## Step 4 — Read the audit timeline

The plan engine reuses the existing v0.53 action event types
rather than minting new ones. Plan-embedded action events add
three fields to the payload:

- **`plan_id`** — the plan the action belongs to.
- **`plan_step_index`** — the action's position within the
  plan (0-based).
- **`plan_step_origin`** — `"plan_embedded"` for actions
  dispatched through a plan step; `"standalone"` (or
  omitted) for the existing v0.53 dispatch path. Filtering
  on this lets SIEM consumers separate the two paths
  without joining tables.

The four event types you'll see are:

- **`action.dispatched`** — the engine just signed and
  persisted the action_request. The runner has not picked it
  up yet. Payload fields: `request_id`, `runner_id`,
  `action_type`, plus the three plan fields above.
- **`action.executed`** — the runner reported `success`. The
  engine has transitioned the step to `succeeded` and is
  promoting the next step. Payload includes the runner's
  reported `result_payload` if any.
- **`action.failed`** — the runner reported `failure`. The
  engine has transitioned the step to `aborted` with reason
  `action_runtime_failure` and triggered the backwards walk
  on the rest of the plan.
- **`action.denied`** — the runner reported `denied` (or the
  signature check failed, or the action type was outside the
  capability set). Payload includes a `denied_for` reason
  field if the runner supplied one; the engine substitutes
  the generic `action_denied` if not. The backwards walk
  fires on this path too.

The Timeline page's v0.89.14 humanizer renders plan-embedded
events with a payload-aware title: `Action <type> dispatched
for plan <plan_id_short> step <idx>`, `Action <type> succeeded
for plan <plan_id_short> step <idx>`, etc. The short plan id is
the first 8 chars of the UUID, matching the truncation used
elsewhere in the UI. Standalone action events fall through to
the unchanged v0.53 title format. If you've written external
log parsers against the action.* event types, no change is
needed — the new payload fields are additive and the existing
top-level keys are unchanged.

The `plan.rolled_back` event (fired when the backwards walk
completes) lists every step in the plan, including the action
steps that the walk SKIPPED. The step entries carry a
`skipped_reason` field set to `action_step_no_auto_undo`. SIEM
consumers can filter on that to surface "the action step ran
and was not reversed" without joining against the action
event table.

## Step 5 — Worked example

Cost spike on a Splunk-fed group `web-prod`. The proposer's
analysis: a noisy attribute is dominating the volume, and the
collector needs a restart after the config drop so its
processor pipeline rebuilds. Two steps, one approval, one
audit arc.

1. **Operator (or proposer) submits the plan.** Two-step plan:
   step 0 is a kind=rollout to push the new config to
   `web-prod`; step 1 is a kind=action to restart the
   `otelcol-contrib.service` systemd unit on the same group's
   runner. `require_approval: true` on step 0. Step 1 has no
   approval gate of its own.
2. **Operator approves the plan at step 0.** UI shows two
   cards — the config rollout with its 25%→100% stages, and
   the action card with action_type `restart-systemd-service`
   and parameters `{"unit_name": "otelcol-contrib.service"}`.
   Operator clicks Approve.
3. **Engine drives step 0.** Standard rollout sequencing: 25%
   canary at 5 minutes dwell, then 100%. Audit timeline shows
   `rollout.started`, `rollout.stage_advanced`, `rollout.succeeded`.
4. **Engine promotes step 1 from queued to pending.** This is
   the `advancePlan` step-rollout / step-action interleave;
   the action step looks identical to a rollout step at this
   layer.
5. **Dispatcher signs the action_request.** Step 1 transitions
   to `in_progress`. Timeline shows `action.dispatched` with
   `plan_id`, `plan_step_index=1`, `plan_step_origin=plan_embedded`,
   `action_type=restart-systemd-service`, `runner_id=ed25519:...`.
6. **Runner picks up the request, verifies the signature,
   checks `restart-systemd-service` is in its capability
   set, checks the unit_name `otelcol-contrib.service`
   matches the configured glob, runs the action.** Posts
   `success` back to Squadron.
7. **Engine's next tick reads `success` from the action_request
   row.** Step 1 transitions to `succeeded`. Timeline shows
   `action.executed`. Engine calls `advancePlan` which finds
   no more steps and emits `plan.completed`.

End-to-end latency from approval to plan-complete is
roughly: 5 min canary dwell + a few seconds to sign + a few
seconds for the runner heartbeat + the systemd restart
itself. The audit arc is one plan, two non-rollout state
transitions, three action audit events; an approver looking
at the timeline sees the rotation, the restart, and the
completion in chronological order.

## Failure walk: what happens when step 1 fails

Same plan, but the unit fails to restart (systemd reports
exit-1 and the runner reports `failure`).

1. **Step 0 already succeeded.** Config was pushed to 100% of
   `web-prod`. Audit timeline up to this point is identical to
   the happy path through `rollout.succeeded`.
2. **Step 1 dispatched, runner posts `failure`.** Engine's next
   tick reads the failure status, transitions step 1 to
   `aborted` with reason `action_runtime_failure`. Audit
   timeline shows `action.failed` with the three plan-embedded
   fields plus the runner's `denied_for` field (often the
   systemd exit reason or a stderr excerpt) if reported.
3. **Engine triggers the backwards walk.** No queued followers
   to cancel (step 1 was the last). One succeeded predecessor
   in the plan — step 0, the config rollout. The walk
   identifies step 0 as a kind=rollout and pushes a rollback
   config to `web-prod`. Audit timeline shows the rollback
   rollout's own arc starting with `rollout.started`.
4. **Plan-level event.** Once the backwards walk finishes,
   the engine emits `plan.rolled_back`. The payload's step
   list has step 0 marked `rolled_back` and step 1 marked
   `aborted`. There is no `skipped` entry here because the
   only succeeded predecessor was a rollout, which the walk
   reverses. Action steps skipped by the walk would carry
   `skipped_reason: action_step_no_auto_undo` per §"Step 4"
   above.

The operator's recovery path is to read the runner's posted
failure reason, fix whatever broke the systemd unit, and
either resubmit the plan or run the action standalone.
Squadron does not auto-retry; the plan is closed and the
audit timeline is the receipt.

## Failure walk: what happens when step 0 (the rollout) fails AFTER step 1 (an action) succeeded

This is the inverted case worth thinking through, because it's
the one slice-1's trade-offs are most opinionated about.

A three-step plan: step 0 is `action` (rotate-secret on a
single host), step 1 is `rollout` (push the new config that
references the rotated secret), step 2 is `action` (restart
the daemon).

If step 1's rollout fails (auto-abort on error rate, say),
the backwards walk starts at step 1 and tries to roll back
step 0. Step 0 is an action — the walk skips it, emits
`plan.rolled_back` with step 0 marked `skipped_reason:
action_step_no_auto_undo`, and emits an audit warning. The
secret on the host stays rotated. The operator owns the
manual reversal.

This is on purpose. The system would rather be honest that
it can't undo a rotation than silently fake it. The proposer
prompt explicitly teaches the model that action steps in the
succeeded prefix won't be reversed, so a careful proposer
will sequence the irreversible action LATER in the plan, not
EARLIER — or split it into two plans.

## CLI parity

`squadronctl plans create --file plan.json` accepts the same
JSON body the API does, including the `kind=action` step shape.
A 400 from the server surfaces as a humanized error naming the
offending step index. `squadronctl plans get <plan_id>` renders
action steps with the action_type and runner_id alongside the
rollout steps.

There is no `squadronctl plans propose --include-actions` flag
— the proposer decides on action-step inclusion based on the
cost-spike context, not an operator flag.

## Roadmap touchpoints

The slice-1 trade-offs that are most likely to shift in later
slices:

- **In-plan dry-run.** Slice 2 candidate. Would require the
  plan engine to dispatch `phase=dry_run` first, capture the
  runner's prediction in the plan-detail UI, gate the
  execute phase on a second operator click, and audit both
  phases. The standalone path's two-phase pattern would
  transplant straight through.
- **Action-type-driven reversal.** Slice 3 candidate. A
  runner capability declaration that includes an
  `auto_reverse` block per action type lets the backwards
  walk dispatch the reverse verb on a previously-succeeded
  action step. Rotate-secret + restore-previous-secret is the
  canonical example. Until then, the skip behavior is the
  honest default.
- **Multi-runner fan-out.** Slice 2 candidate. A single
  action step that names a runner group (or a label
  selector) instead of a single `runner_id`, dispatched to
  all matching runners with one shared `action_request`
  envelope. The signing path already supports this; the
  dispatcher API doesn't yet.

These slice candidates are tracked under
[#530](./proposals/530-action-runner-steps-in-plans.md). None
ship in v0.89.14; everything in this runbook describes
behavior you can rely on today.

## Cross-references

- [Multi-step plans — design](./multi-step-plans-design.md) —
  the plan protocol this runbook builds on. The v0.89.14
  appendix at the end of that doc is the locked design this
  implementation promotes.
- [Action runner — design](./action-runner-design.md) — the
  signed-action protocol the plan engine reuses verbatim.
  Capability declarations, signature verification, dry-run
  semantics (for the standalone path), and the threat model
  all live there.
- [Rollouts](./rollouts.md) — the single-rollout protocol
  that kind=rollout plan steps inherit.
- [Audit log](./audit-log.md) — event-type catalog and
  payload schemas. The v0.89.14 plan-embedded fields are
  documented under the action.* entries.
- [#530 proposal](./proposals/530-action-runner-steps-in-plans.md) —
  the locked design that v0.89.14 promotes to implementation.
