// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

// v0.89.14 (#630) — action runner steps in plans, slice 1.
//
// This file holds the engine's action-step lifecycle: dispatch on
// predecessor-succeeded promotion, poll the runner's reported
// result each tick, and short-circuit to abort on engine-side
// timeout. The plumbing reuses the existing rollback walk
// (RollBackPlanPredecessors + CancelPlanFollowers) so an action
// failure tears down the rest of the plan exactly the way a
// rollout failure does.
//
// Slice 1 trade-offs (per spec §11):
//   - No in-plan dry-run. The standalone two-phase
//     dry-run/execute path stays for plain action_requests; plan-
//     embedded actions dispatch Phase=execute directly. The plan
//     approval at step 0 covers operator intent.
//   - Action steps in the succeeded prefix are SKIPPED by the
//     backwards rollback walk. Reversal is an action-type
//     property, not a plan property; Squadron has no automatic
//     action undo.
//   - The no-poll-within-timeout/2 heuristic from the spec is NOT
//     implemented in slice 1. The engine relies on the
//     ActionRequest's ExpiresAt window (signer default = 5min,
//     overridable to the spec'd timeout_seconds) as the single
//     source of truth for "too long without progress."

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// processActionStep is the engine's per-tick handler for a kind=
// action plan step. Dispatched-step state machine:
//
//	Pending          → dispatch via ActionDispatcher, set state =
//	                   InProgress, attach the request id.
//	InProgress       → poll the runner's reported status. On
//	                   success advance the plan; on failure /
//	                   denied / expired trigger the abort path.
//	Succeeded /
//	  Aborted /
//	  RolledBack /
//	  Cancelled       → terminal; nothing to do.
//
// PendingApproval and Rejected are reachable when an action step
// is at index 0 with RequireApproval=true. The engine ignores
// them — same posture as a kind=rollout step in those states.
func (e *Engine) processActionStep(ctx context.Context, r *services.Rollout) {
	switch r.State {
	case services.RolloutStatePending:
		e.dispatchActionStep(ctx, r)
	case services.RolloutStateInProgress:
		e.pollActionStep(ctx, r)
	case services.RolloutStateAborted:
		// Aborted action step: no rollback push to make (an action
		// is "did a thing", not "set state X"). The state-change
		// publish + audit already fired when the abort was
		// declared; here we just transition to RolledBack to mark
		// the lifecycle complete so the engine doesn't keep
		// scanning the row on every tick. The plan-level walk
		// (cancelPlanFollowers + rollBackPlanPredecessors) fired
		// at abort-declare time, not here.
		e.finalizeAbortedActionStep(ctx, r)
	}
}

