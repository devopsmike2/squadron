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

// mockConfigSender is a simple mock for AgentCommander
type mockConfigSender struct{}

func (m *mockConfigSender) SendConfigToAgent(agentId uuid.UUID, configContent string) error {
	return nil
}

func (m *mockConfigSender) SendConfigToAgentWithContext(ctx context.Context, agentId uuid.UUID, configContent string) error {
	return nil
}

func (m *mockConfigSender) RestartAgent(agentId uuid.UUID) error {
	return nil
}

func (m *mockConfigSender) RestartAgentsInGroup(groupId string) ([]uuid.UUID, []error) {
	return []uuid.UUID{}, []error{}
}

func (m *mockConfigSender) SendConfigToAgentsInGroup(groupId string, configContent string) ([]uuid.UUID, []error) {
	return []uuid.UUID{}, []error{}
}

func setupAgentHandlersTest() (*AgentHandlers, *testutils.MockAgentService) {
	mockService := testutils.NewMockAgentService()
	mockSender := &mockConfigSender{}
	logger := zap.NewNop()
	handlers := NewAgentHandlers(mockService, mockSender, logger)
	return handlers, mockService
}

func TestHandleGetAgents(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Create test agents
	agent1 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOnline)
	agent2 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOffline)

	_ = mockService.CreateAgent(context.TODO(), agent1)
	_ = mockService.CreateAgent(context.TODO(), agent2)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents", nil)

	// Execute handler
	handlers.HandleGetAgents(c)

	// Assert response
	assert.Equal(t, http.StatusOK, w.Code)

	var response GetAgentsResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, 2, response.TotalCount)
	assert.Equal(t, 1, response.ActiveCount)
	assert.Equal(t, 1, response.InactiveCount)
	assert.Len(t, response.Agents, 2)
}

func TestHandleGetAgents_ServiceError(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Set error flag
	mockService.ListAgentsErr = fmt.Errorf("database error")

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents", nil)

	// Execute handler
	handlers.HandleGetAgents(c)

	// Assert response
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response["error"], "Failed to fetch agents")
}

// TestHandleGetAgents_DriftStatusFilter verifies that ?drift_status=… returns
// only matching agents, and that an unrecognized value 400s with the allowed
// list in the body.
func TestHandleGetAgents_DriftStatusFilter(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Two synced, one drifted, one with no_intent.
	mkAgent := func(name string, drift services.ConfigDriftStatus) *services.Agent {
		a := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOnline)
		a.Name = name
		a.DriftStatus = drift
		return a
	}
	syncedA := mkAgent("a", services.ConfigDriftStatusSynced)
	syncedB := mkAgent("b", services.ConfigDriftStatusSynced)
	drifted := mkAgent("c", services.ConfigDriftStatusDrifted)
	noIntent := mkAgent("d", services.ConfigDriftStatusNoIntent)

	for _, a := range []*services.Agent{syncedA, syncedB, drifted, noIntent} {
		require.NoError(t, mockService.CreateAgent(context.TODO(), a))
	}

	// Filter: drifted -> should return exactly 1 agent (c).
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents?drift_status=drifted", nil)
	handlers.HandleGetAgents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp GetAgentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.TotalCount, "drifted filter should return exactly one agent")
	assert.Len(t, resp.Agents, 1)
	require.Contains(t, resp.Agents, drifted.ID.String())
	assert.Equal(t, services.ConfigDriftStatusDrifted, resp.Agents[drifted.ID.String()].DriftStatus)

	// Filter: synced -> should return both synced agents.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents?drift_status=synced", nil)
	handlers.HandleGetAgents(c)

	require.Equal(t, http.StatusOK, w.Code)
	resp = GetAgentsResponse{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.TotalCount, "synced filter should return both synced agents")

	// Filter: unknown value -> 400 with allowed list.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents?drift_status=bogus", nil)
	handlers.HandleGetAgents(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "invalid drift_status", errResp["error"])
	assert.Contains(t, errResp, "allowed")
}

