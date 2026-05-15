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

func TestAgentServiceConfigDriftDetection(t *testing.T) {
	store := memory.NewStore()
	logger := zap.NewNop()
	service := NewAgentService(store, nil, logger)

	agentID := uuid.New()
	now := time.Now()
	baseConfig := "receivers:\n  otlp:\n    protocols:\n      grpc:"

	agent := &Agent{
		ID:           agentID,
		Name:         "test-agent",
		Status:       AgentStatusOnline,
		Labels:       map[string]string{},
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	ctx := context.Background()
	require.NoError(t, service.CreateAgent(ctx, agent))

	_, err := service.StoreConfigForAgent(ctx, agentID, baseConfig)
	require.NoError(t, err)

	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, baseConfig))

	syncedAgent, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, syncedAgent)
	assert.Equal(t, ConfigDriftStatusSynced, syncedAgent.DriftStatus)
	require.NotNil(t, syncedAgent.ConfigIntent)
	require.NotNil(t, syncedAgent.DriftDetails)
	assert.Equal(t, syncedAgent.DriftDetails.IntentHash, syncedAgent.DriftDetails.EffectiveHash)

	driftedConfig := baseConfig + "\n  http: {}"
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, driftedConfig))

	driftedAgent, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, driftedAgent)
	assert.Equal(t, ConfigDriftStatusDrifted, driftedAgent.DriftStatus)
	require.NotNil(t, driftedAgent.DriftDetails)
	assert.NotEmpty(t, driftedAgent.DriftDetails.Diff)
}
