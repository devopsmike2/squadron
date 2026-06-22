// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

// v0.89.14 (#630) — action runner steps in plans, slice 1.
//
// Plan steps gain a third kind alongside the v0.69 default "rollout"
// and the v0.79 "plan" (nested-plan) variants: "action". An action
// step dispatches a signed action_request to a runner mid-plan, lets
// the action complete (success → advance; failure / denied / timeout
// → abort + backwards rollback walk), and feeds the runner's
// reported result back into the plan's lifecycle.
//
// This file holds the service-layer validators and the storage-only
// create path for kind=action steps. The forward-walk dispatcher
// lives in the rollout engine (internal/rollouts/engine.go) and
// reaches into ActionDispatcher (defined here) to sign + persist the
// action_requests row and to consult the runner's status feedback
// loop. The plan create handler enforces the scope check (rollouts
// :write + actions:write) per spec §7 + acceptance test #5.
//
// See docs/proposals/530-action-runner-steps-in-plans.md for the
// full design.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MaxActionStepTimeoutSeconds clamps how long the plan engine waits
// for a runner result before declaring the step a failure. Per spec
// §4, the model defaults TimeoutSeconds to 300 (5 minutes) and the
// hard ceiling is 3600 (1 hour). Anything above 3600 is almost
// always a misconfiguration: longer-running ops shouldn't block the
// whole plan tick loop, and the action runner's own
// signature/expires_at envelope is sized to the same window. The
// validator clamps with a precise error rather than silently
// capping so the operator sees what the system rejected.
const (
	DefaultActionStepTimeoutSeconds = 300
	MaxActionStepTimeoutSeconds     = 3600
)

// validateActionStepInput catches obvious problems on a kind=action
// step before any storage write fires. Mirrors the pre-flight pass
// CreatePlan does for kind=rollout steps; called from CreatePlan
// only.
//
// Acceptance test #4 from the spec exercises three rejection paths
// here: action block present alongside a rollout-only field (any of
// target_config_id / inline_config_snippet / stages / abort_criteria
// non-zero), kind=action with a nil Action block, and the explicit
// mismatch cases (kind=action with TargetConfigID set). The error
// strings include the step index so the handler-level 400 response
// names exactly which step the operator needs to fix.
func validateActionStepInput(index int, step RolloutInput) error {
	if step.Action == nil {
		return fmt.Errorf("plan step %d kind=action requires an action block", index)
	}
	// Forbid the rollout-only fields. The proposer or operator may
	// have populated these from a template before flipping the kind
	// to action; the explicit rejection prevents an action step from
	// silently inheriting rollout semantics it has no way to honor.
	if strings.TrimSpace(step.TargetConfigID) != "" {
		return fmt.Errorf("plan step %d kind=action must not set target_config_id", index)
	}
	if strings.TrimSpace(step.InlineConfigSnippet) != "" {
		return fmt.Errorf("plan step %d kind=action must not set inline_config_snippet", index)
	}
	if len(step.Stages) > 0 {
		return fmt.Errorf("plan step %d kind=action must not set stages", index)
	}
	if zero := (RolloutAbortCriteria{}); step.AbortCriteria != zero {
		return fmt.Errorf("plan step %d kind=action must not set abort_criteria", index)
	}
	// Action-block content checks.
	if strings.TrimSpace(step.Action.RunnerID) == "" {
		return fmt.Errorf("plan step %d action.runner_id is required", index)
	}
	if strings.TrimSpace(step.Action.ActionType) == "" {
		return fmt.Errorf("plan step %d action.action_type is required", index)
	}
	if step.Action.TimeoutSeconds < 0 {
		return fmt.Errorf("plan step %d action.timeout_seconds must be non-negative", index)
	}
	if step.Action.TimeoutSeconds > MaxActionStepTimeoutSeconds {
		return fmt.Errorf("plan step %d action.timeout_seconds %d exceeds maximum %d",
			index, step.Action.TimeoutSeconds, MaxActionStepTimeoutSeconds)
	}
	return nil
}

