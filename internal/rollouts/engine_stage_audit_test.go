// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// recordingAuditService captures every Record call so the stage-emit tests can
// assert on the exact audit events produced. Embeds the interface so the three
// unused methods (List/Get/SetExplanation) satisfy it without bodies.
type recordingAuditService struct {
	services.AuditService
	events []services.AuditEntry
}

func (r *recordingAuditService) Record(_ context.Context, e services.AuditEntry) error {
	r.events = append(r.events, e)
	return nil
}

func (r *recordingAuditService) eventTypes() []string {
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.EventType
	}
	return out
}

// TestRecordStageApplied_ZeroAgentStageEmitsEmptyCanaryAudit pins the fix: a
// stage that resolves to zero agents must emit BOTH rollout.stage_applied AND
// rollout.empty_canary audit rows. The advance path used to record only the
// empty_canary TRACE event, dropping the audit row — this regression-guards the
// shared helper both start() and advanceOrCheck() now route through.
func TestRecordStageApplied_ZeroAgentStageEmitsEmptyCanaryAudit(t *testing.T) {
	audit := &recordingAuditService{}
	e := &Engine{auditService: audit, tracer: nil, logger: zap.NewNop()}
	r := &services.Rollout{
		ID:      "rollout-1",
		Name:    "empty-selector",
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Percentage: 10}},
	}

	e.recordStageApplied(context.Background(), r, nil) // zero agents

	assert.Equal(t, []string{"rollout.stage_applied", "rollout.empty_canary"}, audit.eventTypes(),
		"a zero-agent stage must emit both the stage_applied and empty_canary audit rows")
	// The empty_canary payload must report canary_size 0 so a post-mortem sees why.
	last := audit.events[len(audit.events)-1]
	assert.Equal(t, 0, last.Payload["canary_size"])
	assert.Equal(t, "empty_canary", last.Action)
	assert.Equal(t, "rollout-1", last.TargetID)
}

// TestRecordStageApplied_NonEmptyStageOmitsEmptyCanary confirms the gate: a
// stage that moved at least one agent emits only stage_applied — no spurious
// empty_canary row.
func TestRecordStageApplied_NonEmptyStageOmitsEmptyCanary(t *testing.T) {
	audit := &recordingAuditService{}
	e := &Engine{auditService: audit, tracer: nil, logger: zap.NewNop()}
	r := &services.Rollout{
		ID:      "rollout-2",
		Name:    "has-agents",
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Percentage: 50}},
	}

	e.recordStageApplied(context.Background(), r, []uuid.UUID{{0x01}, {0x02}})

	assert.Equal(t, []string{"rollout.stage_applied"}, audit.eventTypes(),
		"a non-empty stage must emit only stage_applied")
}

// TestRecordStageApplied_NilAuditServiceSafe guards the optional-audit path:
// with no audit service wired the helper still runs (trace-only) without
// panicking.
func TestRecordStageApplied_NilAuditServiceSafe(t *testing.T) {
	e := &Engine{auditService: nil, tracer: nil, logger: zap.NewNop()}
	r := &services.Rollout{ID: "r", Stages: []services.RolloutStage{{Percentage: 10}}}
	assert.NotPanics(t, func() { e.recordStageApplied(context.Background(), r, nil) })
}
