// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/testutils"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// setupGroupHandlersTest wires up a GroupHandlers against the same
// MockAgentService the agents handler tests use. The mockConfigSender
// is reused as-is — it implements AgentCommander and the group
// handlers only invoke it for restart fan-out which these tests
// don't exercise.
func setupGroupHandlersTest() (*GroupHandlers, *testutils.MockAgentService) {
	mockService := testutils.NewMockAgentService()
	mockSender := &mockConfigSender{}
	logger := zap.NewNop()
	handlers := NewGroupHandlers(mockService, mockSender, logger)
	return handlers, mockService
}

// TestHandleCreateGroup_LearnFromVerdicts_DefaultsTrue exercises
// the v0.89.18 (#634) plumbing: when the create payload omits
// learn_from_verdicts entirely, the handler must default to true to
// match the storage column default (NOT NULL DEFAULT 1). Without
// the *bool pointer + explicit default, the zero value would flip
// every new group to opted-out.
func TestHandleCreateGroup_LearnFromVerdicts_DefaultsTrue(t *testing.T) {
	handlers, mockService := setupGroupHandlersTest()

	body, err := json.Marshal(map[string]any{
		"name": "web-prod",
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/groups", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handlers.HandleCreateGroup(c)

	assert.Equal(t, http.StatusCreated, w.Code, "response body: %s", w.Body.String())

	var resp services.Group
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.LearnFromVerdicts,
		"omitting learn_from_verdicts should default to true; got false")

	// Round-trip: confirm the field actually landed in the service
	// layer, not just on the response envelope.
	stored, err := mockService.GetGroup(context.Background(), resp.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.LearnFromVerdicts,
		"service-layer Group should carry LearnFromVerdicts=true after omitted create")
}

// TestHandleCreateGroup_LearnFromVerdicts_ExplicitFalse covers the
// other branch: an operator who intentionally opts a new group
// out of the v0.89.17 (#633) feedback loop. Explicit false must
// survive the *bool dereference; without the pointer wrap the
// zero-value collision would swallow this.
func TestHandleCreateGroup_LearnFromVerdicts_ExplicitFalse(t *testing.T) {
	handlers, mockService := setupGroupHandlersTest()

	body, err := json.Marshal(map[string]any{
		"name":                "audit-only",
		"learn_from_verdicts": false,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/groups", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handlers.HandleCreateGroup(c)
	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp services.Group
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.LearnFromVerdicts,
		"explicit learn_from_verdicts=false must survive the create path")

	stored, err := mockService.GetGroup(context.Background(), resp.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.False(t, stored.LearnFromVerdicts,
		"service-layer Group should carry LearnFromVerdicts=false after explicit-false create")
}

// TestHandleUpdateGroup_LearnFromVerdicts_RoundTrip exercises the
// partial-update path. Seed a group with LearnFromVerdicts=true,
// PUT {learn_from_verdicts:false}, assert: (a) the field flipped,
// (b) Name is untouched (partial semantics), (c) the response
// payload reflects the new value.
//
// This is the test that proves the §9 line 4 "Flipped via the
// existing group settings handler" path actually works — a claim
// that was true in spec but only true in code after v0.89.18.
func TestHandleUpdateGroup_LearnFromVerdicts_RoundTrip(t *testing.T) {
	handlers, mockService := setupGroupHandlersTest()

	groupID := "grp_test_" + time.Now().Format("150405")
	seed := &services.Group{
		ID:                groupID,
		Name:              "web-prod",
		Labels:            map[string]string{"env": "prod"},
		LearnFromVerdicts: true,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	require.NoError(t, mockService.CreateGroup(context.Background(), seed))

	flipPayload, err := json.Marshal(map[string]any{
		"learn_from_verdicts": false,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/api/v1/groups/"+groupID, bytes.NewReader(flipPayload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: groupID}}

	handlers.HandleUpdateGroup(c)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp services.Group
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.False(t, resp.LearnFromVerdicts, "PUT should flip the flag to false")
	assert.Equal(t, "web-prod", resp.Name, "Name must be untouched by partial update")

	stored, err := mockService.GetGroup(context.Background(), groupID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.False(t, stored.LearnFromVerdicts,
		"service-layer Group should reflect the flipped flag after PUT")
}

// TestHandleUpdateGroup_LearnFromVerdicts_Omit_LeavesUnchanged is
// the partial-update twin: an UPDATE that omits learn_from_verdicts
// entirely must NOT touch the field. This is what makes the
// require_approval + learn_from_verdicts policies independent.
func TestHandleUpdateGroup_LearnFromVerdicts_Omit_LeavesUnchanged(t *testing.T) {
	handlers, mockService := setupGroupHandlersTest()

	groupID := "grp_omit_" + time.Now().Format("150405")
	seed := &services.Group{
		ID:                groupID,
		Name:              "web-prod",
		Labels:            map[string]string{"env": "prod"},
		LearnFromVerdicts: true,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	require.NoError(t, mockService.CreateGroup(context.Background(), seed))

	// PUT touches only require_approval — learn_from_verdicts MUST
	// remain at its prior value of true.
	body, err := json.Marshal(map[string]any{
		"require_approval": true,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/api/v1/groups/"+groupID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: groupID}}

	handlers.HandleUpdateGroup(c)
	assert.Equal(t, http.StatusOK, w.Code)

	stored, err := mockService.GetGroup(context.Background(), groupID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.LearnFromVerdicts,
		"omitted learn_from_verdicts in PUT must leave the prior value intact")
	assert.True(t, stored.RequireApproval,
		"require_approval should have flipped to true on this PUT")
}