// TestHandleGetAgents_Pagination pins the v0.23 pagination
// envelope: items array sorted stably by ID, total reflects the
// pre-pagination filtered count, and offset+limit slice correctly
// at boundaries.
func TestHandleGetAgents_Pagination(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Seed 5 deterministic agents with sortable IDs so we can pin
	// the expected order across pages.
	mk := func(n int) *services.Agent {
		var id uuid.UUID
		id[0] = byte(n) // first byte controls lexicographic order
		a := testutils.MakeTestAgentWithStatus(id, services.AgentStatusOnline)
		a.Name = fmt.Sprintf("agent-%02d", n)
		return a
	}
	all := []*services.Agent{mk(1), mk(2), mk(3), mk(4), mk(5)}
	for _, a := range all {
		require.NoError(t, mockService.CreateAgent(context.TODO(), a))
	}

	doReq := func(query string) GetAgentsResponse {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/v1/agents?"+query, nil)
		handlers.HandleGetAgents(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var r GetAgentsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		return r
	}

	// Page 1 of 2: offset=0, limit=2 → items[0..2], total=5.
	page1 := doReq("offset=0&limit=2")
	require.Len(t, page1.Items, 2)
	assert.Equal(t, 5, page1.Total, "total reflects unpaginated filtered count")
	assert.Equal(t, 0, page1.Offset)
	assert.Equal(t, 2, page1.Limit)
	assert.Equal(t, "agent-01", page1.Items[0].Name)
	assert.Equal(t, "agent-02", page1.Items[1].Name)

	// Page 2 of 2: offset=2, limit=2 → items[2..4].
	page2 := doReq("offset=2&limit=2")
	require.Len(t, page2.Items, 2)
	assert.Equal(t, "agent-03", page2.Items[0].Name)
	assert.Equal(t, "agent-04", page2.Items[1].Name)

	// Final page: offset=4, limit=2 → items[4..5] (1 element).
	page3 := doReq("offset=4&limit=2")
	require.Len(t, page3.Items, 1)
	assert.Equal(t, "agent-05", page3.Items[0].Name)

	// Offset past the end → empty items, total still accurate.
	pageEmpty := doReq("offset=99&limit=10")
	assert.Empty(t, pageEmpty.Items)
	assert.Equal(t, 5, pageEmpty.Total)

	// Stability across calls: same query, same items, same order.
	again := doReq("offset=0&limit=2")
	assert.Equal(t, page1.Items[0].ID, again.Items[0].ID,
		"successive page-1 fetches should return identical items in identical order")

	// Legacy fields still present.
	assert.Equal(t, 5, page1.TotalCount, "legacy totalCount equals new Total")
	assert.NotNil(t, page1.Agents, "legacy agents map still populated for back-compat")
	assert.Len(t, page1.Agents, 2, "legacy map mirrors the current page's items")
}

// TestHandleGetAgents_LimitCap enforces the maxAgentsLimit guard
// — a client asking for limit=10000 should be capped to 500, not
// trigger a 400.
func TestHandleGetAgents_LimitCap(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()
	for i := 0; i < 3; i++ {
		var id uuid.UUID
		id[0] = byte(i + 1)
		require.NoError(t, mockService.CreateAgent(context.TODO(),
			testutils.MakeTestAgentWithStatus(id, services.AgentStatusOnline)))
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents?limit=99999", nil)
	handlers.HandleGetAgents(c)

	require.Equal(t, http.StatusOK, w.Code)
	var r GetAgentsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
	assert.Equal(t, 500, r.Limit, "limit must be clamped to maxAgentsLimit")
}

// TestHandleGetAgents_PaginationBadInputs covers the 400 paths so
// clients get actionable errors rather than a silent fallback to
// the default page.
func TestHandleGetAgents_PaginationBadInputs(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()
	cases := []struct {
		name  string
		query string
	}{
		{"negative offset", "offset=-1"},
		{"non-numeric offset", "offset=abc"},
		{"zero limit", "limit=0"},
		{"negative limit", "limit=-5"},
		{"non-numeric limit", "limit=foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/v1/agents?"+tc.query, nil)
			handlers.HandleGetAgents(c)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}

// TestHandleGetAgents_FiltersCompose pins that drift_status,
// status, group_id, q, and pagination all narrow the same working
// set rather than overriding each other.
func TestHandleGetAgents_FiltersCompose(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	groupA := "00000000-0000-0000-0000-aaaaaaaaaaaa"
	groupB := "00000000-0000-0000-0000-bbbbbbbbbbbb"

	mk := func(n int, status services.AgentStatus, drift services.ConfigDriftStatus, group string, labels map[string]string) *services.Agent {
		var id uuid.UUID
		id[0] = byte(n)
		a := testutils.MakeTestAgentWithStatus(id, status)
		a.Name = fmt.Sprintf("agent-%02d", n)
		a.DriftStatus = drift
		if group != "" {
			g := group
			a.GroupID = &g
		}
		if labels != nil {
			a.Labels = labels
		}
		return a
	}

	// Seed a mixed fleet.
	all := []*services.Agent{
		mk(1, services.AgentStatusOnline, services.ConfigDriftStatusDrifted, groupA, map[string]string{"region": "us-east"}),
		mk(2, services.AgentStatusOnline, services.ConfigDriftStatusSynced, groupA, map[string]string{"region": "us-west"}),
		mk(3, services.AgentStatusOffline, services.ConfigDriftStatusDrifted, groupB, map[string]string{"region": "eu-west"}),
		mk(4, services.AgentStatusOnline, services.ConfigDriftStatusSynced, groupB, map[string]string{"region": "us-east"}),
	}
	for _, a := range all {
		require.NoError(t, mockService.CreateAgent(context.TODO(), a))
	}

	doReq := func(query string) GetAgentsResponse {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/v1/agents?"+query, nil)
		handlers.HandleGetAgents(c)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var r GetAgentsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &r))
		return r
	}

	// status=online narrows to 3 of 4.
	r := doReq("status=online")
	assert.Equal(t, 3, r.Total)

	// status=online + drift=synced narrows to 2 (agents 2 + 4).
	r = doReq("status=online&drift_status=synced")
	assert.Equal(t, 2, r.Total)

	// + group_id picks just agent 2.
	r = doReq("status=online&drift_status=synced&group_id=" + groupA)
	require.Equal(t, 1, r.Total)
	require.Len(t, r.Items, 1)
	assert.Equal(t, "agent-02", r.Items[0].Name)

	// q matches label values too.
	r = doReq("q=us-east")
	assert.Equal(t, 2, r.Total, "us-east matches agents 1 + 4")

	// q + filters: us-east AND online AND not-drifted -> agent 4.
	r = doReq("q=us-east&status=online&drift_status=synced")
	require.Equal(t, 1, r.Total)
	assert.Equal(t, "agent-04", r.Items[0].Name)

	// Pagination on top of a filter: status=online, limit=1.
	page1 := doReq("status=online&limit=1&offset=0")
	page2 := doReq("status=online&limit=1&offset=1")
	page3 := doReq("status=online&limit=1&offset=2")
	require.Equal(t, 3, page1.Total)
	require.Len(t, page1.Items, 1)
	require.Len(t, page2.Items, 1)
	require.Len(t, page3.Items, 1)
	assert.NotEqual(t, page1.Items[0].ID, page2.Items[0].ID)
	assert.NotEqual(t, page2.Items[0].ID, page3.Items[0].ID)
}

// TestHandleGetAgents_BadStatusFilter ensures unrecognized values
// 400 cleanly with the allowed list (mirrors the drift_status
// behavior so the UX is consistent).
func TestHandleGetAgents_BadStatusFilter(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents?status=enabled", nil)
	handlers.HandleGetAgents(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var errResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Equal(t, "invalid status", errResp["error"])
	assert.Contains(t, errResp, "allowed")
}

func TestHandleGetAgent(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Create test agent
	agentID := uuid.New()
	agent := testutils.MakeTestAgent(agentID)
	_ = mockService.CreateAgent(context.TODO(), agent)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/agents/%s", agentID), nil)
	c.Params = gin.Params{{Key: "id", Value: agentID.String()}}

	// Execute handler
	handlers.HandleGetAgent(c)

	// Assert response
	assert.Equal(t, http.StatusOK, w.Code)

	var response services.Agent
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Equal(t, agentID, response.ID)
	assert.Equal(t, agent.Name, response.Name)
}

func TestHandleGetAgent_NotFound(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()

	// Use non-existent agent ID
	nonExistentID := uuid.New()

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/agents/%s", nonExistentID), nil)
	c.Params = gin.Params{{Key: "id", Value: nonExistentID.String()}}

	// Execute handler
	handlers.HandleGetAgent(c)

	// Assert response
	assert.Equal(t, http.StatusNotFound, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response["error"], "Agent not found")
}

func TestHandleGetAgent_InvalidID(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()

	// Create test request with invalid UUID
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents/invalid-uuid", nil)
	c.Params = gin.Params{{Key: "id", Value: "invalid-uuid"}}

	// Execute handler
	handlers.HandleGetAgent(c)

	// Assert response
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response["error"], "Invalid agent ID format")
}

func TestHandleGetAgent_MissingID(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()

	// Create test request without ID param
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents/", nil)

	// Execute handler
	handlers.HandleGetAgent(c)

	// Assert response
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response["error"], "Agent ID is required")
}

