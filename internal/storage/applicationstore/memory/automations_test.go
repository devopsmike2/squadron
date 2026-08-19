// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryAutomationCRUD(t *testing.T) {
	store := NewStore()
	ctx := context.Background()
	now := time.Now().UTC()
	gid := "southern-pilot"

	a := &types.Automation{
		ID:              "au1",
		Name:            "restart unhealthy",
		Enabled:         true,
		GroupID:         &gid,
		Trigger:         types.AutomationTriggerAgentUnhealthy,
		Action:          types.AutomationActionSupervisorRestart,
		CooldownSeconds: 300,
		MaxAttempts:     3,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, store.CreateAutomation(ctx, a))

	// Duplicate id rejected.
	require.Error(t, store.CreateAutomation(ctx, a))

	got, err := store.GetAutomation(ctx, "au1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "restart unhealthy", got.Name)
	require.NotNil(t, got.GroupID)
	assert.Equal(t, gid, *got.GroupID)
	assert.Nil(t, got.AgentID)

	// Returned pointer must not alias the stored copy.
	*got.GroupID = "mutated"
	reread, _ := store.GetAutomation(ctx, "au1")
	assert.Equal(t, gid, *reread.GroupID)

	// Missing get is (nil, nil).
	miss, err := store.GetAutomation(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, miss)

	// Update.
	a.Enabled = false
	a.DryRun = true
	require.NoError(t, store.UpdateAutomation(ctx, a))
	got, _ = store.GetAutomation(ctx, "au1")
	assert.False(t, got.Enabled)
	assert.True(t, got.DryRun)

	// Update missing errors.
	require.Error(t, store.UpdateAutomation(ctx, &types.Automation{ID: "ghost"}))

	// List.
	list, err := store.ListAutomations(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// Delete.
	require.NoError(t, store.DeleteAutomation(ctx, "au1"))
	require.Error(t, store.DeleteAutomation(ctx, "au1"))
	list, _ = store.ListAutomations(ctx)
	assert.Empty(t, list)
}
