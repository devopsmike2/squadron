// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/testutils"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func init() {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)
}

// mockCommander is a mock implementation of AgentCommander that tracks calls
type mockCommander struct {
	sendConfigToAgentCalls         []sendConfigToAgentCall
	sendConfigToAgentsInGroupCalls []sendConfigToAgentsInGroupCall
	restartAgentCalls              []restartAgentCall
	restartAgentsInGroupCalls      []restartAgentsInGroupCall
}

type sendConfigToAgentCall struct {
	agentID       uuid.UUID
	configContent string
	err           error
}

type sendConfigToAgentsInGroupCall struct {
	groupID       string
	configContent string
	updatedAgents []uuid.UUID
	errors        []error
}

type restartAgentCall struct {
	agentID uuid.UUID
	err     error
}

type restartAgentsInGroupCall struct {
	groupID         string
	restartedAgents []uuid.UUID
	errors          []error
}

func newMockCommander() *mockCommander {
	return &mockCommander{
		sendConfigToAgentCalls:         []sendConfigToAgentCall{},
		sendConfigToAgentsInGroupCalls: []sendConfigToAgentsInGroupCall{},
		restartAgentCalls:              []restartAgentCall{},
		restartAgentsInGroupCalls:      []restartAgentsInGroupCall{},
	}
}

func (m *mockCommander) SendConfigToAgent(agentId uuid.UUID, configContent string) error {
	call := sendConfigToAgentCall{
		agentID:       agentId,
		configContent: configContent,
	}
	m.sendConfigToAgentCalls = append(m.sendConfigToAgentCalls, call)
	return call.err
}

func (m *mockCommander) SendConfigToAgentWithContext(ctx context.Context, agentId uuid.UUID, configContent string) error {
	return m.SendConfigToAgent(agentId, configContent)
}

func (m *mockCommander) SendConfigToAgentsInGroup(groupId string, configContent string) ([]uuid.UUID, []error) {
	call := sendConfigToAgentsInGroupCall{
		groupID:       groupId,
		configContent: configContent,
		updatedAgents: []uuid.UUID{},
		errors:        []error{},
	}
	m.sendConfigToAgentsInGroupCalls = append(m.sendConfigToAgentsInGroupCalls, call)
	return call.updatedAgents, call.errors
}

func (m *mockCommander) RestartAgent(agentId uuid.UUID) error {
	call := restartAgentCall{
		agentID: agentId,
	}
	m.restartAgentCalls = append(m.restartAgentCalls, call)
	return call.err
}

func (m *mockCommander) RestartAgentsInGroup(groupId string) ([]uuid.UUID, []error) {
	call := restartAgentsInGroupCall{
		groupID:         groupId,
		restartedAgents: []uuid.UUID{},
		errors:          []error{},
	}
	m.restartAgentsInGroupCalls = append(m.restartAgentsInGroupCalls, call)
	return call.restartedAgents, call.errors
}

func setupConfigHandlersTest() (*ConfigHandlers, *testutils.MockAgentService, *mockCommander) {
	mockService := testutils.NewMockAgentService()
	mockCommander := newMockCommander()
	logger := zap.NewNop()
	handlers := NewConfigHandlers(mockService, mockCommander, logger)
	return handlers, mockService, mockCommander
}

// TestHandleCreateConfig_GroupConfig_PropagatesToAgents tests that when a config
// is created for a group, it is automatically sent to all agents in that group.
// This is the key test for the bug fix.
func TestHandleCreateConfig_GroupConfig_PropagatesToAgents(t *testing.T) {
	handlers, mockService, mockCommander := setupConfigHandlersTest()

	// Create a group
	groupID := "test-group-1"
	group := testutils.MakeTestGroup(groupID)
	err := mockService.CreateGroup(context.TODO(), group)
	require.NoError(t, err)

	// Create agents in the group
	agent1ID := uuid.New()
	agent2ID := uuid.New()
	agent1 := testutils.MakeTestAgentWithStatus(agent1ID, services.AgentStatusOnline)
	agent2 := testutils.MakeTestAgentWithStatus(agent2ID, services.AgentStatusOnline)
	agent1.GroupID = &groupID
	agent2.GroupID = &groupID

	err = mockService.CreateAgent(context.TODO(), agent1)
	require.NoError(t, err)
	err = mockService.CreateAgent(context.TODO(), agent2)
	require.NoError(t, err)

	// Create config request for the group
	configContent := `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
processors:
  batch:
exporters:
  otlp:
    endpoint: http://localhost:4318
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]`

	configHash := fmt.Sprintf("%x", []byte(configContent))
	req := CreateConfigRequest{
		Name:       "test-config",
		GroupID:    &groupID,
		ConfigHash: configHash,
		Content:    configContent,
		Version:    1,
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/configs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Execute handler
	handlers.HandleCreateConfig(c)

	// Assert response
	assert.Equal(t, http.StatusCreated, w.Code)

	var response services.Config
	err = json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, configContent, response.Content)
	assert.Equal(t, groupID, *response.GroupID)

	// KEY ASSERTION: Verify that SendConfigToAgentsInGroup was called
	assert.Len(t, mockCommander.sendConfigToAgentsInGroupCalls, 1, "SendConfigToAgentsInGroup should be called once")
	call := mockCommander.sendConfigToAgentsInGroupCalls[0]
	assert.Equal(t, groupID, call.groupID, "Should be called with correct group ID")
	assert.Equal(t, configContent, call.configContent, "Should be called with correct config content")
}

