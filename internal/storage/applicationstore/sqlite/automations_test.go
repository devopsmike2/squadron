// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteAutomationCRUD(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
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
			DryRun:          false,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		require.NoError(t, store.CreateAutomation(ctx, a))

		got, err := store.GetAutomation(ctx, "au1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "restart unhealthy", got.Name)
		require.NotNil(t, got.GroupID)
		assert.Equal(t, gid, *got.GroupID)
		assert.Nil(t, got.AgentID, "group-scoped rule must have nil agent_id")
		assert.Equal(t, types.AutomationTriggerAgentUnhealthy, got.Trigger)
		assert.Equal(t, types.AutomationActionSupervisorRestart, got.Action)
		assert.Equal(t, 300, got.CooldownSeconds)
		assert.Equal(t, 3, got.MaxAttempts)
		assert.False(t, got.DryRun)
		assert.True(t, got.Enabled)

		// Missing get is (nil, nil).
		miss, err := store.GetAutomation(ctx, "nope")
		require.NoError(t, err)
		assert.Nil(t, miss)

		// Update: switch to agent scope, flip enabled + dry-run.
		aid := "agent-abc"
		a.Enabled = false
		a.DryRun = true
		a.GroupID = nil
		a.AgentID = &aid
		a.Trigger = types.AutomationTriggerAgentSilent
		a.UpdatedAt = now.Add(time.Second)
		require.NoError(t, store.UpdateAutomation(ctx, a))

		got, _ = store.GetAutomation(ctx, "au1")
		assert.False(t, got.Enabled)
		assert.True(t, got.DryRun)
		assert.Nil(t, got.GroupID)
		require.NotNil(t, got.AgentID)
		assert.Equal(t, aid, *got.AgentID)
		assert.Equal(t, types.AutomationTriggerAgentSilent, got.Trigger)

		// List ordering is name ASC.
		a2 := &types.Automation{ID: "au2", Name: "abc automation", Enabled: true, AgentID: &aid,
			Trigger: types.AutomationTriggerAgentUnhealthy, Action: types.AutomationActionSupervisorRestart,
			CooldownSeconds: 60, MaxAttempts: 1, CreatedAt: now, UpdatedAt: now}
		require.NoError(t, store.CreateAutomation(ctx, a2))
		list, err := store.ListAutomations(ctx)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, "abc automation", list[0].Name)

		// Update / delete missing are errors.
		require.Error(t, store.UpdateAutomation(ctx, &types.Automation{ID: "ghost"}))
		require.Error(t, store.DeleteAutomation(ctx, "ghost"))

		require.NoError(t, store.DeleteAutomation(ctx, "au1"))
		require.Error(t, store.DeleteAutomation(ctx, "au1"))
	})
}
