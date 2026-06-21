# #530 — Action runner steps in plans

**Status:** proposal, slice-1 scoping. v0.80+ candidate.
**See also:** [multi-step-plans-design.md](../multi-step-plans-design.md),
[action-runner-design.md](../action-runner-design.md).

## 1. Problem

Every plan step today is a rollout. The v0.79 proposer and operators
cannot interleave **actions** (v0.53 runner verbs: restart, verify,
page) into a plan. The seed corpus in
[multi-step-plans-design.md:266–305](../multi-step-plans-design.md)
all want config push + runner verify; today that decomposes into a
plan + a separate action proposal, breaking the single-approval,
one-rollback-walk shape.

## 2. Non-goals (slice 1)

- Multiple actions per step; chaining inside one step.
- Retry on action failure. Failure → backwards walk.
- New action types. v0.53 registry unchanged.
- Parallel steps; `kind=wait`.
- Per-step rollback. Whole-plan walk only.
- Proposer prompt change — schema locks here, prompt is a follow-on.

## 3. Vocabulary

- **Action**: signed, schema'd verb a runner executes.
  `types.ActionRequest`
  ([types.go:859](../../internal/storage/applicationstore/types/types.go)).
- **Rollout**: staged config push.
  `services.Rollout`
  ([rollout_service.go:340](../../internal/services/rollout_service.go)).
- **Plan step** (extended): row in `rollouts` grouped by `plan_id`
  ([rollout_service.go:349–354](../../internal/services/rollout_service.go)).
  Slice 1 adds `step_kind`: `rollout` (default) or `action`. Action
  steps have empty `target_config_id` and carry `action_request_id`.

## 4. Wire schema

Proposer plan steps gain `kind`. Today's schema
([proposer_prompt.go:142–176](../../internal/ai/proposer_prompt.go))
treats every step as a rollout implicitly.

```json
{
  "kind": "plan",
  "plan": {
    "steps": [
      {
        "kind": "rollout",
        "name": "Step 0: drop container.id from metrics",
        "group_id": "web-prod",
        "inline_config_snippet": "...",
        "stages": [...],
        "abort_criteria": {...},
        "require_approval": true
      },
      {
        "kind": "action",
        "name": "Step 1: verify cost dropped",
        "group_id": "web-prod",
        "action": {
          "runner_id": "ed25519:abc123...",
          "action_type": "verify-metric-threshold",
          "parameters": {
            "metric": "otelcol_exporter_sent_metric_points",
            "comparison": "lt",
            "threshold_ratio": 0.5,
            "settle_window": "5m"
          },
          "timeout_seconds": 600
        }
      }
    ]
  }
}
```

Rules: `kind` omitted → `rollout` (v0.79 back-compat). `kind=action`
MUST set `action`, MUST NOT set `target_config_id`/
`inline_config_snippet`/`stages`/`abort_criteria`. `runner_id`
required. `timeout_seconds` default 300, max 3600. `require_approval`
ignored on action steps (plans approve at step 0).

Storage: `rollouts` gains `step_kind TEXT NOT NULL DEFAULT 'rollout'`
and `action_request_id TEXT` (FK → `action_requests.id`).

## 5. Plan-engine behavior

The forward walk in
[rollout_service_impl.go:313–432](../../internal/services/rollout_service_impl.go)
switches on `step_kind`:

| Trigger | Engine action |
|---|---|
| Predecessor succeeded, rollout | unchanged |
| Predecessor succeeded, action | sign + dispatch `Phase=execute`, step → `in_progress` |
| Runner `success` | step → `succeeded`, `action.executed`, `NextPlanStep` |
| Runner `failure`/`denied` | step → `aborted`, `action.failed`/`action.denied`, `CancelPlanFollowers` + `RollBackPlanPredecessors` (same path as a rollout abort, [rollout_service_impl.go:432, 1312](../../internal/services/rollout_service_impl.go)) |
| `timeout_seconds` elapsed | step → `aborted` reason `action_timeout`, `action.failed` `denied_for="timeout"`, rollback |
| No runner poll within `timeout/2` | same as timeout. No retry |
| Operator aborts plan in-flight | engine writes signed `abort` ([action-runner-design.md:288–292](../action-runner-design.md)); step → `aborted` on ack or timeout |

Slice 1 dispatches `Phase=execute` only — no in-plan dry-run.
Standalone actions keep two-phase dry-run + execute (Q1).

**Rollback.** Action steps in the succeeded prefix are **skipped** by
the backwards walk. Actions are "did a thing", not "set state X";
reversal is an action-type property
([action-runner-design.md:159](../action-runner-design.md)), not a
plan property. Skipped steps appear in `plan.rolled_back` payload but
are not re-dispatched.

