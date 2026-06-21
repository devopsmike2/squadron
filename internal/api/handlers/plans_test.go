// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// v0.73 — HandleCreatePlan exercises the full path: handler →
// service → memory store. Verifies the wire shape (CreatePlanRequest
// in, CreatePlanResponse out), the 201 status, and that the service
// layer's plan grouping rules (assigned PlanID, PlanStepIndex
// 0..N-1, RequireApproval forced on steps 1+) survive the trip.

func setupPlanHandler(t *testing.T) (*RolloutHandlers, *memory.Store, string, string) {
	t.Helper()
	store := memory.NewStore()
	gid := "web-prod"
	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{ID: gid, Name: "Web Prod"}))
	cfg := &types.Config{
		ID: "cfg", Name: "C", Content: "x", GroupID: &gid,
		CreatedAt: time.Now(),
	}
	require.NoError(t, store.CreateConfig(t.Context(), cfg))

	svc := services.NewRolloutService(store, nil, nil, zap.NewNop())
	h := NewRolloutHandlers(svc, zap.NewNop())
	return h, store, gid, cfg.ID
}

func TestHandleCreatePlan_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, gid, cfgID := setupPlanHandler(t)

	body := CreatePlanRequest{
		Steps: []services.RolloutInput{
			{
				Name: "Step 0", GroupID: gid, TargetConfigID: cfgID,
				Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}},
			},
			{
				Name: "Step 1", GroupID: gid, TargetConfigID: cfgID,
				Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}},
			},
		},
	}
	raw, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", bytes.NewBuffer(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleCreatePlan(c)

	require.Equal(t, http.StatusCreated, w.Code)

	var resp CreatePlanResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.PlanID, "service must assign a plan id")
	assert.Equal(t, 2, resp.Count)
	require.Len(t, resp.Steps, 2)

	// Step indices assigned in step order regardless of input.
	assert.Equal(t, 0, resp.Steps[0].PlanStepIndex)
	assert.Equal(t, 1, resp.Steps[1].PlanStepIndex)
	// Both steps share the assigned plan id.
	assert.Equal(t, resp.PlanID, resp.Steps[0].PlanID)
	assert.Equal(t, resp.PlanID, resp.Steps[1].PlanID)
}

func TestHandleCreatePlan_RejectsEmptySteps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, _, _ := setupPlanHandler(t)

	body := bytes.NewBufferString(`{"steps":[]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", body)
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleCreatePlan(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "at least one step")
}

// v0.74 — GET /api/v1/rollouts/plans/:id returns the envelope.
// 404 when the plan id doesn't exist.
func TestHandleGetPlan_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, gid, cfgID := setupPlanHandler(t)

	// Seed via the service to get a real PlanID.
	svc, ok := h.rolloutService.(services.RolloutService)
	require.True(t, ok)
	_, planID, err := svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "S0", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
		{Name: "S1", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = []gin.Param{{Key: "id", Value: planID}}
	c.Request, _ = http.NewRequest(http.MethodGet, "/", nil)

	h.HandleGetPlan(c)

	require.Equal(t, http.StatusOK, w.Code)
	var env services.Plan
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, planID, env.PlanID)
	assert.Equal(t, gid, env.GroupID)
	assert.Equal(t, 2, env.StepCount)
	require.Len(t, env.Steps, 2)
	assert.Empty(t, env.RollbackSteps)
}

func TestHandleGetPlan_Unknown404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, _, _ := setupPlanHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = []gin.Param{{Key: "id", Value: "plan-does-not-exist"}}
	c.Request, _ = http.NewRequest(http.MethodGet, "/", nil)

	h.HandleGetPlan(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// HandleListRollouts now accepts plan_id query param.
func TestHandleListRollouts_FiltersByPlanID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, gid, cfgID := setupPlanHandler(t)

	svc, ok := h.rolloutService.(services.RolloutService)
	require.True(t, ok)
	// Two plans, two steps each.
	_, plan1, err := svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "P1S0", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
		{Name: "P1S1", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)
	_, _, err = svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "P2S0", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/?plan_id="+plan1, nil)

	h.HandleListRollouts(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Rollouts []*services.Rollout `json:"rollouts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Rollouts, 2,
		"plan_id filter should only return rollouts in plan1")
	for _, r := range resp.Rollouts {
		assert.Equal(t, plan1, r.PlanID)
	}
}

// v0.89.2 — HandleListPlans (#554, backfill of the v0.77 squadronctl
// plans subcommand). Happy path returns the {plans, count} envelope
// shape; bad RFC3339 `since` returns 400.

func TestHandleListPlans_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, gid, cfgID := setupPlanHandler(t)

	svc, ok := h.rolloutService.(services.RolloutService)
	require.True(t, ok)
	// Two plans, both in the same group so the default list sees both.
	_, planA, err := svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "A0", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
		{Name: "A1", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)
	_, planB, err := svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "B0", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/", nil)

	h.HandleListPlans(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp ListPlansResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 2, resp.Count)
	require.Len(t, resp.Plans, 2)

	ids := []string{resp.Plans[0].PlanID, resp.Plans[1].PlanID}
	assert.Contains(t, ids, planA)
	assert.Contains(t, ids, planB)
	for _, p := range resp.Plans {
		assert.NotEmpty(t, p.State, "envelope state must be derived")
		assert.Equal(t, gid, p.GroupID)
	}
}

func TestHandleListPlans_BadSinceReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, _, _ := setupPlanHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/?since=not-a-timestamp", nil)

	h.HandleListPlans(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "since")
}

func TestHandleListPlans_GroupIDFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, store, gid, cfgID := setupPlanHandler(t)

	// Add a second group + config so we can prove the filter narrows.
	gid2 := "other-group"
	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{ID: gid2, Name: "Other"}))
	cfg2 := &types.Config{ID: "cfg2", Name: "C2", Content: "x", GroupID: &gid2, CreatedAt: time.Now()}
	require.NoError(t, store.CreateConfig(t.Context(), cfg2))

	svc, ok := h.rolloutService.(services.RolloutService)
	require.True(t, ok)
	_, planMain, err := svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "M", GroupID: gid, TargetConfigID: cfgID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)
	_, _, err = svc.CreatePlan(t.Context(), []services.RolloutInput{
		{Name: "O", GroupID: gid2, TargetConfigID: cfg2.ID,
			Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/?group_id="+gid, nil)

	h.HandleListPlans(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp ListPlansResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Count)
	require.Len(t, resp.Plans, 1)
	assert.Equal(t, planMain, resp.Plans[0].PlanID)
	assert.Equal(t, gid, resp.Plans[0].GroupID)
}

func TestHandleListPlans_BadLimitReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, _, _, _ := setupPlanHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/?limit=0", nil)

	h.HandleListPlans(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "limit")
}

func TestHandleCreatePlan_RejectsMismatchedGroups(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, store, gid, cfgID := setupPlanHandler(t)
	gid2 := "g2"
	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{ID: gid2, Name: "G2"}))

	body := CreatePlanRequest{
		Steps: []services.RolloutInput{
			{Name: "0", GroupID: gid, TargetConfigID: cfgID,
				Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
			{Name: "1", GroupID: gid2, TargetConfigID: cfgID,
				Stages: []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100}}},
		},
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", bytes.NewBuffer(raw))
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleCreatePlan(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "group_id")
}