// dispatchActionStep signs + persists the action_request for r and
// transitions r to InProgress. Called from processActionStep when
// the step is in Pending state — which means either the engine
// just promoted it from Queued (predecessor succeeded path) or
// step 0 was created without RequireApproval. The dispatcher is
// optional at the engine level; nil disables action-step support
// (the step sits in Pending forever, which a deployment that
// hasn't wired the signing key tolerates as a no-op).
func (e *Engine) dispatchActionStep(ctx context.Context, r *services.Rollout) {
	if e.actionDispatcher == nil {
		e.logger.Warn("plan engine: action step pending but no dispatcher configured",
			zap.String("rollout_id", r.ID),
			zap.String("plan_id", r.PlanID),
			zap.Int("step_index", r.PlanStepIndex))
		return
	}
	spec, err := services.DecodeActionStepSpec(r)
	if err != nil {
		e.logger.Warn("plan engine: decode action spec failed; aborting step",
			zap.String("rollout_id", r.ID),
			zap.Error(err))
		e.triggerActionStepAbort(ctx, r, "action_spec_decode_failed", true)
		return
	}
	actor := r.RequestedBy
	if actor == "" {
		actor = "system:plan_engine"
	}
	requestID, _, err := e.actionDispatcher.Dispatch(ctx, r.PlanID, r.PlanStepIndex, *spec, actor)
	if err != nil {
		e.logger.Warn("plan engine: action dispatch failed",
			zap.String("rollout_id", r.ID),
			zap.String("plan_id", r.PlanID),
			zap.Int("step_index", r.PlanStepIndex),
			zap.Error(err))
		e.triggerActionStepAbort(ctx, r, "action_dispatch_failed", true)
		return
	}
	now := time.Now().UTC()
	r.State = services.RolloutStateInProgress
	r.StageStartedAt = &now
	r.ActionRequestID = requestID
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("plan engine: persist after action dispatch failed",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.publishStateChange(r, "action_dispatched")
	e.logger.Info("plan engine: action step dispatched",
		zap.String("rollout_id", r.ID),
		zap.String("plan_id", r.PlanID),
		zap.Int("step_index", r.PlanStepIndex),
		zap.String("action_type", spec.ActionType),
		zap.String("runner_id", spec.RunnerID),
		zap.String("action_request_id", requestID))
}

// pollActionStep checks the action_request status. The runner
// reports success / failure / denied via the existing
// HandlePostActionResult endpoint; the engine simply reads the
// row and advances the plan based on what landed. Pending +
// in-flight reports stay quiet until the runner posts a terminal
// status or the engine's timeout sweep declares the step expired.
func (e *Engine) pollActionStep(ctx context.Context, r *services.Rollout) {
	if e.actionDispatcher == nil {
		return
	}
	if r.ActionRequestID == "" {
		e.logger.Warn("plan engine: in-progress action step has no request id",
			zap.String("rollout_id", r.ID))
		return
	}
	status, deniedFor, expiresAt, err := e.actionDispatcher.GetStatus(ctx, r.ActionRequestID)
	if err != nil {
		e.logger.Warn("plan engine: poll action status failed",
			zap.String("rollout_id", r.ID),
			zap.String("action_request_id", r.ActionRequestID),
			zap.Error(err))
		return
	}
	switch status {
	case "success":
		e.finishActionStep(ctx, r)
	case "failure":
		// Runner-reported: HandlePostActionResult already wrote the
		// action.failed audit row, so don't duplicate it here.
		e.triggerActionStepAbort(ctx, r, deniedReason(deniedFor, "action_runtime_failure"), false)
	case "denied":
		// Runner-reported: action.denied already audited by the handler.
		e.triggerActionStepAbort(ctx, r, deniedReason(deniedFor, "action_denied"), false)
	default:
		// pending / unknown — apply the engine-side timeout. The
		// dispatcher returns the action_request's ExpiresAt which
		// the signer set to IssuedAt + timeout_seconds at dispatch
		// time. Once we're past it, declare action_timeout and
		// trigger the backwards walk.
		if !expiresAt.IsZero() && time.Now().UTC().After(expiresAt) {
			// Engine-side timeout sweep — the runner never posted a
			// result, so emit the action.failed audit row ourselves.
			e.triggerActionStepAbort(ctx, r, "action_timeout", true)
		}
	}
}

// deniedReason picks a human-readable abort reason for an action
// step that the runner rejected. The runner's posted denied_for
// field is the most specific value when present; otherwise we
// fall back to the generic category. Mirrors the verb the spec
// §6 expects on the action.failed / action.denied audit event.
func deniedReason(runnerReported, fallback string) string {
	if runnerReported != "" {
		return runnerReported
	}
	return fallback
}

// finishActionStep marks the action step as succeeded and lets the
// plan advance to the next step. Mirrors the rollout-step finish
// path (see engine.go finish()) — both call advancePlan to
// promote the next step out of Queued, which is what closes the
// arc when the action step is the last step in the plan.
func (e *Engine) finishActionStep(ctx context.Context, r *services.Rollout) {
	r.State = services.RolloutStateSucceeded
	now := time.Now().UTC()
	r.CompletedAt = &now
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("plan engine: persist action succeeded failed",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.publishStateChange(r, "action_succeeded")
	e.logger.Info("plan engine: action step succeeded",
		zap.String("rollout_id", r.ID),
		zap.String("plan_id", r.PlanID),
		zap.Int("step_index", r.PlanStepIndex))
	if r.PlanID != "" {
		e.advancePlan(ctx, r)
	}
}

// actionStepAuditPayload builds the payload shared by the plan-
// embedded action lifecycle audit rows. plan_id + plan_step_index
// are the keys the timeline's planEmbeddedActionTitle renderer uses
// to title the row "Action <type> …"; action_type is best-effort
// (the spec is decoded from the step when it's still decodable) so
// the title matches the runner-reported action.* rows that
// HandlePostActionResult writes. extra keys (e.g. reason) merge last.
func actionStepAuditPayload(r *services.Rollout, extra map[string]any) map[string]any {
	p := map[string]any{
		"plan_id":         r.PlanID,
		"plan_step_index": r.PlanStepIndex,
		"step_kind":       services.StepKindAction,
	}
	if spec, err := services.DecodeActionStepSpec(r); err == nil && spec != nil {
		p["action_type"] = spec.ActionType
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// triggerActionStepAbort flips an in-progress action step to
// Aborted with the supplied reason, then runs the same plan-level
// walk a kind=rollout failure does (cancel followers + roll back
// succeeded predecessors). The RollBackPlanPredecessors
// implementation skips action steps in the succeeded prefix per
// spec §5 — action reversal is an action-type property, not a
// plan property — so the engine doesn't have to filter here.
//
// engineInitiated marks the terminations the runner never posted a
// result for (spec-decode failure, dispatch failure, the engine-
// side timeout sweep). Runner-reported failure/denied paths pass
// false: HandlePostActionResult already wrote their action.failed /
// action.denied audit row (with plan context) before the engine
// polled the terminal status, so re-emitting here would duplicate.
func (e *Engine) triggerActionStepAbort(ctx context.Context, r *services.Rollout, reason string, engineInitiated bool) {
	r.State = services.RolloutStateAborted
	r.AbortReason = reason
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("plan engine: persist action abort failed",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.recordAudit(ctx, r, "rollout.aborted", "aborted", map[string]any{
		"reason":          reason,
		"plan_id":         r.PlanID,
		"plan_step_index": r.PlanStepIndex,
		"step_kind":       services.StepKindAction,
	})
	// Engine-initiated terminations bypass HandlePostActionResult, so
	// without this they'd carry only the generic rollout.aborted row
	// and render as "Rollout aborted" instead of "Action <type>
	// failed" on the plan timeline. Emit the action.failed row so an
	// engine-detected action failure is audited + titled exactly like
	// a runner-reported one.
	if engineInitiated {
		e.recordAudit(ctx, r, services.AuditEventActionFailed, "failed",
			actionStepAuditPayload(r, map[string]any{"reason": reason}))
	}
	e.publishStateChange(r, "aborted")
	e.logger.Warn("plan engine: action step aborted",
		zap.String("rollout_id", r.ID),
		zap.String("plan_id", r.PlanID),
		zap.Int("step_index", r.PlanStepIndex),
		zap.String("reason", reason))

	// On timeout the runner might still be working — mark the
	// underlying request expired so it's not picked up later.
	// Best effort: errors are logged but don't block the walk.
	if r.ActionRequestID != "" && e.actionDispatcher != nil {
		if err := e.actionDispatcher.Cancel(ctx, r.ActionRequestID, reason); err != nil {
			e.logger.Warn("plan engine: cancel action request after abort failed",
				zap.String("rollout_id", r.ID),
				zap.String("action_request_id", r.ActionRequestID),
				zap.Error(err))
		}
	}

	// Plan-level walk: cancel queued followers, roll back
	// succeeded predecessors. The walk handles the no-followers
	// and no-predecessors cases internally so we don't have to
	// branch here.
	if r.PlanID != "" {
		e.cancelPlanFollowers(ctx, r, reason)
		e.rollBackPlanPredecessors(ctx, r, reason)
	}
}

// finalizeAbortedActionStep moves an aborted action step out of
// the engine's active scan set by transitioning it to
// RolledBack. The rollout path uses an actual rollback config
// push to undo the change; action steps have no equivalent (the
// action either ran or didn't), so the transition is a bookkeeping
// no-op that lets IsTerminal() return true on the next List
// query and stops the row from showing up in every tick.
func (e *Engine) finalizeAbortedActionStep(ctx context.Context, r *services.Rollout) {
	r.State = services.RolloutStateRolledBack
	now := time.Now().UTC()
	if r.CompletedAt == nil {
		r.CompletedAt = &now
	}
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("plan engine: finalize aborted action step failed",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.publishStateChange(r, "action_aborted_finalized")
}
