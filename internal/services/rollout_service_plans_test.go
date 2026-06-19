// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// v0.69 — the storage slice of multi step plans. The engine doesn't
// sequence steps yet, but PlanID + PlanStepIndex must round trip
// cleanly through Create + Get so the engine work in v0.70 has a
// stable contract to land on.

func TestRollout_PlanFieldsRoundTrip(t *testing.T) {
	store := memory.NewStore()
	ctx := context.Background()

	groupID := "web-prod"
	require.NoError(t, store.CreateGroup(ctx, &types.Group{
		ID:   groupID,
		Name: "Web Prod",
	}))

	gid := groupID
	cfg := &types.Config{
		ID:        "cfg-target",
		Name:      "Target",
		Content:   "receivers: { otlp: {} }",
		GroupID:   &gid,
		CreatedAt: time.Now().Add(-1 * time.Hour),
	}
	require.NoError(t, store.CreateConfig(ctx, cfg))

	svc := &RolloutServiceImpl{
		appStore: store,
		logger:   zap.NewNop(),
	}

	// Create step 0 of a three step plan. The other steps would be
	// created by the same caller in the engine work; v0.69 only
	// proves the storage contract for one step here.
	created, err := svc.Create(ctx, RolloutInput{
		Name:           "Plan abc step 0: drop noisy attr",
		GroupID:        groupID,
		TargetConfigID: cfg.ID,
		Stages: []RolloutStage{
			{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
		PlanID:        "plan-abc",
		PlanStepIndex: 0,
	})
	require.NoError(t, err)
	assert.Equal(t, "plan-abc", created.PlanID)
	assert.Equal(t, 0, created.PlanStepIndex)

	// Read back through the storage layer to prove the conversion
	// functions round trip both fields.
	stored, err := store.GetRollout(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "plan-abc", stored.PlanID)
	assert.Equal(t, 0, stored.PlanStepIndex)

	// And the service layer read should see the same values.
	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "plan-abc", got.PlanID)
	assert.Equal(t, 0, got.PlanStepIndex)
}

// A standalone rollout has empty PlanID and step 0 — the v0.4 through
// v0.68 default. The new fields must not change that behavior.
func TestRollout_StandaloneHasEmptyPlanFields(t *testing.T) {
	store := memory.NewStore()
	ctx := context.Background()
	gid := "g-1"
	require.NoError(t, store.CreateGroup(ctx, &types.Group{ID: gid, Name: "G"}))
	cfg := &types.Config{
		ID: "c-1", Name: "C", Content: "x",
		GroupID:   &gid,
		CreatedAt: time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, cfg))

	svc := &RolloutServiceImpl{appStore: store, logger: zap.NewNop()}
	created, err := svc.Create(ctx, RolloutInput{
		Name:           "Standalone",
		GroupID:        gid,
		TargetConfigID: cfg.ID,
		Stages: []RolloutStage{
			{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, created.PlanID)
	assert.Equal(t, 0, created.PlanStepIndex)
}
