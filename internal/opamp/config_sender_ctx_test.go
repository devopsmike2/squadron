// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestSendConfigToAgentWithContext_CancelledContextReturnsPromptly pins the
// rollout follow-up B behavior: the ack wait selects on the caller's ctx, so a
// cancelled/expired context (e.g. the rollout engine's per-tick deadline) ends
// the wait at once instead of holding the push goroutine for the full 30s cap.
func TestSendConfigToAgentWithContext_CancelledContextReturnsPromptly(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	configSender := NewConfigSender(agents, logger)

	agentID := uuid.New()
	agent := NewAgent(agentID, &mockConnection{})
	agent.Status = &protobufs.AgentToServer{
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}
	agents.agentsById[agentID] = agent

	// A genuine config change (the agent has no prior config) does NOT notify
	// synchronously, so the call enters the ack wait. With an already-cancelled
	// context, the ctx.Done() arm must fire immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := configSender.SendConfigToAgentWithContext(ctx, agentID, "new-config-body")
	elapsed := time.Since(start)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 5*time.Second,
		"must return promptly on ctx cancel, not block on the 30s per-push cap")
}
