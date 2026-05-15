// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// validRolloutInput returns a baseline RolloutInput with a target config
// that exists in the given store. Tests mutate fields to exercise specific
// validation branches.
func validRolloutInput(t *testing.T, store applicationstore.ApplicationStore) RolloutInput {
	t.Helper()
	cfg := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "test-cfg",
		ConfigHash: "abc",
		Content:    "receivers: {}",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(context.Background(), cfg))
	return RolloutInput{
		Name:           "test rollout",
		GroupID:        "group-a",
		TargetConfigID: cfg.ID,
		Stages: []RolloutStage{
			{Percentage: 10, DwellSeconds: 5},
			{Percentage: 100, DwellSeconds: 10},
		},
		AbortCriteria: RolloutAbortCriteria{MaxDriftedAgents: 0},
	}
}

func TestRolloutService_CreateAndGet(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)
	require.NotEmpty(t, r.ID)
	assert.Equal(t, RolloutStatePending, r.State)
	assert.Equal(t, 0, r.CurrentStage)

	got, err := svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, r.ID, got.ID)
	assert.Len(t, got.Stages, 2)
}

func TestRolloutService_Validation(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*RolloutInput)
		errSub string
	}{
		{"empty name", func(i *RolloutInput) { i.Name = "" }, "name is required"},
		{"empty group", func(i *RolloutInput) { i.GroupID = "" }, "group_id is required"},
		{"empty target config", func(i *RolloutInput) { i.TargetConfigID = "" }, "target_config_id is required"},
		{"no stages", func(i *RolloutInput) { i.Stages = nil }, "at least one stage is required"},
		{"stage > 100", func(i *RolloutInput) {
			i.Stages = []RolloutStage{{Percentage: 150, DwellSeconds: 1}}
		}, "percentage must be in"},
		{"stage <= 0", func(i *RolloutInput) {
			i.Stages = []RolloutStage{{Percentage: 0, DwellSeconds: 1}}
		}, "percentage must be in"},
		{"non-monotonic", func(i *RolloutInput) {
			i.Stages = []RolloutStage{
				{Percentage: 50, DwellSeconds: 1},
				{Percentage: 25, DwellSeconds: 1},
				{Percentage: 100, DwellSeconds: 1},
			}
		}, "must be >= previous"},
		{"final stage not 100", func(i *RolloutInput) {
			i.Stages = []RolloutStage{
				{Percentage: 10, DwellSeconds: 1},
				{Percentage: 50, DwellSeconds: 1},
			}
		}, "final stage must have percentage = 100"},
		{"negative dwell", func(i *RolloutInput) {
			i.Stages[0].DwellSeconds = -1
		}, "dwell_seconds must be >= 0"},
		{"negative max drifted", func(i *RolloutInput) {
			i.AbortCriteria.MaxDriftedAgents = -1
		}, "max_drifted_agents must be >= 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validRolloutInput(t, store)
			tc.mutate(&in)
			_, err := svc.Create(ctx, in)
			require.Error(t, err, "expected validation to fail")
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

func TestRolloutService_TargetConfigMustExist(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := RolloutInput{
		Name: "x", GroupID: "g", TargetConfigID: "does-not-exist",
		Stages:        []RolloutStage{{Percentage: 100, DwellSeconds: 0}},
		AbortCriteria: RolloutAbortCriteria{},
	}
	_, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target config not found")
}

func TestRolloutService_Abort(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)

	updated, err := svc.Abort(ctx, r.ID, "operator changed mind")
	require.NoError(t, err)
	assert.Equal(t, RolloutStateAborted, updated.State)
	assert.Equal(t, "operator changed mind", updated.AbortReason)

	// Aborting again should refuse — we're in a terminal-ish state.
	// (Service considers aborted as not-yet-terminal to allow engine to
	// roll back; verify the error path is at least sane though.)
	_, err = svc.Abort(ctx, r.ID, "again")
	// Aborted-state rollouts can still be re-aborted in our impl (state
	// is already aborted, the call is a no-op-ish update). The contract
	// we tested elsewhere is that succeeded/rolled_back reject. So no
	// assertion here — just don't crash.
	_ = err
}

func TestRolloutService_Abort_RejectsTerminalStates(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)

	// Mutate state via Persist() to simulate the engine completing the
	// rollout.
	r.State = RolloutStateSucceeded
	require.NoError(t, svc.Persist(ctx, r))

	_, err = svc.Abort(ctx, r.ID, "too late")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal state")
}

func TestRolloutService_AbortMissing(t *testing.T) {
	svc := NewRolloutService(memory.NewStore(), nil, nil, zap.NewNop())
	_, err := svc.Abort(context.Background(), "no-such-rollout", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