## 6. Audit

Reuse `action.dispatched`/`action.executed`/`action.failed`/
`action.denied`
([audit_service.go:108–117](../../internal/services/audit_service.go)).
Plan-embedded actions add `plan_id`, `plan_step_index`, and
`plan_step_origin="plan_embedded"` (vs. standalone) to the payload.
No new `plan.action_*` events — `plan.*` envelope events from
[multi-step-plans-design.md:107–132](../multi-step-plans-design.md)
already cover the plan arc.

## 7. Security / trust

No regression vs. v0.53. Every dispatch is signed by `actions.Signer`
([action-runner-design.md:208–227](../action-runner-design.md));
runner-side capability + parameter checks unchanged; envelope
`expires_at` stays at 5 minutes; Squadron holds no node creds.

IAM: `CreatePlan` with any `kind=action` step requires **both**
`rollouts:write` and `actions:write` on the caller's token. No new
scope. Read-only IAM posture preserved.

## 8. Slice 1 contract

**In:**

1. `step_kind` + `action_request_id` columns on `rollouts`,
   round-tripped through storage/service/handler.
2. `CreatePlan` accepts `kind=action`; validates and materializes the
   `action_requests` row (`Phase=execute, Status=pending`) when the
   predecessor succeeds.
3. Forward-walk handling in `RolloutServiceImpl.advance`: dispatch,
   wait, success → advance, failure → rollback.
4. Engine-side timeout enforcement per action step.
5. `plan_id`/`plan_step_index`/`plan_step_origin` on existing
   `action.*` audit payloads.
6. `rollouts:write` + `actions:write` check on `CreatePlan` when any
   step is `kind=action`.

**Out:** proposer prompt change, UI rendering, dry-run for embedded
actions, `kind=wait`, parallel steps, retries, per-step rollback.

## 9. Open questions

1. **Dry-run at plan approval, or skip?** Slice 1 skips. Surfacing
   would require `pending_dry_run` and a reachable runner before
   approval.
2. **`verify-metric-threshold` action type.** Seed corpus needs it;
   v0.53 MVP catalog
   ([action-runner-design.md:148–181](../action-runner-design.md))
   does not. Add in slice 1 or block the corpus.
3. **`action_request_id` FK target.** Reuse `action_requests` (cheap)
   vs. new `plan_action_requests` (isolates schema blast radius).
4. **Runner-unavailable detection.** No-poll-within-`timeout/2` is a
   heuristic; runners have no liveness today. Freshness check at
   dispatch, or timeout-only?
5. **Forward-compat decode test.** Does slice 1 include an
   `internal/ai` test that `kind=action` decodes through
   `ProposalResult`, or is that deferred to the proposer slice?

## 10. Acceptance tests

Five tests in `internal/services/rollout_service_plans_test.go` (or
a peer `..._action_steps_test.go`):

1. **plan_with_action_step_advances_on_runner_success.** Create a
   3-step plan: rollout, action, rollout. Approve step 0; let it
   succeed. Assert: action step transitions to `in_progress`, an
   `action_requests` row exists with `phase=execute, status=pending,
   plan_step_origin=plan_embedded` in audit payload. Post a `success`
   result. Assert: action step → `succeeded`, step 2 starts,
   `action.executed` audit event carries `plan_id` and
   `plan_step_index=1`.

2. **plan_with_action_step_failure_triggers_backwards_walk.** Same
   plan shape. Step 0 succeeds. Action step dispatches; runner posts
   `failure`. Assert: action step → `aborted`, step 2 → `cancelled`,
   a rollback rollout exists for step 0 with `plan_step_index=-1`,
   `plan.rolled_back` fires with the failed step's id, no rollback
   rollout exists for the action step.

3. **action_step_timeout_counts_as_failure.** Step 0 succeeds; action
   step dispatches with `timeout_seconds=2`; no runner result is
   posted. After the engine tick past timeout, assert: action step →
   `aborted` with reason `action_timeout`, `action.failed` event
   carries `denied_for="timeout"`, backwards walk runs.

4. **create_plan_rejects_mixed_action_and_rollout_fields.** POST a
   plan where step 1 has both `inline_config_snippet` and `action`
   set. Assert: 400 with a precise error pointing at the offending
   step index. Repeat with `kind=action` but no `action` block: 400.
   Repeat with `kind=action` and `target_config_id` set: 400.

5. **create_plan_requires_actions_write_when_kind_action_present.**
   Token with only `rollouts:write` POSTs a plan whose step 1 is
   `kind=action`. Assert: 403, error names the missing scope. Same
   plan with token carrying both scopes: 200, plan created. Plan
   with only `kind=rollout` steps and `rollouts:write`-only token:
   200, regression guard for the existing path.
