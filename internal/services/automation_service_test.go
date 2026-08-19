// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

func newAutomationService(t *testing.T) AutomationService {
	t.Helper()
	return NewAutomationService(memory.NewStore(), zap.NewNop())
}

func strp(s string) *string { return &s }

func TestAutomationService_CreateValidation(t *testing.T) {
	svc := newAutomationService(t)
	ctx := context.Background()

	base := func() AutomationInput {
		return AutomationInput{
			Name:    "restart",
			Enabled: true,
			AgentID: strp("agent-1"),
			Trigger: AutomationTriggerAgentUnhealthy,
			Action:  AutomationActionSupervisorRestart,
		}
	}

	t.Run("valid agent-scoped rule with defaulted guardrails", func(t *testing.T) {
		a, err := svc.CreateAutomation(ctx, base())
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.NotEmpty(t, a.ID)
		// Guardrails default to storm-safe values when omitted.
		assert.Equal(t, DefaultAutomationCooldownSeconds, a.CooldownSeconds)
		assert.Equal(t, DefaultAutomationMaxAttempts, a.MaxAttempts)
	})

	t.Run("empty name rejected", func(t *testing.T) {
		in := base()
		in.Name = "  "
		_, err := svc.CreateAutomation(ctx, in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("neither agent nor group rejected", func(t *testing.T) {
		in := base()
		in.AgentID = nil
		in.GroupID = nil
		_, err := svc.CreateAutomation(ctx, in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exactly one")
	})

	t.Run("both agent and group rejected", func(t *testing.T) {
		in := base()
		in.GroupID = strp("group-1")
		_, err := svc.CreateAutomation(ctx, in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not both")
	})

	t.Run("empty-string agent id is treated as unset", func(t *testing.T) {
		in := base()
		in.AgentID = strp("")
		in.GroupID = strp("group-1")
		a, err := svc.CreateAutomation(ctx, in)
		require.NoError(t, err)
		assert.Nil(t, a.AgentID)
		require.NotNil(t, a.GroupID)
	})

	t.Run("invalid trigger rejected", func(t *testing.T) {
		in := base()
		in.Trigger = "meltdown"
		_, err := svc.CreateAutomation(ctx, in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid trigger")
	})

	t.Run("invalid action rejected", func(t *testing.T) {
		in := base()
		in.Action = "nuke"
		_, err := svc.CreateAutomation(ctx, in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid action")
	})
}

func TestAutomationService_UpdateAndEnable(t *testing.T) {
	svc := newAutomationService(t)
	ctx := context.Background()

	created, err := svc.CreateAutomation(ctx, AutomationInput{
		Name:            "restart pilot group",
		Enabled:         true,
		GroupID:         strp("southern-pilot"),
		Trigger:         AutomationTriggerAgentSilent,
		Action:          AutomationActionSupervisorRestart,
		CooldownSeconds: 120,
		MaxAttempts:     2,
		DryRun:          true,
	})
	require.NoError(t, err)

	// Update mutates fields and preserves CreatedAt.
	updated, err := svc.UpdateAutomation(ctx, created.ID, AutomationInput{
		Name:            "restart pilot group v2",
		Enabled:         true,
		GroupID:         strp("southern-pilot"),
		Trigger:         AutomationTriggerAgentUnhealthy,
		Action:          AutomationActionSupervisorRestart,
		CooldownSeconds: 200,
		MaxAttempts:     4,
	})
	require.NoError(t, err)
	assert.Equal(t, "restart pilot group v2", updated.Name)
	assert.Equal(t, AutomationTriggerAgentUnhealthy, updated.Trigger)
	assert.Equal(t, created.CreatedAt, updated.CreatedAt)

	// SetEnabled flips only the flag.
	off, err := svc.SetAutomationEnabled(ctx, created.ID, false)
	require.NoError(t, err)
	assert.False(t, off.Enabled)
	assert.Equal(t, "restart pilot group v2", off.Name)

	// Missing id paths.
	_, err = svc.UpdateAutomation(ctx, "ghost", AutomationInput{
		Name: "x", AgentID: strp("a"), Trigger: AutomationTriggerAgentUnhealthy, Action: AutomationActionSupervisorRestart,
	})
	require.Error(t, err)
	_, err = svc.SetAutomationEnabled(ctx, "ghost", true)
	require.Error(t, err)

	require.NoError(t, svc.DeleteAutomation(ctx, created.ID))
	require.Error(t, svc.DeleteAutomation(ctx, created.ID))
}