func TestHandleGetAgentStats(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Create test agents with different statuses
	agent1 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOnline)
	agent2 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOnline)
	agent3 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusOffline)
	agent4 := testutils.MakeTestAgentWithStatus(uuid.New(), services.AgentStatusError)

	_ = mockService.CreateAgent(context.TODO(), agent1)
	_ = mockService.CreateAgent(context.TODO(), agent2)
	_ = mockService.CreateAgent(context.TODO(), agent3)
	_ = mockService.CreateAgent(context.TODO(), agent4)

	// Create test groups
	group1 := testutils.MakeTestGroup("group-1")
	group2 := testutils.MakeTestGroup("group-2")
	_ = mockService.CreateGroup(context.TODO(), group1)
	_ = mockService.CreateGroup(context.TODO(), group2)

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents/stats", nil)

	// Execute handler
	handlers.HandleGetAgentStats(c)

	// Assert response
	assert.Equal(t, http.StatusOK, w.Code)

	var response GetAgentStatsResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, 4, response.TotalAgents)
	assert.Equal(t, 2, response.OnlineAgents)
	assert.Equal(t, 1, response.OfflineAgents)
	assert.Equal(t, 1, response.ErrorAgents)
	assert.Equal(t, 2, response.GroupsCount)
}

func TestHandleGetAgentStats_ServiceError(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Set error flag
	mockService.ListAgentsErr = fmt.Errorf("database error")

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents/stats", nil)

	// Execute handler
	handlers.HandleGetAgentStats(c)

	// Assert response
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	assert.Contains(t, response["error"], "Failed to fetch agent statistics")
}

