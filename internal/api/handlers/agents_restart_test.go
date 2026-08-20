// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/devopsmike2/squadron/internal/testutils"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// restartMockCommander is an AgentCommander whose RestartAgent returns a
// configurable error, so we can exercise both the dispatched and the
// capability-blocked branches of HandleRestartAgent.
type restartMockCommander struct {
	restartErr error
}

func (m *restartMockCommander) SendConfigToAgent(uuid.UUID, string) error { return nil }
func (m *restartMockCommander) SendConfigToAgentWithContext(context.Context, uuid.UUID, string) error {
	return nil
}
func (m *restartMockCommander) RestartAgent(uuid.UUID) error { return m.restartErr }
func (m *restartMockCommander) RestartAgentsInGroup(string) ([]uuid.UUID, []error) {
	return nil, nil
}
func (m *restartMockCommander) SendConfigToAgentsInGroup(string, string) ([]uuid.UUID, []error) {
	return nil, nil
}

func setupRestartHandler(restartErr error) *AgentHandlers {
	return NewAgentHandlers(
		testutils.NewMockAgentService(),
		&restartMockCommander{restartErr: restartErr},
		zap.NewNop(),
	)
}

// TestHandleRestartAgent_Dispatched verifies the success path returns 200 with an
// HONEST message: it reports the command was dispatched, not that the collector
// restarted (dispatch != confirmed actuation).
func TestHandleRestartAgent_Dispatched(t *testing.T) {
	h := setupRestartHandler(nil)
	agentID := uuid.New()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/agents/%s/restart", agentID), nil)
	c.Params = gin.Params{{Key: "id", Value: agentID.String()}}

	h.HandleRestartAgent(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp RestartAgentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Contains(t, resp.Message, "dispatched",
		"success message must state the command was dispatched, not that the collector restarted")
	assert.NotContains(t, resp.Message, "restarted successfully",
		"message must not over-claim actuation")
}

// TestHandleRestartAgent_UnsupportedCapabilityBlocked verifies that when the
// connected agent does not advertise the restart capability, the handler returns
// 400 with success=false and a clear error — NOT a false success.
func TestHandleRestartAgent_UnsupportedCapabilityBlocked(t *testing.T) {
	h := setupRestartHandler(fmt.Errorf("agent does not support restart command"))
	agentID := uuid.New()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/agents/%s/restart", agentID), nil)
	c.Params = gin.Params{{Key: "id", Value: agentID.String()}}

	h.HandleRestartAgent(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp RestartAgentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
	assert.Contains(t, resp.Message, "does not support restart command")
}

// TestHandleRestartAgent_NotFound verifies a missing agent maps to 404.
func TestHandleRestartAgent_NotFound(t *testing.T) {
	h := setupRestartHandler(fmt.Errorf("agent not found"))
	agentID := uuid.New()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/agents/%s/restart", agentID), nil)
	c.Params = gin.Params{{Key: "id", Value: agentID.String()}}

	h.HandleRestartAgent(c)

	assert.Equal(t, http.StatusNotFound, w.Code)

	var resp RestartAgentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.Success)
}