// createActionStep persists a kind=action plan step. The step lands
// in Queued (for indices 1..N) or Pending (for index 0) the same
// way a kind=rollout step does; the engine's forward walk picks it
// up and dispatches the action_request on the predecessor's
// succeeded transition. The action_request row itself is NOT
// created here — only the plan-step shell. Dispatch happens in the
// engine so the signing key and the runner registry stay on the
// engine side, where they already live.
//
// Slice 1 stores the action spec (runner id, action type,
// parameters, timeout) in NotificationURL as a JSON blob — the
// column already exists, it's optional, and rollout steps don't
// use it for plans (the plan-level webhook fan-out runs against
// the envelope, not per step). This avoids a second schema
// migration on top of the step_kind + action_request_id pair the
// spec §4 already calls for. The engine reads the blob back when
// it dispatches; the field is otherwise opaque. Resolution of
// spec §11 Q3 — reuse, don't isolate.
func (s *RolloutServiceImpl) createActionStep(ctx context.Context, input RolloutInput, planID string, stepIndex int) (*Rollout, error) {
	if input.Action == nil {
		// Should be unreachable — the pre-flight validator catches this.
		return nil, fmt.Errorf("plan step %d kind=action requires an action block", stepIndex)
	}
	timeout := input.Action.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultActionStepTimeoutSeconds
	}
	spec := storedActionStepSpec{
		RunnerID:       strings.TrimSpace(input.Action.RunnerID),
		ActionType:     strings.TrimSpace(input.Action.ActionType),
		Parameters:     input.Action.Parameters,
		TimeoutSeconds: timeout,
	}
	if spec.Parameters == nil {
		// Empty JSON object is a stable carrier for action types that
		// take no parameters (none ship today, but the runner-side
		// schema validators reject malformed JSON outright).
		spec.Parameters = json.RawMessage("{}")
	}
	blob, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("plan step %d action spec marshal: %w", stepIndex, err)
	}

	// Step state: step 0 honors RequireApproval (the plan's gate);
	// steps 1..N land in Queued the same way rollout steps do.
	state := RolloutStatePending
	if input.RequireApproval {
		state = RolloutStatePendingApproval
	}
	if stepIndex > 0 {
		state = RolloutStateQueued
	}

	now := time.Now().UTC()
	r := &Rollout{
		ID:      uuid.New().String(),
		Name:    input.Name,
		GroupID: input.GroupID,
		// TargetConfigID intentionally empty — action steps point at
		// a runner verb, not a config. The plan envelope and the
		// engine both branch on StepKind before reading
		// TargetConfigID, so the empty value is never dereferenced.
		State:           state,
		RequireApproval: input.RequireApproval && stepIndex == 0,
		RequestedBy:     input.RequestedBy,
		ProposedBy: func() string {
			if input.ProposedBy == "" {
				return RolloutProposedByOperator
			}
			return input.ProposedBy
		}(),
		ProposalReasoning: input.ProposalReasoning,
		EvidenceRefs:      input.EvidenceRefs,
		PlanID:            planID,
		PlanStepIndex:     stepIndex,
		StepKind:          StepKindAction,
		// NotificationURL doubles as the action-spec carrier so the
		// engine can rehydrate the spec at dispatch time without a
		// schema migration. See the function comment above.
		NotificationURL: string(blob),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.appStore.CreateRollout(ctx, toStorageRollout(r)); err != nil {
		return nil, fmt.Errorf("persist action step: %w", err)
	}
	s.logger.Info("created plan action step",
		zap.String("plan_id", planID),
		zap.Int("step_index", stepIndex),
		zap.String("runner_id", spec.RunnerID),
		zap.String("action_type", spec.ActionType),
	)
	return r, nil
}

// storedActionStepSpec is the wire shape the action-step's spec
// takes when persisted on the rollout row's NotificationURL column
// (see createActionStep). The engine deserializes this at dispatch
// time. Keeping it private to the services package keeps the
// storage encoding off the public surface.
type storedActionStepSpec struct {
	RunnerID       string          `json:"runner_id"`
	ActionType     string          `json:"action_type"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
}

// DecodeActionStepSpec rehydrates the spec the plan engine needs to
// sign + dispatch an action_request. Exported so the engine
// (internal/rollouts) can call it without importing the JSON
// encoding internals. Returns a typed ActionStepSpec the caller can
// use directly with the action runner signer.
func DecodeActionStepSpec(r *Rollout) (*ActionStepSpec, error) {
	if r == nil {
		return nil, fmt.Errorf("nil rollout")
	}
	if r.StepKind != StepKindAction {
		return nil, fmt.Errorf("rollout %s is not an action step (kind=%q)", r.ID, r.StepKind)
	}
	if r.NotificationURL == "" {
		return nil, fmt.Errorf("rollout %s has no action spec", r.ID)
	}
	var stored storedActionStepSpec
	if err := json.Unmarshal([]byte(r.NotificationURL), &stored); err != nil {
		return nil, fmt.Errorf("decode action spec for rollout %s: %w", r.ID, err)
	}
	timeout := stored.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultActionStepTimeoutSeconds
	}
	return &ActionStepSpec{
		RunnerID:       stored.RunnerID,
		ActionType:     stored.ActionType,
		Parameters:     stored.Parameters,
		TimeoutSeconds: timeout,
	}, nil
}

// HasActionStep reports whether any step in steps is kind=action.
// Used by the plan create handler to decide whether to require the
// caller's token to carry actions:write in addition to
// rollouts:write (per spec §7 + acceptance test #5).
func HasActionStep(steps []RolloutInput) bool {
	for _, s := range steps {
		if s.Kind == StepKindAction {
			return true
		}
	}
	return false
}

// ActionDispatcher is the plan-engine boundary to the action runner
// substrate. The engine implements forward-walk dispatch + result
// ingestion against this interface so the engine package doesn't
// pull in the action signer + registry directly; the wire layer in
// cmd/all-in-one builds the concrete dispatcher and hands it to
// NewEngineWithActionDispatcher. v0.89.14 (#630).
//
// Dispatch signs a fresh action_request from the supplied spec,
// persists it with status=pending and plan-embedded provenance in
// the audit payload, and returns the request id so the engine can
// attach it to the plan step.
//
// GetStatus reads the runner's most-recent reported status off the
// action_requests row (pending / success / failure / denied), plus
// the ExpiresAt timestamp the engine uses for its timeout sweep.
//
// Cancel marks an in-flight request as expired so a plan that the
// operator aborts mid-action doesn't leave a dangling pending row
// for the runner to pick up. The runner-side cancellation is
// best-effort (the runner may have already started executing); the
// audit trail records the engine-side cancel either way.
type ActionDispatcher interface {
	Dispatch(ctx context.Context, planID string, stepIndex int, spec ActionStepSpec, actor string) (requestID string, expiresAt time.Time, err error)
	GetStatus(ctx context.Context, requestID string) (status string, deniedFor string, expiresAt time.Time, err error)
	Cancel(ctx context.Context, requestID string, reason string) error
}