func TestHandleGetAgentStats_GroupsError(t *testing.T) {
	handlers, mockService := setupAgentHandlersTest()

	// Create test agent
	agent := testutils.MakeTestAgent(uuid.New())
	_ = mockService.CreateAgent(context.TODO(), agent)

	// Set error flag for groups only
	mockService.ListGroupsErr = fmt.Errorf("groups error")

	// Create test request
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/agents/stats", nil)

	// Execute handler
	handlers.HandleGetAgentStats(c)

	// Assert response - should succeed with groups count = 0
	assert.Equal(t, http.StatusOK, w.Code)

	var response GetAgentStatsResponse
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, 1, response.TotalAgents)
	assert.Equal(t, 0, response.GroupsCount)
}

// patchGroup is a small helper that drives HandleUpdateAgentGroup with a
// JSON body and the :id path param set, returning the recorder.
func patchGroup(t *testing.T, h *AgentHandlers, agentID, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: agentID}}
	c.Request = httptest.NewRequest("PATCH", "/api/v1/agents/"+agentID+"/group",
		bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleUpdateAgentGroup(c)
	return w
}

// TestHandleUpdateAgentGroup_Reassign: PATCH with a valid existing
// group re-points the agent and the new GroupID/GroupName persist.
func TestHandleUpdateAgentGroup_Reassign(t *testing.T) {
	handlers, mock := setupAgentHandlersTest()
	ctx := context.Background()

	agentID := uuid.New()
	require.NoError(t, mock.CreateAgent(ctx, &services.Agent{ID: agentID, Name: "a1"}))
	require.NoError(t, mock.CreateGroup(ctx, &services.Group{ID: "grp-1", Name: "prod"}))

	w := patchGroup(t, handlers, agentID.String(), `{"group_id":"grp-1"}`)
	require.Equal(t, http.StatusOK, w.Code)

	got, err := mock.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got.GroupID)
	assert.Equal(t, "grp-1", *got.GroupID)
	require.NotNil(t, got.GroupName)
	assert.Equal(t, "prod", *got.GroupName)
}