// TestHandleCreateConfig_AgentConfig_DoesNotPropagate tests that when a config
// is created for an individual agent (not a group), it does NOT call SendConfigToAgentsInGroup
func TestHandleCreateConfig_AgentConfig_DoesNotPropagate(t *testing.T) {
	handlers, mockService, mockCommander := setupConfigHandlersTest()

	// Create an agent
	agentID := uuid.New()
	agent := testutils.MakeTestAgentWithStatus(agentID, services.AgentStatusOnline)
	err := mockService.CreateAgent(context.TODO(), agent)
	require.NoError(t, err)

	// Create config request for the agent (not group)
	configContent := "test-config-content"
	configHash := fmt.Sprintf("%x", []byte(configContent))
	req := CreateConfigRequest{
		Name:       "test-config",
		AgentID:    &agentID,
		GroupID:    nil, // No group
		ConfigHash: configHash,
		Content:    configContent,
		Version:    1,
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/configs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Execute handler
	handlers.HandleCreateConfig(c)

	// Assert response
	assert.Equal(t, http.StatusCreated, w.Code)

	// KEY ASSERTION: Verify that SendConfigToAgentsInGroup was NOT called
	assert.Len(t, mockCommander.sendConfigToAgentsInGroupCalls, 0, "SendConfigToAgentsInGroup should NOT be called for agent-specific configs")
}

// TestHandleCreateConfig_EmptyGroupID_DoesNotPropagate tests that an empty group ID
// does not trigger propagation
func TestHandleCreateConfig_EmptyGroupID_DoesNotPropagate(t *testing.T) {
	handlers, _, mockCommander := setupConfigHandlersTest()

	// Create config request with empty group ID
	configContent := "test-config-content"
	configHash := fmt.Sprintf("%x", []byte(configContent))
	emptyGroupID := ""
	req := CreateConfigRequest{
		Name:       "test-config",
		GroupID:    &emptyGroupID, // Empty string
		ConfigHash: configHash,
		Content:    configContent,
		Version:    1,
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/configs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Execute handler
	handlers.HandleCreateConfig(c)

	// Assert response
	assert.Equal(t, http.StatusCreated, w.Code)

	// KEY ASSERTION: Verify that SendConfigToAgentsInGroup was NOT called for empty group ID
	assert.Len(t, mockCommander.sendConfigToAgentsInGroupCalls, 0, "SendConfigToAgentsInGroup should NOT be called for empty group ID")
}

// TestHandleCreateConfig_GroupConfig_ServiceError tests error handling when
// config creation fails
func TestHandleCreateConfig_GroupConfig_ServiceError(t *testing.T) {
	handlers, mockService, mockCommander := setupConfigHandlersTest()

	// Set error on service
	mockService.CreateConfigErr = fmt.Errorf("database error")

	groupID := "test-group-1"
	configContent := "test-config-content"
	configHash := fmt.Sprintf("%x", []byte(configContent))
	req := CreateConfigRequest{
		Name:       "test-config",
		GroupID:    &groupID,
		ConfigHash: configHash,
		Content:    configContent,
		Version:    1,
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/configs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Execute handler
	handlers.HandleCreateConfig(c)

	// Assert response
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	// KEY ASSERTION: Verify that SendConfigToAgentsInGroup was NOT called when config creation fails
	assert.Len(t, mockCommander.sendConfigToAgentsInGroupCalls, 0, "SendConfigToAgentsInGroup should NOT be called if config creation fails")
}

// TestHandleCreateConfig_GroupConfig_WithMultipleAgents tests that config
// propagation works correctly when there are multiple agents in a group
func TestHandleCreateConfig_GroupConfig_WithMultipleAgents(t *testing.T) {
	handlers, mockService, mockCommander := setupConfigHandlersTest()

	// Create a group
	groupID := "test-group-1"
	group := testutils.MakeTestGroup(groupID)
	err := mockService.CreateGroup(context.TODO(), group)
	require.NoError(t, err)

	// Create multiple agents in the group
	agent1ID := uuid.New()
	agent2ID := uuid.New()
	agent3ID := uuid.New()

	agent1 := testutils.MakeTestAgentWithStatus(agent1ID, services.AgentStatusOnline)
	agent2 := testutils.MakeTestAgentWithStatus(agent2ID, services.AgentStatusOnline)
	agent3 := testutils.MakeTestAgentWithStatus(agent3ID, services.AgentStatusOnline)

	agent1.GroupID = &groupID
	agent2.GroupID = &groupID
	agent3.GroupID = &groupID

	err = mockService.CreateAgent(context.TODO(), agent1)
	require.NoError(t, err)
	err = mockService.CreateAgent(context.TODO(), agent2)
	require.NoError(t, err)
	err = mockService.CreateAgent(context.TODO(), agent3)
	require.NoError(t, err)

	// Create config request for the group
	configContent := "test-config-content"
	configHash := fmt.Sprintf("%x", []byte(configContent))
	req := CreateConfigRequest{
		Name:       "test-config",
		GroupID:    &groupID,
		ConfigHash: configHash,
		Content:    configContent,
		Version:    1,
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/configs", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Execute handler
	handlers.HandleCreateConfig(c)

	// Assert response
	assert.Equal(t, http.StatusCreated, w.Code)

	// KEY ASSERTION: Verify that SendConfigToAgentsInGroup was called once
	// (it should send to all agents in one call)
	assert.Len(t, mockCommander.sendConfigToAgentsInGroupCalls, 1, "SendConfigToAgentsInGroup should be called once for the group")
	call := mockCommander.sendConfigToAgentsInGroupCalls[0]
	assert.Equal(t, groupID, call.groupID)
	assert.Equal(t, configContent, call.configContent)
}

// --- Config soft-archive (POST /configs/:id/archive|unarchive) ---

// archiveCtx builds a gin context carrying the :id path param, ready to hand to
// the archive/unarchive handlers directly (RequireScope is a router-level
// concern, not exercised here).
func archiveCtx(w *httptest.ResponseRecorder, method, path, id string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	return c
}

// listConfigIDs runs HandleGetConfigs with the given raw query and returns the
// set of config IDs the default (or include_archived) listing returns.
func listConfigIDs(t *testing.T, h *ConfigHandlers, rawQuery string) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/configs?"+rawQuery, nil)
	h.HandleGetConfigs(c)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Configs []services.Config `json:"configs"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	ids := map[string]bool{}
	for _, cfg := range resp.Configs {
		ids[cfg.ID] = true
	}
	return ids
}

// TestHandleArchiveConfig_UnassignedConfig_LifecycleThroughList archives an
// unassigned config, confirms it leaves the default list but shows with
// include_archived, then unarchives it and confirms it returns.
func TestHandleArchiveConfig_UnassignedConfig_LifecycleThroughList(t *testing.T) {
	handlers, mockService, _ := setupConfigHandlersTest()

	cfg := &services.Config{ID: "cfg-unassigned", Name: "leftover", ConfigHash: "h", Content: "c", Version: 1, CreatedAt: time.Now()}
	require.NoError(t, mockService.CreateConfig(context.TODO(), cfg))

	// Present in the default list before archiving.
	assert.True(t, listConfigIDs(t, handlers, "")[cfg.ID], "config should be listed before archiving")

	// Archive → 200.
	w := httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-unassigned/archive", cfg.ID))
	require.Equal(t, http.StatusOK, w.Code)

	// Gone from the default list, present with include_archived.
	assert.False(t, listConfigIDs(t, handlers, "")[cfg.ID], "archived config must disappear from the default listing")
	assert.True(t, listConfigIDs(t, handlers, "include_archived=true")[cfg.ID], "archived config must appear with include_archived=true")

	// Archiving again is idempotent (200, still hidden).
	w = httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-unassigned/archive", cfg.ID))
	assert.Equal(t, http.StatusOK, w.Code)

	// Unarchive restores it to the default list.
	w = httptest.NewRecorder()
	handlers.HandleUnarchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-unassigned/unarchive", cfg.ID))
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, listConfigIDs(t, handlers, "")[cfg.ID], "unarchived config must return to the default listing")
}

// TestHandleArchiveConfig_AssignedToAgent_Refused proves archiving the CURRENT
// effective config of a live agent is refused with 409.
func TestHandleArchiveConfig_AssignedToAgent_Refused(t *testing.T) {
	handlers, mockService, _ := setupConfigHandlersTest()

	agentID := uuid.New()
	require.NoError(t, mockService.CreateAgent(context.TODO(), testutils.MakeTestAgentWithStatus(agentID, services.AgentStatusOnline)))
	cfg := &services.Config{ID: "cfg-live", AgentID: &agentID, ConfigHash: "h", Content: "c", Version: 1, CreatedAt: time.Now()}
	require.NoError(t, mockService.CreateConfig(context.TODO(), cfg))

	w := httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-live/archive", cfg.ID))
	require.Equal(t, http.StatusConflict, w.Code, "archiving a config assigned to a live agent must be refused")

	// Still active + still listed (the refusal did not archive it).
	assert.True(t, listConfigIDs(t, handlers, "")[cfg.ID])
}

// TestHandleArchiveConfig_SupersededVersion_Archivable proves an older version
// (not the latest for the agent) is archivable while the latest is refused.
func TestHandleArchiveConfig_SupersededVersion_Archivable(t *testing.T) {
	handlers, mockService, _ := setupConfigHandlersTest()

	agentID := uuid.New()
	require.NoError(t, mockService.CreateAgent(context.TODO(), testutils.MakeTestAgentWithStatus(agentID, services.AgentStatusOnline)))
	v1 := &services.Config{ID: "cfg-v1", AgentID: &agentID, ConfigHash: "h1", Content: "c1", Version: 1, CreatedAt: time.Now().Add(-time.Hour)}
	v2 := &services.Config{ID: "cfg-v2", AgentID: &agentID, ConfigHash: "h2", Content: "c2", Version: 2, CreatedAt: time.Now()}
	require.NoError(t, mockService.CreateConfig(context.TODO(), v1))
	require.NoError(t, mockService.CreateConfig(context.TODO(), v2))

	// Superseded v1 archives fine.
	w := httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-v1/archive", v1.ID))
	require.Equal(t, http.StatusOK, w.Code, "a superseded (non-latest) version must be archivable")

	// Current v2 is refused.
	w = httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-v2/archive", v2.ID))
	assert.Equal(t, http.StatusConflict, w.Code, "the current latest version must be refused")
}

// TestHandleArchiveConfig_OrphanedAgentConfig_Archivable proves a config pinned
// to an agent that no longer exists (decommissioned) is residue and archivable —
// the exact acceptance-pass cleanup case for leftover "…-adopted" configs.
func TestHandleArchiveConfig_OrphanedAgentConfig_Archivable(t *testing.T) {
	handlers, mockService, _ := setupConfigHandlersTest()

	// AgentID set, but no such agent exists in the store (GetAgent -> nil).
	goneAgent := uuid.New()
	cfg := &services.Config{ID: "cfg-adopted", Name: "host-adopted", AgentID: &goneAgent, ConfigHash: "h", Content: "c", Version: 1, CreatedAt: time.Now()}
	require.NoError(t, mockService.CreateConfig(context.TODO(), cfg))

	w := httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/cfg-adopted/archive", cfg.ID))
	require.Equal(t, http.StatusOK, w.Code, "a config pinned to a decommissioned agent is residue and archivable")
	assert.False(t, listConfigIDs(t, handlers, "")[cfg.ID])
}

// TestHandleArchiveConfig_NotFound returns 404 for an unknown id.
func TestHandleArchiveConfig_NotFound(t *testing.T) {
	handlers, _, _ := setupConfigHandlersTest()
	w := httptest.NewRecorder()
	handlers.HandleArchiveConfig(archiveCtx(w, "POST", "/api/v1/configs/nope/archive", "nope"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
