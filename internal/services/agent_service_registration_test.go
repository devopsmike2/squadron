// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestUpdateAgentRegistration_PersistsMutableFields: the service method
// converts the services.Agent and writes Name/Labels/Version/GroupID/
// GroupName through to the store, leaving Status (a heartbeat-owned
// field) untouched.
func TestUpdateAgentRegistration_PersistsMutableFields(t *testing.T) {
	store := memory.NewStore()
	service := NewAgentService(store, nil, nil, nil, zap.NewNop())
	ctx := context.Background()

	agentID := uuid.New()
	require.NoError(t, service.CreateAgent(ctx, &Agent{
		ID:        agentID,
		Name:      "original",
		Status:    AgentStatusOnline,
		Labels:    map[string]string{"env": "test"},
		Version:   "1.0.0",
		LastSeen:  time.Now(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))

	gid, gname := "grp-9", "prod"
	err := service.UpdateAgentRegistration(ctx, &Agent{
		ID:        agentID,
		Name:      "renamed",
		Labels:    map[string]string{"tier": "gold"},
		Version:   "2.0.0",
		GroupID:   &gid,
		GroupName: &gname,
	})
	require.NoError(t, err)

	got, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "2.0.0", got.Version)
	require.NotNil(t, got.GroupID)
	assert.Equal(t, "grp-9", *got.GroupID)
	require.NotNil(t, got.GroupName)
	assert.Equal(t, "prod", *got.GroupName)
	assert.Equal(t, "gold", got.Labels["tier"])
	// Status is heartbeat-owned, not written by registration.
	assert.Equal(t, AgentStatusOnline, got.Status)
}

func TestUpdateAgentRegistration_UnknownAgent(t *testing.T) {
	store := memory.NewStore()
	service := NewAgentService(store, nil, nil, nil, zap.NewNop())

	err := service.UpdateAgentRegistration(context.Background(), &Agent{
		ID:   uuid.New(),
		Name: "ghost",
	})
	require.Error(t, err)
}