// TestHandleUpdateAgentGroup_Clear: a null group_id clears the
// assignment (un-assign).
func TestHandleUpdateAgentGroup_Clear(t *testing.T) {
	handlers, mock := setupAgentHandlersTest()
	ctx := context.Background()

	gid, gname := "grp-1", "prod"
	agentID := uuid.New()
	require.NoError(t, mock.CreateAgent(ctx, &services.Agent{
		ID: agentID, Name: "a1", GroupID: &gid, GroupName: &gname}))

	w := patchGroup(t, handlers, agentID.String(), `{"group_id":null}`)
	require.Equal(t, http.StatusOK, w.Code)

	got, err := mock.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Nil(t, got.GroupID)
	assert.Nil(t, got.GroupName)
}

// TestHandleUpdateAgentGroup_UnknownAgent: 404 when the agent doesn't exist.
func TestHandleUpdateAgentGroup_UnknownAgent(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()
	w := patchGroup(t, handlers, uuid.New().String(), `{"group_id":"grp-1"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestHandleUpdateAgentGroup_UnknownGroup: 400 when the target group
// doesn't exist (agent unchanged).
func TestHandleUpdateAgentGroup_UnknownGroup(t *testing.T) {
	handlers, mock := setupAgentHandlersTest()
	ctx := context.Background()

	agentID := uuid.New()
	require.NoError(t, mock.CreateAgent(ctx, &services.Agent{ID: agentID, Name: "a1"}))

	w := patchGroup(t, handlers, agentID.String(), `{"group_id":"does-not-exist"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	got, err := mock.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Nil(t, got.GroupID)
}

// TestHandleUpdateAgentGroup_BadID: 400 on a non-UUID agent id.
func TestHandleUpdateAgentGroup_BadID(t *testing.T) {
	handlers, _ := setupAgentHandlersTest()
	w := patchGroup(t, handlers, "not-a-uuid", `{"group_id":"grp-1"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestHandleUpdateAgentGroup_EmitsAuditOnChange: reassigning an agent to a
// different group records one agent.group_reassigned event carrying the
// from→to transition. Every other agent mutation already audits; this closes
// the gap for group reassignment.
func TestHandleUpdateAgentGroup_EmitsAuditOnChange(t *testing.T) {
	mock := testutils.NewMockAgentService()
	audit := &recordingAuditService{}
	h := NewAgentHandlers(mock, &mockConfigSender{}, zap.NewNop()).WithAuditService(audit)
	ctx := context.Background()

	agentID := uuid.New()
	require.NoError(t, mock.CreateAgent(ctx, &services.Agent{ID: agentID, Name: "a1"}))
	require.NoError(t, mock.CreateGroup(ctx, &services.Group{ID: "grp-1", Name: "prod"}))

	w := patchGroup(t, h, agentID.String(), `{"group_id":"grp-1"}`)
	require.Equal(t, http.StatusOK, w.Code)

	require.Len(t, audit.recorded, 1, "a group change must emit one audit event")
	got := audit.recorded[0]
	assert.Equal(t, services.AuditEventAgentGroupReassigned, got.EventType)
	assert.Equal(t, services.AuditTargetAgent, got.TargetType)
	assert.Equal(t, agentID.String(), got.TargetID)
	assert.Equal(t, "", got.Payload["from_group_id"], "agent started unassigned")
	assert.Equal(t, "grp-1", got.Payload["to_group_id"])
	assert.Equal(t, "prod", got.Payload["to_group_name"])
}

// TestHandleUpdateAgentGroup_NoOpReassignEmitsNoAudit: re-selecting an agent's
// CURRENT group (the Fleet dropdown re-picking the same value) returns 200 but
// records no audit row — transition-only emission, mirroring the exclusion /
// rollback-flag posture.
func TestHandleUpdateAgentGroup_NoOpReassignEmitsNoAudit(t *testing.T) {
	mock := testutils.NewMockAgentService()
	audit := &recordingAuditService{}
	h := NewAgentHandlers(mock, &mockConfigSender{}, zap.NewNop()).WithAuditService(audit)
	ctx := context.Background()

	gid, gname := "grp-1", "prod"
	agentID := uuid.New()
	require.NoError(t, mock.CreateAgent(ctx, &services.Agent{
		ID: agentID, Name: "a1", GroupID: &gid, GroupName: &gname}))
	require.NoError(t, mock.CreateGroup(ctx, &services.Group{ID: "grp-1", Name: "prod"}))

	w := patchGroup(t, h, agentID.String(), `{"group_id":"grp-1"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, audit.recorded, 0, "no-op reassignment to the same group must not audit")
}
