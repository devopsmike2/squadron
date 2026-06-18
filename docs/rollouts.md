# Rollouts

A **rollout** is a safe, staged way to push a new config to a group of
agents. Instead of all-at-once, Squadron progressively widens the canary
set, watches the canary's drift state and error rate at each stage, and
rolls back to the previous config automatically if anything looks wrong.

- [Quick start](#quick-start)
- [The rollout state machine](#the-rollout-state-machine)
- [Stages: percent vs label mode](#stages-percent-vs-label-mode)
- [Dwell and abort criteria](#dwell-and-abort-criteria)
- [Cookbook: pre-tuned criteria recipes](#cookbook-pre-tuned-criteria-recipes)
- [Templates: pre-built rollout shapes](#templates-pre-built-rollout-shapes)
- [Preview and diff](#preview-and-diff)
- [Pause, resume, abort](#pause-resume-abort)
- [Webhook notifications](#webhook-notifications)
- [Audit trail](#audit-trail)
- [API reference](#api-reference)

## Quick start

In the UI, open the **Rollouts** page and click **New rollout**. The
fastest path to a sane rollout:

1. Pick a **template** (e.g. "Standard percent ramp"). This prefills
   stages and abort criteria.
2. Pick the target **group**.
3. Paste the target **config ID** (the new config you want to ship).
4. The preview pane shows you the diff between the group's current config
   and your target, plus any lint findings on the target.
5. Click **Start rollout**.

Squadron's background engine picks up the rollout on its next tick, pushes
the new config to the stage-1 canary set, and starts the dwell timer.

You can also create rollouts via the API. The wire shape is `RolloutInput`:

```bash
curl -X POST http://localhost:8080/api/v1/rollouts \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "ship v2 collector config",
    "group_id": "prod-app-collectors",
    "target_config_id": "9bbe...",
    "stages": [
      {"mode": "percent", "percentage": 10, "dwell_seconds": 120},
      {"mode": "percent", "percentage": 50, "dwell_seconds": 180},
      {"mode": "percent", "percentage": 100, "dwell_seconds": 120}
    ],
    "abort_criteria": {
      "max_drifted_agents": 1,
      "max_error_logs_per_minute": 20,
      "min_dwell_seconds_before_abort": 60
    }
  }'
```

## The rollout state machine

```
                            (operator clicks Pause)
                                     ↓
   pending ──► in_progress ─────► paused ─────► in_progress
                  │  │                              │
                  │  └────── (abort criteria) ──► aborted ──► rolled_back
                  │
                  └────── (final stage done) ──► succeeded
```

- **pending** — created, engine hasn't picked it up yet (usually <5s).
- **in_progress** — actively advancing through stages. The engine watches
  drift + error rate every tick.
- **paused** — operator pressed Pause. Engine no-ops, no stage advance,
  no auto-abort. Resume restarts the dwell clock.
- **succeeded** — final stage cleared its dwell. Terminal.
- **aborted** — operator clicked Abort, or an abort criterion fired.
  Engine picks this up and performs the actual rollback push on its next
  tick.
- **rolled_back** — previous config was pushed back to every canary. Terminal.

## Stages: percent vs label mode

Each stage is a "promotion step": apply the new config to a wider slice of
the group, then wait (dwell) and check abort criteria before going further.

Squadron supports two selection modes for a stage's canary set:

### Percent mode

Pick the first N% of the group's agents, sorted deterministically by ID.

- Cumulative: a stage at 50% includes every agent the 10% stage covered,
  plus more.
- The final stage must reach 100% (validation rejects a percent rollout
  that doesn't hit everyone).
- Use when you want a smooth ramp and your group is large enough that "the
  first 10%" is a meaningful sample.

```json
{"mode": "percent", "percentage": 10, "dwell_seconds": 120}
```

### Label mode

Pick agents whose labels AND-match the selector.

- Selector is `{key: value}` pairs; every pair must match exactly.
- Re-evaluated on every engine tick — agents that gain or lose matching
  labels join or leave the canary live.
- No "must reach 100%" constraint. The final stage is whatever the
  operator decided is "everyone we care about".
- Use when you want a specific host or sub-environment as the canary
  (e.g. one designated canary box per region, or a `tier=staging` shard).

```json
{
  "mode": "label",
  "label_selector": {"host.name": "canary-1"},
  "dwell_seconds": 300
}
```

Mixing modes in one rollout is rejected at create time. Pick one and
commit; you can do two back-to-back rollouts if you really need both
shapes.

### Resolved agent set

Every `rollout.stage_applied` audit event records the resolved
`agent_ids` for that stage. So even though label-mode selection is
dynamic, you can always answer "which exact hosts received the push at
stage 2 at this moment in time?" from the audit log.

## Dwell and abort criteria

Each stage has a `dwell_seconds`: how long to sit before moving on. During
the dwell, the engine ticks every 5 seconds and evaluates the rollout's
**abort criteria**. If any criterion fires, the rollout flips to aborted
and Squadron pushes the previous config back to the canary set.

The criteria today:

- `max_drifted_agents` — if more than N canary agents go drifted during
  the dwell, abort. `0` means "any drift aborts".
- `max_error_logs_per_minute` — if the canary collectively emits more
  than N ERROR-or-higher log records per minute (averaged over the dwell
  window so far), abort. `0` disables the check.
- `min_dwell_seconds_before_abort` — warmup window. The error-rate check
  won't fire until this many seconds have elapsed since the stage started.
  Gives newly-pushed agents time to flush startup noise without false-
  positiving the abort.

All criteria fire independently — drift OR error rate, not AND.

## Cookbook: pre-tuned criteria recipes

The UI ships a small curated cookbook of abort-criteria recipes. Picking
one prefills the three fields above. The list is server-of-record and
accessible at `/api/v1/rollout-recipes/abort-criteria`.

| Recipe                  | Drift | Errors/min | Warmup | When to use                                    |
|-------------------------|-------|------------|--------|------------------------------------------------|
| `strict-canary`         | 0     | 5          | 30s    | High-risk pushes with cross-team blast radius. |
| `standard-production`   | 1     | 20         | 60s    | Default for routine prod tweaks.               |
| `permissive-staging`    | 3     | 100        | 30s    | Non-prod environments with expected churn.     |
| `drift-only`            | 0     | disabled   | 30s    | When error rates aren't a reliable signal.     |
| `manual-abort-only`     | ∞     | disabled   | 0s     | Experimental rollouts; operator is the safety net. |

Operators can hand-tune any field after picking a recipe — the recipe is
just a starting point.

## Templates: pre-built rollout shapes

Templates are one level bigger than recipes: they bundle stages + criteria
+ a default name. The UI's template picker prefills the entire form except
for group ID and target config ID. List at
`/api/v1/rollout-recipes/templates`.

| Template                | Stages                       | Criteria preset       |
|-------------------------|------------------------------|-----------------------|
| `cautious-percent-ramp` | 1% → 10% → 50% → 100% (long dwells) | strict-canary  |
| `standard-percent-ramp` | 10% → 50% → 100% (medium dwells)    | standard-production |
| `fast-percent-ramp`     | 25% → 100% (short dwells)           | permissive-staging  |
| `big-bang`              | 100% (no dwell)                     | manual-abort-only   |

Templates are percent-mode only today. Label-mode rollouts depend on the
operator's specific label scheme, so they're easier to hand-build than to
template against the wrong assumptions.

## Preview and diff

When you pick a group + target config in the create form, Squadron
fetches a preview from `/api/v1/rollout-preview?group_id=X&target_config_id=Y`
and renders:

- A Monaco diff editor showing the current group config vs the target.
- Lint findings on the target config (anti-patterns, undefined components,
  etc.).
- Summary chips: `+N / -M lines`, lint error/warning counts.

The diff fingerprint (added/removed line counts + previous_config_id) is
also persisted into the `rollout.created` audit payload, so a post-mortem
can answer "how big a change was this?" without re-fetching both configs.

If the target is identical to the group's current config, the preview
shows a non-blocking warning. You can still start the rollout (sometimes
useful as a re-push after manual edits) but you almost certainly meant a
different config.

## Pause, resume, abort

- **Pause** stops engine progress without touching the agents. The current
  stage's pushed agents stay on the new config. No stage advance, no
  abort-criteria evaluation. Useful when you want to think.
- **Resume** flips back to in_progress and restarts the stage's dwell
  clock fresh — the engine treats the stage as if it just started, so the
  warmup window applies again. This is the safer default than picking up
  mid-dwell with stale criteria state.
- **Abort** flips to aborted with a reason string (recorded in the audit
  log). The engine performs the actual rollback on its next tick by
  pushing the previous config back to the entire canary set.

```bash
# Abort with a reason
curl -X POST http://localhost:8080/api/v1/rollouts/<id>/abort \
  -H 'Content-Type: application/json' \
  -d '{"reason": "p99 latency spiked on canary"}'

# Pause / resume
curl -X POST http://localhost:8080/api/v1/rollouts/<id>/pause
curl -X POST http://localhost:8080/api/v1/rollouts/<id>/resume
```

## Rolling back a completed rollout (v0.60)

A rollout that succeeded looked fine at the time. Thirty minutes
later, metrics are degrading. The operator wants to undo it.

Squadron's Roll back button (or `POST /rollouts/:id/rollback`)
creates a new rollout that targets the source rollout's previous
config, the one the group was on before the source ran. The new
rollout is itself a normal rollout: it goes through approval if the
source did, flows through the engine, and emits the standard audit
events. The Roll back button is the convenient way to construct
the right `RolloutInput`, not a special bypass path.

```bash
# One click rollback against a completed rollout.
curl -X POST http://localhost:8080/api/v1/rollouts/<id>/rollback
# Returns the new rollout. RolledBackFromID points back at <id>;
# the UI uses that to render a "Rollback" badge on the card and
# chain the two rollouts together on the audit timeline.
```

Constraints:

- The source rollout must be in a terminal state (`succeeded`,
  `aborted`, or `rolled_back`). Operators who want to stop an
  in flight rollout reach for Abort instead.
- The source must have a `previous_config_id`. Brand-new groups
  whose first rollout succeeded have nowhere to roll back to.
- The rollback inherits `require_approval` from the source. If the
  source needed two person approval, so does the rollback — the
  policy applies to the group, not the direction of the change.
- The rollback fires as a single 100% stage with zero dwell because
  the caller is asking for an emergency undo. Operators who want a
  staged rollback can call `Create` directly with the previous
  config as the target.

Audit events: `rollout.rollback_requested` fires on the source
rollout the moment the button is clicked, and the new rollout
emits the normal `rollout.created` plus engine lifecycle events.
Both rollouts carry the same `rolled_back_from_id` link so a query
on either row can find the other.

## Webhook notifications

Set `notification_url` on a rollout and Squadron POSTs a JSON payload on
every state transition:

```json
{
  "rollout_id": "...",
  "name": "ship v2",
  "group_id": "prod-app-collectors",
  "target_config_id": "...",
  "state": "aborted",
  "transition": "aborted",
  "current_stage": 1,
  "total_stages": 3,
  "abort_reason": "2 canary agent(s) drifted (max 1)",
  "at": "2026-05-15T13:42:01.123456789Z"
}
```

Webhook failures (5xx, timeout, DNS) are logged but don't block engine
progress — the audit log captures the durable record.

## Audit trail

Every rollout state transition is recorded in the audit log under
`target_type=rollout, target_id=<rollout_id>`. The full set:

| Event type             | When                          | Payload highlights                        |
|------------------------|-------------------------------|-------------------------------------------|
| `rollout.created`      | Create succeeded              | name, stage_count, **diff_added_lines, diff_removed_lines, previous_config_id** |
| `rollout.stage_applied`| Engine pushed a stage         | stage, mode, canary_size, **agent_ids[]**, percentage or label_selector |
| `rollout.empty_canary` | Stage resolved to 0 agents    | (informational; rollout still proceeds)   |
| `rollout.paused`       | Operator clicked Pause        | —                                         |
| `rollout.resumed`      | Operator clicked Resume       | —                                         |
| `rollout.aborted`      | Auto-abort or manual abort    | reason                                    |
| `rollout.rolled_back`  | Rollback push completed       | —                                         |
| `rollout.succeeded`    | Final stage cleared dwell     | —                                         |

The UI's per-rollout history (click **Show history** on a rollout card)
mounts an [AuditTimeline](./audit-log.md) filtered to the rollout —
operators get the full transcript in one view.

## API reference

| Method | Path                                            | Purpose                              |
|--------|-------------------------------------------------|--------------------------------------|
| GET    | `/api/v1/rollouts`                              | List, with `?group_id=`, `?state=`, `?limit=` |
| POST   | `/api/v1/rollouts`                              | Create. Body = RolloutInput          |
| GET    | `/api/v1/rollouts/:id`                          | Get one                              |
| POST   | `/api/v1/rollouts/:id/pause`                    | Pause                                |
| POST   | `/api/v1/rollouts/:id/resume`                   | Resume                               |
| POST   | `/api/v1/rollouts/:id/abort`                    | Abort. Body `{"reason": "..."}` (optional) |
| GET    | `/api/v1/rollout-preview?group_id=&target_config_id=` | Diff + lint preview          |
| GET    | `/api/v1/rollout-recipes/abort-criteria`        | List recipe cookbook                 |
| GET    | `/api/v1/rollout-recipes/templates`             | List template gallery                |
