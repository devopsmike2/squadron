// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestRestartAgent_DispatchesWhenLiveCapabilityPresent verifies the restart path
// dispatches (returns nil) when the connected agent advertises AcceptsRestartCommand
// on its live OpAMP capability bitmask.
func TestRestartAgent_DispatchesWhenLiveCapabilityPresent(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	configSender := NewConfigSender(agents, logger)

	agentID := uuid.New()
	agent := NewAgent(agentID, &mockConnection{})
	agent.Status = &protobufs.AgentToServer{
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand),
	}
	agents.agentsById[agentID] = agent

	err := configSender.RestartAgent(agentID)
	assert.NoError(t, err, "restart should dispatch when the agent advertises AcceptsRestartCommand")
}

// TestRestartAgent_BlockedWhenCapabilityAbsent verifies that an agent which does
// NOT advertise AcceptsRestartCommand is blocked with a clear error — NOT a false
// "success". This is the honesty guarantee: Squadron must never report a restart
// dispatched to a connection that never claimed to accept it.
func TestRestartAgent_BlockedWhenCapabilityAbsent(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	configSender := NewConfigSender(agents, logger)

	agentID := uuid.New()
	agent := NewAgent(agentID, &mockConnection{})
	// Advertises remote-config but NOT restart.
	agent.Status = &protobufs.AgentToServer{
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}
	agents.agentsById[agentID] = agent

	err := configSender.RestartAgent(agentID)
	require.Error(t, err, "restart must be blocked when the agent does not advertise AcceptsRestartCommand")
	assert.Contains(t, err.Error(), "does not support restart command",
		"the error must clearly state the agent does not accept the restart command")
}

// TestRestartAgent_NotFound verifies a missing agent surfaces a clear error
// rather than a silent success.
func TestRestartAgent_NotFound(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	configSender := NewConfigSender(agents, logger)

	err := configSender.RestartAgent(uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent not found")
}
