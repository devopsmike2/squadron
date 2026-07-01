// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// fakeAbortRolloutSvc satisfies services.RolloutService by embedding the
// interface; only the three methods the abort path touches are given
// bodies. Persist is a no-op and the two plan-walk lookups return empty
// slices so cancelPlanFollowers / rollBackPlanPredecessors emit nothing —
// isolating the assertion to the action-step abort audit rows.
type fakeAbortRolloutSvc struct {
	services.RolloutService
}

func (f *fakeAbortRolloutSvc) Persist(context.Context, *services.Rollout) error {
	return nil
}

func (f *fakeAbortRolloutSvc) CancelPlanFollowers(context.Context, string, int) ([]*services.Rollout, error) {
	return nil, nil
}

func (f *fakeAbortRolloutSvc) RollBackPlanPredecessors(context.Context, string, int, string) ([]*services.Rollout, error) {
	return nil, nil
}

// actionStepRolloutForTest builds a plan-embedded action step whose spec
// decodes to action_type=restart_service.
func actionStepRolloutForTest(t *testing.T) *services.Rollout {
	t.Helper()
	spec, err := json.Marshal(map[string]any{
		"runner_id":       "runner-1",
		"action_type":     "restart_service",
		"timeout_seconds": 300,
	})
	require.NoError(t, err)
	return &services.Rollout{
		ID:              "ro-action-1",
		Name:            "restart step",
		PlanID:          "plan-abcdef1234",
		PlanStepIndex:   2,
		StepKind:        services.StepKindAction,
		NotificationURL: string(spec),
		State:           services.RolloutStateInProgress,
	}
}

func entryByType(entries []services.AuditEntry, eventType string) (services.AuditEntry, bool) {
	for _, e := range entries {
		if e.EventType == eventType {
			return e, true
		}
	}
	return services.AuditEntry{}, false
}

// TestTriggerActionStepAbort_EngineInitiated_EmitsActionFailed pins the
// fix: an engine-initiated action-step termination (timeout / dispatch
// failure / spec-decode failure) — which never reaches
// HandlePostActionResult — must emit BOTH the generic rollout.aborted row
// AND a plan-embedded action.failed row so the plan timeline titles it
// "Action restart_service failed" the same way a runner-reported failure
// is titled. The action.failed payload must carry plan_id +
// plan_step_index + action_type so planEmbeddedActionTitle can render it.
func TestTriggerActionStepAbort_EngineInitiated_EmitsActionFailed(t *testing.T) {
	audit := &recordingAuditService{}
	e := &Engine{auditService: audit, rolloutService: &fakeAbortRolloutSvc{}, logger: zap.NewNop()}
	r := actionStepRolloutForTest(t)

	e.triggerActionStepAbort(context.Background(), r, "action_timeout", true)

	assert.Equal(t, []string{"rollout.aborted", services.AuditEventActionFailed}, audit.eventTypes(),
		"engine-initiated abort must emit rollout.aborted AND the plan-embedded action.failed row")

	failed, ok := entryByType(audit.events, services.AuditEventActionFailed)
	require.True(t, ok, "action.failed row must be present")
	assert.Equal(t, "failed", failed.Action)
	assert.Equal(t, "plan-abcdef1234", failed.Payload["plan_id"])
	assert.Equal(t, 2, failed.Payload["plan_step_index"])
	assert.Equal(t, services.StepKindAction, failed.Payload["step_kind"])
	assert.Equal(t, "restart_service", failed.Payload["action_type"],
		"action_type is what titles the timeline row; must be decoded from the step spec")
	assert.Equal(t, "action_timeout", failed.Payload["reason"])

	assert.Equal(t, services.RolloutStateAborted, r.State)
	assert.Equal(t, "action_timeout", r.AbortReason)
}

// TestTriggerActionStepAbort_RunnerReported_NoDuplicateActionFailed guards
// the no-duplicate invariant: for a runner-reported failure/denied,
// HandlePostActionResult already wrote the action.failed / action.denied
// audit row before the engine polled the terminal status, so the engine
// must emit ONLY rollout.aborted — never a second action.failed.
func TestTriggerActionStepAbort_RunnerReported_NoDuplicateActionFailed(t *testing.T) {
	audit := &recordingAuditService{}
	e := &Engine{auditService: audit, rolloutService: &fakeAbortRolloutSvc{}, logger: zap.NewNop()}
	r := actionStepRolloutForTest(t)

	e.triggerActionStepAbort(context.Background(), r, "action_runtime_failure", false)

	assert.Equal(t, []string{"rollout.aborted"}, audit.eventTypes(),
		"runner-reported abort must NOT re-emit action.failed (the handler already did)")
	_, ok := entryByType(audit.events, services.AuditEventActionFailed)
	assert.False(t, ok, "no engine-side action.failed row for a runner-reported failure")
}

// TestActionStepAuditPayload_BestEffortActionType covers the graceful
// degradation: a decodable step contributes action_type; an undecodable
// one (e.g. the spec-decode-failure path, where StepKind isn't action or
// the spec JSON is gone) omits action_type rather than erroring, so the
// timeline falls back to a bare "Action failed" title.
func TestActionStepAuditPayload_BestEffortActionType(t *testing.T) {
	decodable := actionStepRolloutForTest(t)
	p := actionStepAuditPayload(decodable, map[string]any{"reason": "action_timeout"})
	assert.Equal(t, "plan-abcdef1234", p["plan_id"])
	assert.Equal(t, 2, p["plan_step_index"])
	assert.Equal(t, services.StepKindAction, p["step_kind"])
	assert.Equal(t, "restart_service", p["action_type"])
	assert.Equal(t, "action_timeout", p["reason"])

	// Undecodable spec (no action step body): action_type omitted, no panic.
	undecodable := &services.Rollout{ID: "ro-x", PlanID: "plan-x", PlanStepIndex: 0}
	p2 := actionStepAuditPayload(undecodable, nil)
	assert.Equal(t, "plan-x", p2["plan_id"])
	_, hasType := p2["action_type"]
	assert.False(t, hasType, "action_type must be omitted when the spec can't be decoded")
}
