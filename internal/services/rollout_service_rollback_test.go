// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// rollbackSetup creates a minimal store seeded with a group and two
// configs (the "previous" the rollback will target, plus the "target"
// the source rollout pushed), then a single succeeded rollout. The
// resulting state mirrors the common operator workflow: a rollout
// completed; metrics regressed; the operator wants to undo.
func rollbackSetup(t *testing.T) (*RolloutServiceImpl, *types.Rollout) {
	t.Helper()
	store := memory.NewStore()
	ctx := context.Background()

	groupID := "web-prod"
	require.NoError(t, store.CreateGroup(ctx, &types.Group{
		ID:   groupID,
		Name: "Web Prod",
	}))

	gid := groupID
	prevCfg := &types.Config{
		ID:        "cfg-previous",
		Name:      "Previous",
		Content:   "receivers: { otlp: {} }",
		GroupID:   &gid,
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, store.CreateConfig(ctx, prevCfg))
	tgtCfg := &types.Config{
		ID:        "cfg-target",
		Name:      "Target",
		Content:   "receivers: { otlp: {} }\nprocessors: { batch: {} }",
		CreatedAt: time.Now().Add(-1 * time.Hour),
	}
	require.NoError(t, store.CreateConfig(ctx, tgtCfg))

	svc := &RolloutServiceImpl{
		appStore: store,
		logger:   zap.NewNop(),
	}

	// Seed a completed rollout via Create + post-hoc state transition.
	created, err := svc.Create(ctx, RolloutInput{
		Name:           "Promote target config",
		GroupID:        groupID,
		TargetConfigID: tgtCfg.ID,
		Stages: []RolloutStage{
			{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
	})
	require.NoError(t, err)
	created.State = RolloutStateSucceeded
	require.NoError(t, svc.Persist(ctx, created))

	// Re-read the seeded rollout from storage (typed) so the test
	// can pass it into RollBack the way a handler would.
	stored, err := store.GetRollout(ctx, created.ID)
	require.NoError(t, err)
	return svc, stored
}

func TestRollBack_CreatesRollbackRollout(t *testing.T) {
	svc, source := rollbackSetup(t)

	rb, err := svc.RollBack(context.Background(), source.ID, "alice@example.com")
	require.NoError(t, err)

	assert.NotEqual(t, source.ID, rb.ID, "rollback gets its own ID")
	assert.Equal(t, source.GroupID, rb.GroupID)
	assert.Equal(t, source.PreviousConfigID, rb.TargetConfigID,
		"rollback targets the source's previous config")
	assert.Equal(t, source.ID, rb.RolledBackFromID)
	assert.Equal(t, "alice@example.com", rb.RequestedBy)
	assert.Equal(t, RolloutProposedByOperator, rb.ProposedBy)
	assert.True(t, strings.HasPrefix(rb.Name, "Rollback of:"))
	require.Len(t, rb.Stages, 1)
	assert.Equal(t, 100, rb.Stages[0].Percentage,
		"emergency rollback fires at 100% immediately")
	assert.Equal(t, 0, rb.Stages[0].DwellSeconds)
}

func TestRollBack_RefusesNonTerminalSource(t *testing.T) {
	svc, source := rollbackSetup(t)
	svcSource := toServiceRollout(source)
	svcSource.State = RolloutStateInProgress
	require.NoError(t, svc.Persist(context.Background(), svcSource))

	_, err := svc.RollBack(context.Background(), source.ID, "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal state")
}

func TestRollBack_RefusesEmptyPreviousConfig(t *testing.T) {
	svc, source := rollbackSetup(t)
	svcSource := toServiceRollout(source)
	svcSource.PreviousConfigID = ""
	require.NoError(t, svc.Persist(context.Background(), svcSource))

	_, err := svc.RollBack(context.Background(), source.ID, "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "previous config")
}

func TestRollBack_RefusesMissingRollout(t *testing.T) {
	svc, _ := rollbackSetup(t)
	_, err := svc.RollBack(context.Background(), "does-not-exist", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRollBack_CarriesApprovalRequirementForward(t *testing.T) {
	svc, source := rollbackSetup(t)
	svcSource := toServiceRollout(source)
	svcSource.RequireApproval = true
	require.NoError(t, svc.Persist(context.Background(), svcSource))

	rb, err := svc.RollBack(context.Background(), source.ID, "alice")
	require.NoError(t, err)
	assert.True(t, rb.RequireApproval,
		"if the source needed approval, so does the rollback")
	assert.Equal(t, RolloutStatePendingApproval, rb.State)
}

// v0.61 — a group can require approval on rollbacks even when the
// source rollout did not require approval. This is the policy the
// compliance-strict operator wants: regular rollouts pass through but
// undo is treated as more dangerous and gates on a second operator.
func TestRollBack_GroupPolicyForcesApprovalOnRollback(t *testing.T) {
	svc, source := rollbackSetup(t)
	ctx := context.Background()

	g, err := svc.appStore.GetGroup(ctx, source.GroupID)
	require.NoError(t, err)
	require.NotNil(t, g)
	g.RequireApprovalForRollback = true
	require.NoError(t, svc.appStore.UpdateGroup(ctx, g))

	// Source had no approval requirement of its own.
	require.False(t, source.RequireApproval)

	rb, err := svc.RollBack(ctx, source.ID, "alice")
	require.NoError(t, err)
	assert.True(t, rb.RequireApproval,
		"group's rollback policy forces approval on the new rollout")
	assert.Equal(t, RolloutStatePendingApproval, rb.State)
}

// And when the policy is off the rollback should pass through as
// before — the policy is opt-in, not the new default.
func TestRollBack_GroupPolicyOffLeavesApprovalAlone(t *testing.T) {
	svc, source := rollbackSetup(t)
	ctx := context.Background()

	g, err := svc.appStore.GetGroup(ctx, source.GroupID)
	require.NoError(t, err)
	require.False(t, g.RequireApprovalForRollback,
		"setup leaves the policy off")
	require.False(t, source.RequireApproval)

	rb, err := svc.RollBack(ctx, source.ID, "alice")
	require.NoError(t, err)
	assert.False(t, rb.RequireApproval,
		"no policy + no source approval = no approval gate on rollback")
	assert.NotEqual(t, RolloutStatePendingApproval, rb.State)
}
